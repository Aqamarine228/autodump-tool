# Auto Dump Tool — Implementation Plan (Phase 1: M1–M3)

Source PRD: `.claude/prds/auto-dump.prd.md`. Planned by Fable 5.1 and reviewed by the main session. Fixes applied (marked **[FIX]**).

## 0. Decisions
- **Serialization**: raw `database/sql` via the xorm engine's `DB()`, scanning into `[]sql.RawBytes` over the text protocol. **Reject `xorm.DumpTables`** (`xorm@v1.3.9/engine.go:467-610`):
  - It builds DDL from reflected schema, so engine, charset, collation, defaults and comments are lost.
  - It writes one INSERT per row.
  - It strips control characters, which corrupts text and binary.
  - DDL comes from `SHOW CREATE TABLE` instead.
- **DSN built by the tool, not `xorm-models`.** Same env keys, but `parseTime` stays off: with it on, `time.Time` lands in RawBytes as RFC3339. DSN: `user:pass@tcp(host)/db?charset=utf8mb4&time_zone=%27%2B00%3A00%27&readTimeout=...`. xorm is the engine/connection wrapper; index queries go through it.
- **[FIX] Watermark** = max(highest `last_id` among `*.sql` in OUTPUT_DIR, value in `OUTPUT_DIR/last_id.txt`).
  - `last_id.txt` is rewritten atomically (temp file + rename) after each file commit.
  - This way, if DevOps moves old files off the disk, the tool doesn't re-export terabytes.
  - Files win if they are ahead, e.g. after a crash between the rename and the state write.
- **Parallelism is file-level.** `WORKERS` files are in flight at once. Each is written by one goroutine, streaming. A sequencer renames `.partial` → `.sql` strictly in order.
- **Log rotation**: `gopkg.in/natefinch/lumberjack.v2`, with `MaxSize=LOG_FILE_MB` and `MaxBackups=LOG_TOTAL_MB/LOG_FILE_MB-1`.
- **Config**: `github.com/joho/godotenv` + `os.Getenv` override.
- Stall-alert dedupe is in memory. One repeated alert after a process restart is accepted.

## 1. Layout (`module autodump`, Go 1.23, CGO_ENABLED=0)
```
cmd/autodump/main.go        flags: -once (run now, exit), -bench N, -check-config
internal/config             Config; Load(path) (Config, error); validation (R16)
internal/logx               New(cfg) *log.Logger (lumberjack); Redact(err) string
internal/alert              Notifier{ Failure(key, text); Stall(docID, text); StallResolved(docID, text) }
                            Telegram (sendMessage + pinChatMessage, net/http); Dedupe wrapper; Nop
internal/schedule           ParseTimes("02:00,14:00") ([]HHMM, error); Next(now, times) time.Time
internal/source             Index: Scan(ctx, from uint64) (ids []uint64, blocker *Blocker, err)
                            Docs: ShowCreate(ctx, id) (string, error); StreamRows(ctx, id, fn(cols, row []sql.RawBytes) error) error
internal/batch              Split(ids []uint64, size, merge int) [][]uint64
internal/dump               WriteFileHeader(w); WriteTable(w, name, ddl, rows, opts) (n int64, err); escape.go
internal/output             Sink{ Begin(name) (io.WriteCloser, error); Commit(name) error; Abort(name) error }
                            Dir: Watermark() (uint64, error); CleanPartials() (int, error); state file last_id.txt
internal/export             Runner: Run(ctx) (Summary, error); StallCheck(ctx) error; pipeline + sequencer
```
Phase 2 boundary: `output.Sink` and `alert.Notifier` are the only interfaces.

## 2. Algorithms
**Resume.** At the start of every run:
1. `CleanPartials()`, logging the count.
2. Parse `^(\d+)-(\d+)\.sql$` and read `last_id.txt`. Ignore malformed names, with a warning.
3. Start = `max(watermark+1, START_ID)`.

