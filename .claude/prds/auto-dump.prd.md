# Auto Dump Tool

## Problem
The prctrl platform stores each parsing document as its own MariaDB table (`database_documents.document_{id}`). The document index is in `database.parsing_documents`. Nothing exports these tables into portable, restorable files automatically. Data runs to terabytes and the service is considered critical. Today a document that never gets archived, or an export that stops silently, would go unnoticed.

## Evidence
- The requester calls it a "critical service" and estimates "terabytes of records". The volume is an assumption until measured on production.
- Data contract verified in `~/GoLandProjects/prctrl-pm-server`:
  - index `database.parsing_documents`: `id`, `agent_id`, `done`, `total_orders`, `finished_orders`, `created_at`, `updated_at`
  - one table per document: `database_documents.document_{id}`
  - "ready" in existing code means `done = 1`
- Production server: MariaDB 10.4.26, from the IDE data source metadata. The tool runs on the same machine (`localhost`).
- Pain from doing this by hand or not at all: assumption, needs validation by asking the operator how documents are archived today.

## Users
- **Primary**: the prctrl platform operator (the requester). They need every finished document archived without manual work, and need to hear about it in Telegram when the archive stalls or fails.
- **Not for**: end users of prctrl dashboards; consumers of a live 30-day mirror (Phase 2, deferred).

## Hypothesis
We believe a **scheduled, sequential document exporter** will **archive every finished document into restorable SQL files with no manual work** for the **prctrl platform operator**.
We'll know we're right when **no document ID below the export position is ever skipped, sampled files restore exactly, and every stall or failure reaches the Telegram group.**

## Requirements

### Selection
- R1. Read the document list from `database.parsing_documents`. Read document tables from `database_documents.document_{id}`. These are two separate database connections.
- R2. **Exportable document**:
  - a non-empty document (`total_orders > 0`) needs `done = 1`
  - an empty document (`total_orders = 0`) is exportable as is. Empty documents are never filled later.
- R3. **No date window.** Export all documents, from the configured start position to the newest.
- R4. **Sequential order.** Export in ascending ID order:
  - If the next document is not yet exportable, stop there and wait, even if later IDs are ready.
  - IDs missing from the index (gaps) do not block.
  - Example: 98 not ready, 99 ready → wait for 98, then export 98 and 99.
- R5. A finished document never changes after `done = 1`, so each document is exported exactly once.
- R6. Documents from different projects (`agent_id`) can share a file.

### Batching and files
- R7. Batching within one run:
  - While more than 150 documents remain, cut a file of 100.
  - When 150 or fewer remain, they all go into one final file.
  - A file holds at most 150 tables. A run with more than 150 documents ends with a file of 51–150.

  | Exportable in run | Files |
  |---|---|
  | 1 | 1 |
  | 100 | 100 |
  | 150 | 150 |
  | 151 | 100 + 51 (e.g. `1-100`, `101-151`) |
  | 250 | 100 + 150 |
  | 251 | 100 + 100 + 51 |
- R8. A run with only one exportable document produces one file. Never wait for a batch to fill.
- R9. Filename = actual first and last document ID in the file: `{first_id}-{last_id}.sql`, e.g. `100001-100100.sql`.
- R10. Every file restores with the standard MariaDB client into an empty database. It recreates each `document_{id}` table: same columns, types, keys, NULLs and values.
- R11. A crash or kill never leaves a file that looks complete but isn't. A restart resumes from the last fully written file, without duplicates or gaps.

### Alerts (Telegram Bot API)
- R12. **Stall alert.** If the blocking document stays unresolved past `STALL_HOURS` (default 24), send a message to the configured Telegram group and pin it.
- R13. **Failure alert.** Any failure that stops or breaks exporting goes to the same group. This covers the DB being unreachable, a missing document table, the disk being full, a write error, or a crash.
- R14. Alerts must not spam: one alert per distinct problem (see OQ3).

### Configuration (.env)
- R15. Everything is set in `.env`:
  - DB connections, reusing the existing `{NAME}_SQL_LOGIN/_SQL_PASSWORD/_SQL_HOST/_DATABASE` convention
  - output folder
  - run times: one or more `HH:mm`
  - batch size and merge threshold
  - stall hours
  - Telegram bot token and chat ID
  - log size cap
  - start position
- R16. Invalid config (bad `HH:mm`, missing secret, unwritable folder) stops at startup with a clear message. Secrets never appear in logs or Telegram.

### Runtime and operations
- R17. Compiles to a single Windows `.exe` from a Go project using xorm. No external dump binary needed (no `mysqldump.exe`).
- R18. Runs unattended for years on an always-on Windows machine. It doesn't stop without a reason: transient DB or network errors are retried, and an unrecoverable error is alerted, never silent.
- R19. Runs at one or more `HH:mm` times per day, listed in `.env` (e.g. `02:00,14:00`). Every run also checks for stalls. If a run is still going at the next scheduled time, the new one is skipped.
- R19a. The tool runs on the same machine as MariaDB and reads it over `localhost` at full speed. No throttling is required.
- R20. **Logging.** One plain `.txt` log, rotated by size, capped at 1 GB in total. Log run summaries, counts, warnings and errors; no row data and no per-row noise.
- R21. **No new database tables** of any kind (no state, no log tables). Source databases are read only.
- R22. **Speed.** There are terabytes of records, so throughput is a first-class requirement. Exporting must stream and must not be limited by the tool itself. Target set from a benchmark (M3).

