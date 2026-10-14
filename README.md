# autodump

Exports finished prctrl documents (`database_documents.document_{id}`) into restorable SQL files, strictly in ID order. Problems are reported to Telegram.

## Build

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o bin/autodump.exe ./cmd/autodump
```

## Run

Put `.env` next to `autodump.exe` (or pass `-env path`). Start with `.env.example`; real OS environment variables override `.env`.

| Command | What it does |
|---|---|
| `autodump.exe` | Service mode: runs at every `SCHEDULE_TIMES` entry, forever |
| `autodump.exe -once` | One export (plus mirror sync if enabled) now, then exit (exit code 1 on failure) |
| `autodump.exe -check-config` | Validate `.env` and exit (exit code 2 on errors) |
| `autodump.exe -bench 200 -out D:\bench` | Export up to 200 documents into a scratch folder and log MB/s and GB/h (no Telegram) |

The process exits only on a configuration error. Everything else is logged, sent to Telegram, and retried at the next scheduled time. Restarting the `.exe` after a reboot is up to the host (DevOps).

## How it works

- **Ready documents.** The index table is `DATABASE_TABLE` in `DATABASE_DATABASE` (default `parsing_documents`). A document is ready when `done = 1` or `total_orders = 0`. Documents are exported in ascending ID order. The first document that is not ready stops the run, and nothing after it is exported until it becomes ready.
- **Stall alerts.** If that stuck document is older than `STALL_HOURS` (by `created_at`), a pinned Telegram alert is sent once. When it becomes ready, a "ready now" message follows.
- **File sizes.** Files hold 100 documents each. When 150 or fewer remain in a run, they all go into one last file. Names are `{first_id}-{last_id}.sql`.
- **Restoring.** Each file restores with `mysql --default-character-set=utf8mb4 targetdb < file.sql`. It recreates the bare `document_{id}` tables using `DROP TABLE IF EXISTS` + `CREATE TABLE` + multi-row `INSERT`. The restoring server's `max_allowed_packet` must be at least `INSERT_BATCH_BYTES` (default 4 MB).
- **Crash safety.** Files are written as `*.partial` and renamed only when complete. On restart, leftover `.partial` files are deleted and the export resumes after the highest ID found in the file names or in `last_id.txt`. Keep `last_id.txt` if you move old `.sql` files away.
- **Mirror (Phase 2, `MIRROR_ENABLED=true`).** After every export run, the third server is synced to "all exported documents created in the last `RETENTION_DAYS` days" (rolling, by the index `created_at`, using the index server's clock):
  - Missing `document_{id}` tables are copied from the source into `MIRROR_DATABASE`, then tables of documents older than the window are dropped. Each table is filled under a `document_{id}__part` name and renamed when complete, so a crash never leaves a half-filled table behind; leftovers are cleaned on the next run.
  - With `MIRROR_FILES_DIR` set (e.g. a share `\\host\autodump`), each exported `.sql` file that holds at least one document in the window is copied there. A file is deleted only when every document in it is older than the window. Leave it empty to mirror tables only.
  - Only what the index proves old is removed. Tables or files of documents that are missing from the index, or have no `created_at`, are left alone. Tables that are not `document_{id}` are never touched.
  - `MIRROR_DATABASE` must be a dedicated database (startup refuses the index or documents database), and `MIRROR_FILES_DIR` must differ from `OUTPUT_DIR`. The mirror server's `max_allowed_packet` must be at least `INSERT_BATCH_BYTES`.
  - A mirror failure is alerted separately and does not affect the export. The next run continues where it stopped.
- **Log.** `LOG_FILE` is rotated at `LOG_FILE_MB`, with at most `LOG_TOTAL_MB` in total (default 1 GB).

## Test

```sh
go test ./...                                          # unit tests
docker run -d --name autodump-mariadb -e MARIADB_ROOT_PASSWORD=autodump -p 127.0.0.1:3310:3306 mariadb:10.4
go test -tags integration ./tests/integration/...      # export -> restore -> checksum
python3 tests/e2e/fixture.py 420 300 397 30 | docker exec -i autodump-mariadb mysql --default-character-set=utf8mb4 -uroot -pautodump
OUTPUT_DIR=./out ./autodump -once && tests/e2e/verify.sh ./out
```