**[FIX] Selection uses ONE query.** The original two-query version (blocker query, then an unbounded ID query) had a race: a not-ready document inserted between the two queries would be exported.
```sql
SELECT id, (total_orders = 0 OR done = 1) AS ok,
       TIMESTAMPDIFF(HOUR, created_at, NOW()) AS age_h
FROM parsing_documents WHERE id >= ? ORDER BY id
```
- Stream the result.
- Collect IDs while `ok = 1`.
- The first `ok = 0` row is the blocker; stop reading and close the rows.
- Gaps don't matter, because only rows that exist are returned.
- `StallCheck` reuses the same scan, but stops at the blocker without collecting.
- Stall rule: `age_h >= STALL_HOURS` and the blocker isn't reported yet → `Stall()`. A reported ID that no longer blocks → `StallResolved()`.

**Batching.**
```go
for len(rest) > merge {
    out = append(out, rest[:size])
    rest = rest[size:]
}
if len(rest) > 0 {
    out = append(out, rest)
}
```
Defaults: size 100, merge 150. The PRD table is the unit test.

**Per-file content** (restores with `mariadb < file` into an empty DB):
```
SET NAMES utf8mb4; SET time_zone='+00:00'; SET FOREIGN_KEY_CHECKS=0; SET UNIQUE_CHECKS=0;
SET SQL_MODE='NO_AUTO_VALUE_ON_ZERO';
DROP TABLE IF EXISTS `document_N`;
<SHOW CREATE TABLE output>;
INSERT INTO `document_N` (`c1`,...) VALUES (...),(...);   -- flushed at INSERT_BATCH_ROWS / INSERT_BATCH_BYTES
/* autodump complete: K tables, R rows */
```

**Value formatting** (`dump/escape.go`, keyed on `ColumnType.DatabaseTypeName()`):
- NULL → `NULL`
- integer types, FLOAT, DOUBLE, DECIMAL, YEAR → raw bytes
- BIT, BINARY, VARBINARY, all BLOB types, GEOMETRY → `X'<hex>'`
- everything else → `'...'`, escaping `\0 \n \r \\ \' \" \x1a`

Query with `SELECT * FROM \`document_N\`` and no args, which forces the text protocol. Reuse the RawBytes slice across rows. Write through a `bufio.Writer` of `WRITE_BUFFER_MB`.

**Pipeline.**
1. Batches go onto a channel.
2. `WORKERS` goroutines each run: `Begin` → write tables → `Close` (flush + fsync) → `done{i}`.
3. The sequencer commits batch `i` only after `i-1` is committed. Each commit renames the file, then updates `last_id.txt`.
4. On any error: cancel, `Abort` every file in flight, and return the error. Later files that finished are deleted, never renamed.

**Missing table** (error 1146) for an exportable document: fail the run, alert key `missing_table:<id>`.

**Main loop.**
1. Wait until `schedule.Next`.
2. Run `StallCheck`.
3. Take the run lock with TryLock; if it's held, log the skip.
4. Run with `recover()`.

A panic in any goroutine becomes `Failure("panic", stack)`. The process exits only on invalid config at startup. Optionally send one "autodump started" message at boot, so DevOps can see restarts.

## 3. `.env` contract
| Key | Default | Note |
|---|---|---|
| `DATABASE_SQL_LOGIN/_SQL_PASSWORD/_SQL_HOST/_DATABASE` | required | index DB |
| `DATABASE_TABLE` | `parsing_documents` | index table; plain identifier only |
| `DOCUMENTS_SQL_LOGIN/_SQL_PASSWORD/_SQL_HOST/_DATABASE` | required | document tables |
| `OUTPUT_DIR` | required | writable probe at startup |
| `SCHEDULE_TIMES` | `02:00` | comma list `HH:mm`, local time |
| `RUN_ON_START` | `false` | |
| `START_ID` | `0` | |
| `BATCH_SIZE` / `MERGE_THRESHOLD` | `100` / `150` | validate merge >= size |
| `STALL_HOURS` | `24` | |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` | required | |
| `LOG_FILE` / `LOG_FILE_MB` / `LOG_TOTAL_MB` | `autodump.log` / `100` / `1024` | |
| `WORKERS` | `2` | files in flight |
| `INSERT_BATCH_ROWS` / `INSERT_BATCH_BYTES` | `1000` / `4194304` | < `max_allowed_packet` |
| `WRITE_BUFFER_MB` | `4` | |
| `DB_RETRY_ATTEMPTS` / `DB_RETRY_BASE_SEC` | `5` / `5` | exponential, cap 5 min |
| `TELEGRAM_RETRY_ATTEMPTS` | `3` | |

## 4. Error policy
| Class | Retry | Alert | Then |
|---|---|---|---|
| Invalid config | no | no (stderr, exit 2) | exit |
| DB connect, ErrBadConn, timeout, 1040/1053/2006/2013 | backoff | after retries run out, key `db:<conn>` | abort run |
| Non-transient SQL error | no | `sql:<doc>` | abort run |
| Missing table 1146 | no | `missing_table:<id>` | abort run |
| Write error, ENOSPC, rename failure | no | `io:<file>` | abort run |
| Panic | no | `panic` | recover, continue loop |
| Telegram send failure | 3x | log | continue |
| Pin failure | no | log | message already sent |
| Stall | — | once per document, pinned, plus a resolved message | keep waiting |
| Overlapping run | — | log | skip |

`Failure(key)` is sent at most once per run.

## 5. Tests
- **Unit**:
  - `batch.Split`: the PRD table, plus 0
  - `schedule.Next`: several times, wrap to the next day, Sunday, DST day
  - `escape`: NULL, `\0`, `'`, `\`, emoji, binary hex, `-0.10`, unsigned max
  - `output.Dir`: watermark from files, from the state file, both, junk names, START_ID, partial cleanup
  - config validation
  - Telegram via `httptest`, including a pin failure
  - dedupe