## Success Metrics
| Metric | Target | How measured |
|---|---|---|
| Skipped documents | 0 | Compare exportable IDs ≤ last exported ID against the IDs in the files |
| Restore fidelity | 100% of sampled files | Restore sample files into a scratch DB and compare against the source tables |
| Stall alert latency | ≤ `STALL_HOURS` + 1 check interval | Fixture document held not-ready; time to Telegram message |
| Silent failures | 0 | Every failure in the log has a matching Telegram alert |
| Throughput | TBD — needs validation via benchmark on production-like data (GB/h) | Timed export of a representative slice |
| Backlog completion | TBD — needs the measured data size and the operator's deadline | Throughput × total size |

## Scope
**MVP**: R1–R22. Sequential export of all exportable documents into SQL files of 100 tables (max 150), named by ID range, Telegram stall and failure alerts, `.env` config, size-capped txt log, a Windows `.exe` that is tested locally.

**Out of scope**
- Phase 2: copy to a third server with 30-day retention. Deferred; the requester called it "room for extension". Notes are kept below.
- Deleting local files, or any date-based retention, in Phase 1. Phase 1 exports everything.
- Database state or log tables. The requester ruled these out.
- Writing to or deleting from source databases.
- Windows service wrapper, installer, and restart after reboot. DevOps runs the `.exe`.
- Compression. Plain `.sql`; disk is sized for it.
- Web UI, multi-host runs, encryption.

## Delivery Milestones
| # | Milestone | Outcome | Status | Plan |
|---|---|---|---|---|
| 1 | Sequential export | `.exe` exports ready documents in ID order into correctly sized, correctly named, restorable files; resumes after restart | complete | .claude/plans/auto-dump-phase1.plan.md |
| 2 | Telegram alerting | Stalls past `STALL_HOURS` and failures reach the group; stall messages are pinned | complete | .claude/plans/auto-dump-phase1.plan.md |
| 3 | Throughput at scale | Benchmarked on production-like data; meets the agreed GB/h target; backlog has a completion date | in-progress | .claude/plans/auto-dump-phase1.plan.md |
| 4 | Phase 2: 30-day mirror (deferred) | Third server holds the last 30 days; older documents and their files are removed | pending | — |

## Phase 2 notes (deferred, not requirements yet)
- Copy recent documents to a third server. Each run removes documents older than 30 days (rolling from run time, by `parsing_documents.created_at`) and adds new ones.
- Delete a file only when every document in it is older than 30 days. A file containing any document younger than 30 days stays.
- Open: does the third server hold restored tables, files, or both?

## Decisions (closed 2026-10-13)
- OQ1 Empty documents (`total_orders = 0`) are never filled later. Exported as is.
- OQ6 Stall checks run at every scheduled time. Several `HH:mm` times are allowed in `.env`.
- OQ7 The disk is sized for terabytes. Plain `.sql`, no compression.
- OQ8 The tool runs on the MariaDB machine and reads `localhost` at full speed.
- Restart after reboot and process supervision belong to DevOps. The `.exe` is the interface.

## Open Questions (defaults apply unless changed)
- [ ] OQ2. **Stall clock.** Measured from the blocking document's `created_at`. This survives restarts and needs no state.
- [ ] OQ3. **Alert repeats.** One pinned alert per stuck document, plus a "resolved" message when it clears. Failure alerts are deduplicated, so the same error is reported at most once per run.
- [ ] OQ4. **After a stall alert:** keep waiting. No skip list.
- [ ] OQ5. **Start position.** `START_ID` in `.env`, default 0 (the lowest ID).
- [ ] OQ9. **Telegram.** One group for everything. If pinning fails (the bot is not an admin), the message is still sent and the pin failure is logged.
- [ ] OQ10. **Restore target.** Files create bare `document_{id}` tables (`DROP TABLE IF EXISTS` + `CREATE TABLE`), with no database prefix.

## Risks
| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| One document never finishes and blocks all later exports | Medium | High | Stall alert + pin (R12) |
| Full-speed export loads the shared MariaDB machine | Medium | Medium | Accepted by the requester; workers configurable in `.env` |
| Disk fills up | Low | High | Failure alert (R13) |
| Pure-Go export changes values (float precision, NULL, Unicode) | Medium | High | R10 restore comparison on real tables in M1 |
| DB timezone vs Go time handling shifts the stall clock | Medium | Low | Do time comparisons in SQL |
| Telegram unreachable → alert lost | Low | Medium | Always write the alert to the log; retry sending |

## Handoff note
The requester asked for Fable 5.1 to do planning (`/plan`) and Opus 5.5 to do the implementation. Reference material: the original draft `Auto_Dump_PRD.md` (design research, edge cases A01–A22) and `~/GoLandProjects/prctrl-pm-server` (models, `.env` convention). Don't reuse the `NextTimeDayily` helper in sopostavlenia-server as is: it skips Sundays.

---
*Status: DRAFT — requirements only. Implementation planning pending via /plan.*