- **Integration** (`//go:build integration`, `TEST_DSN`, local MariaDB 11.3.2 or `mariadb:10.4` in Docker):
  - fixture `parsing_documents` plus 3–5 `document_N` tables with decimal/double NULLs, `tinyint(1)`, unicode varchar, text, datetime, blob, and one empty table
  - export → restore with the `mariadb` CLI → compare `CHECKSUM TABLE` and `SHOW CREATE TABLE`
  - blocker case (98 not ready, 99 ready → nothing exported)
  - gap case
  - missing table → error and alert
  - **[FIX]** race case: insert a not-ready row mid-scan → it's never exported
- **Fault**:
  - cancel mid-file → only `.partial` is left; a restart cleans it and resumes correctly
  - sequencer with WORKERS=4 where file 2 finishes before file 1
  - **[FIX]** delete old `.sql` files → resume still uses `last_id.txt`
- **Windows**: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/autodump`.

## 6. Tasks
- **Chunk A — foundations, mechanical**:
  - `go.mod`, `config`, `logx`, `schedule`, `batch`, `output`, tests first.
  - Acceptance: `go test ./internal/...` green and the Windows build compiles.
- **Chunk B — data path, needs care**:
  - `source` (DSN, one-query scan, StreamRows) and `dump` (escape tests first).
  - Integration harness and fixtures, plus the restore/checksum script.
  - Acceptance: 100% checksum match on the fixtures.
- **Chunk C — alerting + runner**:
  - `alert` (httptest tests first), `export` (sequencer tests first, then Runner and StallCheck), `cmd/autodump`.
  - Acceptance: fault tests pass; a local run produces correctly named files; a stall fixture triggers a pinned message on a real test bot.

Order: A and B run in parallel. C runs after them and integrates. M2 is the alert half of C. M3 is section 7.

## 7. Benchmark (M3)
`autodump -bench N` exports N representative documents. Outputs:
- GB/h and rows/s
- per-stage timing (query, serialize, write)
- a backlog ETA from `SUM(data_length+index_length)` for `database_documents`

Parameter grid:
- `WORKERS` {1,2,4}
- `INSERT_BATCH_ROWS` {500,1000,5000}
- `INSERT_BATCH_BYTES` {1,4,16} MB
- `WRITE_BUFFER_MB` {1,4,16}

Also measure the restore time per file.

## 8. Risks
- The real DDL of the document tables is unknown; it's inferred from the `Result` model. Run `SHOW CREATE TABLE` on prod in M1.
- `xormmodels.GetConnection` turns on `parseTime=True`, so the tool must not reuse it.
- Text round-trip of DOUBLE and FLOAT: the `CHECKSUM TABLE` test catches it. Fallback: `CAST(col AS CHAR)`.
- Prod `max_allowed_packet` is unknown (default 16 MB), so `INSERT_BATCH_BYTES` defaults to 4 MB.
- The initial backlog run can last days. Stall checks still run at every tick. A crash recomputes from the watermark.
- Timezones: the scheduler uses local Windows time, the stall check uses SQL `NOW()`, and files use `+00:00`.
