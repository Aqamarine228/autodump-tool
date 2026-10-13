# Auto Dump Tool Product Requirements

**Version:** 0.1  
**Date:** 13 October 2026  
**Status:** Draft for requirements review  
**Implementation language:** Go  
**Database access:** xorm with the MySQL driver  
**Readers:** Product owner, Go developer, and database operator

## 1 Purpose and proposed outcome

Build a scheduled tool that reads a MySQL or MariaDB document index, identifies completed documents, and exports their database objects into restorable SQL files. Phase 1 delivers the local export. Phase 2 adds synchronization to a third server and removes expired, tool-managed documents there.

The proposed interpretation is that one completed document corresponds to one table in a separate document database. The notes say both “100 documents” and “100 tables,” so this interpretation is plausible but requires confirmation. If each document is actually a complete database or a collection of tables, selection, batching, restoration, and cleanup must be revised before implementation.

The proposed batching rule is 100 documents per file, with a final remainder of 1–49 merged into the preceding batch. This allows the last file to contain up to 149 documents. The proposed remote window is the preceding 30 × 24 hours, based on an authoritative document timestamp. These are proposals, not settled requirements: strict groups of 100 and a cutoff on the 12th of the previous month imply different behavior.

The next decision is to answer the blocking questions in Section 13, then freeze the schema mapping, batch policy, date policy, and destination contract. This deliverable specifies the application; it does not contain or deploy its production implementation.

## 2 Requirements captured from the notes

| Ref | Requirement from the notes | Draft interpretation |
| --- | --- | --- |
| U01 | Check another MySQL or MariaDB table for available documents | A document index is the authoritative registry |
| U02 | Export only ready documents | An explicit completion status controls eligibility |
| U03 | One file contains 100 tables | One table per document and a target batch size of 100 |
| U04 | If fewer than 50 remain, put them into the last file | Merge a tail of 1–49; the last file may exceed 100 |
| U05 | Use a name such as 100001-100100 | A document ID range is part of the filename |
| U06 | If only one document exists, make one file | Never wait for a batch to fill |
| U07 | Configure schedule and output through .env; time is hh:mm | A daily time, timezone, and output directory are configurable |
| U08 | Take documents for a configurable number of days | Selection supports a date window; its timestamp must be named |
| U09 | Phase 2 uses a third server; add new documents and remove old tables | Proposed destination is a database mirror with tool-owned objects |
| U10 | Retain the latest 30 days; also mentioned the 12th of last month | Conflicting date rules need a business decision |
| U11 | Use Go and xorm | Required application stack |
| U12 | Fable 5.1 for planning and Opus 5.5 for implementation | Requested agent routing for the eventual implementation handoff |

**Confirmed scope:** two implementation phases, Go, xorm, readiness filtering, scheduled operation, configurable output, grouped exports, and a remote age-based cleanup requirement.

**Unconfirmed details:** actual schemas, document-to-object mapping, filename semantics, remainder behavior, local full versus incremental exports, the date column, exact cutoff, remote database versus file storage, and whether completed documents can change.

Requested model names are preserved exactly. Model availability and their identifiers must be checked in the implementation environment; no named-model execution is claimed by this PRD.

## 3 System roles and data contract

### 3 1 Database and server roles

| Role | Purpose | Application access |
| --- | --- | --- |
| A Index database | Document ID, readiness, timestamp, object mapping, optional version | Read only |
| B Document database | The actual document tables to export | Read only |
| Runner | Go process, output directory, durable run state | Own files and state only |
| C Third server | Proposed MySQL or MariaDB mirror for recent documents | Create, read, and remove owned objects in a dedicated destination schema |

A and B are logical roles and may share a physical server. The runner may be colocated with either role. The requirement for a “third server” does not by itself determine where the runner lives or how data reaches C.

Phase 1 must not update readiness flags, delete source tables, or create application state tables in A or B. Phase 2 expiration applies to C. Local archive retention is a separate policy and is not implied by remote cleanup.

### 3 2 Required index mapping

The implementation needs the following logical fields. These are names in the tool contract, not assertions about the existing SQL schema.

| Logical field | Required behavior |
| --- | --- |
| document_id | Stable, unique document identity within a configured namespace |
| ready_status | Exact approved status value or completion predicate |
| document_date | Authoritative business timestamp for date selection and expiration |
| ready_at | Time the document became complete, when available |
| source_schema | Allowlisted document database or configured constant |
| source_table | Exact table name or a confirmed deterministic ID-to-table rule |
| content_version | Revision token if completed documents can change |
| updated_at | Useful for change detection; not a substitute for a reliable version contract |

Preserve IDs without floating-point conversion. Confirm whether they are integers, strings, or composite IDs. Include a namespace when IDs can repeat across tenants or schemas. Never derive object identity or age from a filename, table creation time, or filesystem modification time.

The index must contain one authoritative record per document, or define how duplicates and revisions are resolved. Recommend indexes supporting readiness and date selection, and a stable unique document key. Exact SQL indexes depend on the actual table and query plan.

### 3 3 Readiness and consistency

The preferred contract is that a document becomes ready only after its table and data are fully committed, and becomes immutable afterward. If A and B are separate servers, the producer must enforce that ordering; this tool cannot create an atomic transaction across both servers.

At run start, capture a single UTC time T and freeze an authoritative index selection using a consistent index snapshot or an approved completion marker. Persist the selected document list or a durable cursor tied to that snapshot. A cursor based only on IDs is not a snapshot. Work entering eligibility after the selection boundary belongs to a later run. For mutable documents, capture a version, validate it after export, and reject or retry an export whose version changed. A version must reflect payload changes, not just index edits.

Before exporting, check the source object exists and belongs to the configured allowlist. A missing table, unsupported object, or incomplete document is an explicit error. It must not become a successful empty export.

## 4 Phase 1 local document export

### 4 1 Selection and reporting

The tool must report separate counts: all ready documents in the index, documents eligible for the current date window, documents needing export, and documents successfully exported. Do not describe the total row count inside the payload tables as the document count.

Use the same readiness and date predicate for counting and enumeration. Within a stable selection snapshot, the selected list length is the final work count. SQL COUNT or xorm Count can count index records; deduplicate first if the index contains multiple rows per document. Do not use INFORMATION_SCHEMA.TABLES.TABLE_ROWS as an exact count [R01].

Recommend an initial export of all eligible ready documents, followed by incremental exports of previously undelivered document versions. Keep the local date window independently configurable: an unbounded archive and a rolling 30-day mirror are different products. Full exports on every run remain a business option until Q05 is answered.

A document that was previously unfinished must be discovered after becoming ready, even if its ID is lower than the last exported ID. A maximum-ID watermark alone is insufficient. Selection must also discover a qualifying revision of a previously exported document.

Sort documents deterministically, proposed by namespace and numeric ID when IDs are numeric. When several source schemas are allowed, define whether a batch can cross schemas. Prefer one namespace and source schema per batch to keep filenames and restoration unambiguous.

### 4 2 Export behavior

Export table definitions and all document table rows unless a narrower contract is explicitly approved. Preserve columns, types, keys, indexes, character sets, and relevant table options. Generated columns and binary values need restoration fixtures. Views, triggers, routines, events, and cross-table dependencies are conditional scope requiring an explicit decision.

Stream output instead of buffering whole tables or the whole batch in memory. For transactional sources, use a suitable consistent-read export strategy. A snapshot does not protect against concurrent schema changes, and nontransactional tables need a separate consistency policy [R02, R03]. Consistency is per document or batch as agreed; multiple native dump invocations do not provide one common snapshot for the entire run.

The default failure policy proposed here is to fail the affected batch while allowing unrelated batches to finish. The overall run becomes partial or failed. Retry the whole failed batch rather than publishing a file containing an unknown subset of its declared documents.

### 4 3 Durable publication

Create every run under a unique directory inside OUTPUT_DIR. Write the SQL artifact to a temporary file ending in .partial on the same filesystem as its final path. Finish the export, flush and close it, verify successful completion, compute a SHA-256 checksum, and atomically rename it to the final filename. Where supported, synchronize the file and containing directory for crash durability.

Persist a manifest containing the run identity, document IDs and versions, source objects, source timestamps, actual count, export backend, checksum, and final filename. A batch becomes committed only after both its artifact and manifest are durable. Advance delivery receipts last. On restart, reconcile an artifact published before its manifest or receipt was committed; do not silently regard it as delivered.

Zero eligible documents produces a successful no-op summary and no SQL file. A single eligible document produces one SQL file. A zero-row document table can still be a valid document: include its DDL and record zero rows if row verification is enabled.

## 5 Batching and filenames

### 5 1 Proposed tail merge algorithm

After grouping by any approved namespace or schema boundary, divide the selected ordered documents into consecutive groups of 100. If the final group contains 1–49 documents and a preceding group exists, merge it into that preceding group. A remainder of exactly 50 stays in a separate file. With no preceding group, publish the partial group as its own file.

| Selected documents | Tables in each SQL file |
| --- | --- |
| 0 | No SQL files |
| 1 | 1 |
| 49 | 49 |
| 50 | 50 |
| 99 | 99 |
| 100 | 100 |
| 101 | 101 |
| 149 | 149 |
| 150 | 100 and 50 |
| 199 | 100 and 99 |
| 200 | 100 and 100 |
| 201 | 100 and 101 |
| 249 | 100 and 149 |
| 250 | 100 and 100 and 50 |

These examples apply to one grouping scope. They expose the conflict in the original notes: a strict maximum of 100 cannot coexist with this interpretation of tail merging. If 100 is a hard cap, select another algorithm and replace this table before implementation.

Proposed merging is within the current run only. Do not reopen a file from a previous successful run. If “put into the last file” means append across runs, specify how to rewrite its filename, checksum, manifest, receipts, and any remote copy atomically. Plain append is insufficient for that behavior.

### 5 2 Proposed naming contract

Use a configurable filename template. A simple proposed template is `{first_id}-{last_id}.sql`, inside a unique run directory. A full path might be `OUTPUT_DIR/run_20261013T020000Z_<run_id>/100001-100100.sql`.

The range represents the actual minimum and maximum document IDs in the batch; the manifest records the exact members. Sparse IDs do not imply missing exports. For example, IDs 100001, 100005, and 100099 can produce `100001-100099.sql` with three documents.

After merging documents 100001 through 100101, the proposed filename is `100001-100101.sql`. The user's example `100001-100100` might instead mean a fixed ID bucket label. Fixed bucket labels, actual ID ranges, sequence numbers, and document counts are different naming schemes; Q08 must resolve this.

Reject path separators, parent directory traversal, and unsupported placeholders in filenames. Handle leading zeros according to the ID contract. Detect collisions before export and never overwrite a committed artifact from another run. A byte-size limit may conflict with the table-count policy; oversized-document handling must be agreed separately.

## 6 Phase 2 recent document mirror on the third server

### 6 1 Destination contract

The working proposal is to restore recent document tables into a dedicated MySQL or MariaDB schema on C, add newly eligible documents, and drop expired tables owned by the tool. The notes about deleting tables support this interpretation. If C is only a file archive, define transfer and expiration of files instead; that alternative requires a different cleanup contract.

Phase 2 reuses the document selection and export interfaces from Phase 1, with its own delivery receipts. A successful local export must not falsely mark a document as delivered to C. The third server can retry an existing verified artifact or export a new one when needed; its age filter is evaluated at document level.

### 6 2 Window definition requiring confirmation

For the proposed rolling policy, capture T once at the start of a run:

```text
cutoff = T - (RETENTION_DAYS * 24 hours)
eligible = ready AND document_date >= cutoff AND document_date < T
expired = managed_on_C AND document_date < cutoff
```

With RETENTION_DAYS=30 and T equal to 2026-10-13T02:00:00Z, the lower boundary is 2026-09-13T02:00:00Z. A document exactly at the lower boundary is retained. Future-dated and null-dated records are not imported and require an explicit diagnostic; unknown age never authorizes cleanup.

The alternative “from the 12th of the previous month” gives a lower date of 12 September for an October run. It is a calendar rule, not a fixed 30-day duration. Another possibility is that the 12th was simply an example for a run on 12 October. Q11 must choose one interpretation, timezone, inclusivity, and handling of daylight saving transitions.

Apply the window to the document's authoritative index timestamp and copy or remove the entire document table. Do not filter individual rows within that table unless row-level retention is explicitly required. An old document completed today may remain ineligible when its business date is outside the window; using ready_at would produce different behavior.

Between scheduled runs, the mirror can contain documents that have just aged out. Its maximum normal retention lag is approximately one schedule interval plus run duration. Strict real-time expiration requires an additional cleanup cadence and is not achieved by a daily job alone.

### 6 3 Synchronization sequence

1. Validate configuration, destination identity, credentials, and supported database versions. Acquire exclusive run ownership and persist T and the cutoff.
2. Read a complete, successful source selection for the window. Inspect C's managed document catalog and calculate imports, refreshes, unchanged documents, and expiration candidates.
3. Export or reuse verified artifacts for documents requiring delivery. Revalidate document versions or the immutability contract before publication.
4. Restore into destination staging objects. Do not publish incomplete tables. Validate the schema and agreed data checks, then promote one complete document at a time.
5. Persist delivery receipts only after verified publication. Keep an operation journal so a crash between DDL and receipt creation can be reconciled.
6. After all required deliveries succeed, refresh authoritative metadata for cleanup candidates and remove eligible expired, tool-owned tables. Log each cleanup result and finalize the run.

For a run with no active documents, successful empty selection is still authoritative. It can expire old managed tables if all cleanup preconditions pass. A failed query or interrupted enumeration must never be interpreted as an authoritative empty set.

Recommended cleanup is suppressed when required imports fail, selection is incomplete, metadata cannot be verified, run ownership is lost, or destination identity is wrong. This favors recoverability during outages but temporarily extends retention; a hard deletion deadline would require a different failure policy.

### 6 4 Target ownership and recovery

Maintain a tool-owned catalog on C using xorm. Record namespace, document ID, content version, exact target object names, authoritative document date, source identity, last verified checksum, publication state, and operation intent. The catalog itself and staging/control objects are excluded from document expiration.

A preexisting table without a matching ownership record is a conflict, not permission to adopt or delete it. A table prefix alone is insufficient evidence of ownership. Source and destination must be different logical databases, and target credentials must not permit cleanup of source objects.

Cleanup must operate on a finite allowlisted candidate list. Recheck candidates' current authoritative date and identity before dropping them; a stale catalog entry is insufficient when metadata can change. Unknown objects, missing ownership, unknown age, and unresolved dependencies block that candidate. Do not drop an entire schema as a shortcut.

Record cleanup intent before DDL, then confirm the object is absent and mark the operation complete. If the process crashes after a successful drop, restart reconciliation can complete the catalog update idempotently. Missing index records or revoked readiness need a separate policy; age-based expiration alone does not define what they mean.

CREATE, DROP, and RENAME operations must not be presented as one rollbackable xorm transaction. MariaDB documents implicit commits for these statements [R04]. Per-document staging, recoverable operation records, and tested promotion behavior are required. Foreign keys, triggers, and engine/version differences may constrain renaming.

The mirror is not an atomic replacement of the entire 30-day set. Documents become visible individually, and cleanup follows delivery. If consumers require a globally atomic view, specify a generation-based catalog or another publication design before Phase 2.

## 7 Configuration and scheduling

The following is a proposed .env contract. Names, paths, status values, and schema mappings are illustrative. Do not connect to production using guessed values.

```dotenv
# Daily schedule and runner behavior
SCHEDULE_TIME=02:00
SCHEDULE_TIMEZONE=UTC
RUN_ON_START=false
MISSED_SCHEDULE_POLICY=skip

# Source connections supplied through deployment secrets
INDEX_DSN=<read_only_index_mysql_dsn>
DOCUMENT_DSN=<read_only_document_mysql_dsn>
INDEX_TABLE=<document_index_table>
INDEX_ID_COLUMN=<document_id_column>
INDEX_STATUS_COLUMN=<ready_status_column>
INDEX_DATE_COLUMN=<document_date_column>
INDEX_READY_VALUE=<approved_ready_value>

# Phase 1 archive
OUTPUT_DIR=/var/lib/auto-dump/exports
STATE_DIR=/var/lib/auto-dump/state
LOCAL_EXPORT_MODE=incremental
LOCAL_LOOKBACK_DAYS=0
BATCH_SIZE=100
TAIL_MERGE_THRESHOLD=50
FILE_NAME_TEMPLATE={first_id}-{last_id}.sql
COMPRESSION=none
DUMP_BACKEND=native
DUMP_BINARY=/usr/bin/mariadb-dump
NATIVE_DEFAULTS_FILE=/run/secrets/auto-dump-client.cnf

# Phase 2 mirror
PHASE2_ENABLED=false
TARGET_DSN=<dedicated_destination_mysql_dsn>
TARGET_SCHEMA=<dedicated_mirror_schema>
RETENTION_MODE=rolling_hours
RETENTION_DAYS=30
CLEANUP_ENABLED=false

# Operational bounds subject to sizing
WORKERS=1
RETRY_ATTEMPTS=3
QUERY_TIMEOUT=60s
RUN_TIMEOUT=2h
LOG_LEVEL=info
```

LOCAL_LOOKBACK_DAYS=0 means no lower date boundary for local archiving. RETENTION_DAYS defines both eligibility and expiration for the proposed remote rolling window. The date field and its timezone interpretation must be agreed before either filter is implemented.

OS environment variables should override an optional .env file. Configuration is validated once at startup and captured per run; changes require a restart in the first release. Reject invalid times, unknown timezones, nonpositive retention, unsupported modes, invalid identifiers, missing required secrets, and unwritable paths. Do not log complete DSNs.

Parse SCHEDULE_TIME strictly as 24-hour HH:mm. Proposed scheduling is once daily in SCHEDULE_TIMEZONE. With a DST-observing timezone, skip a nonexistent local occurrence and log it; execute at most once for a repeated local date and scheduled time. Confirm this policy in Q31. The scheduler library's timezone and overlap features help implement it but do not supply distributed run ownership [R08].

One run may be active per job scope. A second invocation is skipped with a recorded reason. For the first release, use a process-level guard plus a host lock and a single-instance deployment. If multiple hosts may run the same job, require a shared lock with fencing or an equivalent single-owner deployment contract.

The first Phase 2 rollout runs with cleanup disabled to inspect the proposed candidate set and verify ownership. Normal scheduled cleanup is enabled through the agreed deployment configuration; it does not require an interactive prompt on each run.

## 8 Go and xorm implementation design

### 8 1 Responsibilities

| Component | Responsibility |
| --- | --- |
| Config loader | Parse .env and environment overrides; validate mappings and bounds |
| Scheduler | Trigger daily runs and prevent overlaps |
| Index repository | Use xorm to select, count, and map ready documents |
| Batch planner | Apply deterministic grouping, tail merging, and filename rules |
| Export adapter | Produce restorable DDL and data for the exact selected objects |
| Artifact store | Publish files, checksums, and manifests durably |
| State store | Persist run journals and per-destination document receipts |
| Target repository | Use xorm for the owned catalog, verification, and scoped target operations |
| Synchronizer | Compare desired and owned documents; stage and publish deliveries |
| Retention planner | Produce and execute a verified expiration candidate list |

Use separate xorm engines for A, B, and C, even when connections share a host. Each operation receives context deadlines and uses the correct role. Share engines according to library guarantees; create scoped sessions, close them, and keep a transaction's operations on its own session [R06]. Disable ORM result caching for eligibility, ownership, and retention checks.

Never call automatic schema synchronization against A or B. Migrations for the tool-owned catalog on C are explicit and separately scoped. Parameterize data values. Database, table, and column identifiers must come from validated mappings and allowlists, and be quoted through the database dialect. Placeholders do not bind SQL identifiers.

### 8 2 Export backend decision

**Recommended provisional backend:** a Go service using xorm for orchestration and database metadata, with the native mysqldump or mariadb-dump client for table DDL and data serialization. This satisfies Go and xorm while reusing the database vendor's export tooling. It requires Q14 to confirm native binaries are permitted.

Invoke a native client using an argument vector through Go's exec.CommandContext, never a shell-built command. Use a restricted client options file for native-client credentials; do not place passwords in command arguments. Match the exporter and restore client to the approved server versions. Choose explicit table lists and backend-specific flags after the compatibility spike. Account for native client's own network connections when setting source load limits.

**Alternative when one pure Go binary is required:** implement a streaming exporter using xorm connections and raw SQL where necessary, retrieving authoritative DDL and serializing typed row values. xorm exposes DumpTables and related APIs, but API availability does not establish complete schema fidelity, snapshot consistency, or support for every deployed type [R05]. Evaluate them against representative restore fixtures before choosing them.

A pure Go exporter must preserve NULL, binary data, unsigned integers, exact decimals, JSON, temporal values, generated columns, quoting, SQL mode, and character set semantics. Avoid reflecting unknown document schemas into one fixed application model. A pure Go SQL importer must not split statements on semicolons: strings, comments, and stored programs can contain them.

Never replay an unreviewed source dump directly into a live destination with destructive clauses. Generate controlled output for staging and an explicit destination mapping. Any source-qualified database names, foreign key references, DEFINER clauses, or client commands must be handled by the chosen backend contract and verified restoration tests.

### 8 3 State and idempotency

Proposed Phase 1 state is an append-only operation journal with atomic index snapshots under STATE_DIR. A local database or an existing dedicated control database is an alternative in Q43. Do not add a separate database server merely to keep local receipts.

A receipt key consists of namespace, document ID, content version, and destination identity. If completion is immutable, a fixed initial version can be used. A remote receipt is committed only after verified restoration; a local receipt is committed after durable artifact publication. SQL dump checksums verify artifact integrity, but are not automatically semantic document hashes because comments or export ordering can change.

The target catalog records staged, published, expiration-pending, and removed states. Keep durable intent before operations whose SQL effects cannot be rolled back. A retry must recognize already published versions, preserve newer content, and reconcile partially completed operations. An imported version must not disappear merely because a later transfer attempt failed.

### 8 4 CLI and package shape

The proposed executable is auto-dump, with commands for validate-config, plan, run, and serve. plan produces counts, filenames, target changes, and expiration candidates without writing document files or changing database objects. run executes once; serve uses the configured daily schedule. A phase argument chooses local export or remote synchronization. Exit codes distinguish success, configuration failure, partial delivery, and execution failure.

Keep packages organized around config, registry, batch, exporter, artifacts, state, target, retention, and scheduler. Phase 2 depends on the same document-reference and artifact contracts established in Phase 1. The local export algorithm must not need a rewrite to introduce a second destination.

## 9 Operational requirements

Treat index and document connections as read only. Restrict target DDL privileges to the dedicated mirror and staging scopes. Use verified TLS for remote database connections, restricted output permissions, and deployment-managed secrets. The driver supports time parsing and connection timeouts, but its loc setting does not change the MySQL session timezone [R07].

Logs must identify the run, phase, counts, durations, batch status, retries, and cleanup outcomes. Avoid document contents and credentials. Produce an operator-readable summary showing selected, exported, delivered, failed, unchanged, and expired documents. Monitoring and failure notification channels remain a deployment decision; this PRD does not authorize sending messages.

Retries use bounded exponential backoff with jitter for transient failures. Configuration errors, authorization errors, unsupported schema, and ownership conflicts require operator action. Stop bounded work on context cancellation, and avoid publication of interrupted files. A failed target connection must not invalidate successful local archive receipts.

Disk-full errors, unavailable source tables, checksum mismatches, import failures, and incomplete selection produce explicit failed or partial results. Phase 2 skips cleanup when its prerequisites fail. Staging leftovers and .partial files are reconciled using their recorded ownership and run status, rather than deleted indiscriminately.

Performance limits need real volume measurements: ready documents per day, existing history, rows and bytes per document, largest document, permitted database load, and link speed to C. Count-based batching does not bound bytes. Start with one worker, then set concurrency, timeouts, staging capacity, and a byte limit from measurements. No throughput or completion SLA is asserted without those inputs.

## 10 Acceptance criteria and verification

The following cases define the proposed behavior. Cases involving disputed policies become binding only after those policies are agreed.

| ID | Scenario | Expected result |
| --- | --- | --- |
| A01 | No ready documents | Success, correct zero counts, no SQL artifact |
| A02 | One ready document | One restorable file with its complete table |
| A03 | Mixed ready and unfinished records | Only ready documents exported |
| A04 | 49, 50, 100, 101, 149, 150, and 201 selected | Exact batch sizes from Section 5 |
| A05 | Gaps in IDs or older ID becomes ready later | Correct members, no skipped ready document |
| A06 | Same document version is rerun | No duplicate delivery or overwritten committed artifact |
| A07 | Empty table, BLOB, NULL, decimal, Unicode, generated column | Restoration preserves the agreed schema and values |
| A08 | Source table changes during export | Detect version conflict or satisfy the approved snapshot contract |
| A09 | Crash during writing or after artifact rename | No partial artifact marked delivered; restart reconciles durable state |
| A10 | Output path unwritable or disk full | Explicit failure; no false receipt |
| A11 | Lower cutoff boundary and one instant before it | Boundary retained; older document expired on C only |
| A12 | Future or unknown document date | Diagnostic; no unverified import or expiration |
| A13 | Fresh and expired managed target documents | Fresh documents added and verified before expiration |
| A14 | Target object without ownership or conflicting identity | Left intact; conflict reported |
| A15 | Import, source selection, or metadata check fails | Run failed or partial; cleanup suppressed |
| A16 | Successful empty remote selection | Eligible expired managed documents can be removed |
| A17 | Crash after DDL but before catalog update | Restart reconciles intent and actual target state |
| A18 | Two overlapping invocations | One owner runs; the other records a skip |
| A19 | Invalid HH:mm, invalid timezone, or DST transition | Agreed scheduling policy applied and tested |
| A20 | Same ID, new approved content version | Local and remote deliveries refreshed independently |
| A21 | Source and destination identities match | Phase 2 refuses an unsafe target mapping |
| A22 | plan command | Reports proposed work; changes no document files or database objects |

Verification requires integration fixtures on every approved MySQL/MariaDB server and client combination, plus restoration to a disposable target. Compare authoritative DDL and the agreed logical values or canonical data hashes; row count alone is insufficient. Unit tests should cover batch boundaries, selection predicates, cutoff arithmetic, identifier handling, and receipt transitions. Exercise crash points in artifact publication, import, and cleanup.

## 11 Implementation phases and release gates

### 11 1 Phase 1

1. Resolve the blocking schema, readiness, immutability, batching, filename, and export-backend questions. Record the accepted policies in this PRD.
2. Run a compatibility spike using representative tables and a restore round trip. Decide native tooling versus a pure Go exporter from evidence.
3. Implement configuration validation, separate xorm source engines, selection and counts, and the plan command.
4. Implement batching, export streaming, manifests, journals, and durable local publication. Add idempotent run behavior.
5. Add the scheduler, overlap protection, cancellation, retry bounds, and operational summaries.
6. Complete Phase 1 acceptance cases and a restore rehearsal; document setup and recovery procedures.

Phase 1 is complete when a configured daily job exports all selected ready documents into correctly named, restorable files, survives restarts without losing delivery state, and gives trustworthy counts and failures. The shared document and artifact contracts are versioned before Phase 2 starts.

### 11 2 Phase 2

1. Freeze C's destination type, supported versions, date field, exact retention policy, ownership rules, and consumer visibility contract.
2. Implement the target-owned catalog, destination identity validation, desired-set comparison, and separate remote receipts.
3. Implement controlled staging, restoration checks, per-document publication, and crash reconciliation.
4. Implement expiration planning, metadata rechecks, scoped cleanup, and resumable cleanup intents.
5. Verify transfer and expiration with cleanup disabled first, then enable the agreed automatic cleanup configuration.
6. Complete Phase 2 acceptance cases and test recovery from target, network, and source failures.

Phase 2 is complete when C converges to the agreed recent-document set after each successful run, additions are verified, old managed objects expire according to the exact cutoff, and unrelated or source objects remain unaffected.

### 11 3 Explicit exclusions pending new scope

Source deletion, source schema redesign, row-level TTL inside document tables, a web UI, arbitrary SQL execution from .env, full-instance replication, point-in-time recovery, and multi-host high availability are excluded from the initial proposal. Some may become necessary if the blocking answers change the system interpretation.

## 12 Research basis and technical decisions

Research reviewed on 13 October 2026. Primary database documentation and maintainer-provided package documentation support the findings below. The architecture choices are recommendations derived from those findings, rather than vendor guarantees for this application's behavior.

**R01 MySQL table statistics.** INFORMATION_SCHEMA.TABLES can contain approximate InnoDB row counts. Use authoritative index selection and SQL counts when exact counts matter. [MySQL reference](https://dev.mysql.com/doc/refman/8.4/en/information-schema-tables-table.html)

**R02 MySQL native exports.** mysqldump produces logical SQL backups and supports row streaming. Its single-transaction behavior provides consistency for transactional tables, with caveats for concurrent DDL and other engines. [MySQL export reference](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html)

**R03 MariaDB native exports.** mariadb-dump has engine and DDL consistency limitations. Its sandbox command can be incompatible with older or MySQL command-line clients, so test both export and import clients. [MariaDB export reference](https://mariadb.com/docs/server/clients-and-utilities/backup-restore-and-import-clients/mariadb-dump)

**R04 DDL transaction boundaries.** MariaDB lists CREATE TABLE, DROP TABLE, and RENAME TABLE among statements causing implicit commits. Recoverable operation state is necessary for target object management. [MariaDB transaction reference](https://mariadb.com/docs/server/reference/sql-statements/transactions/sql-statements-that-cause-an-implicit-commit)

**R05 xorm APIs.** xorm exposes Count, raw SQL, sessions, and DumpTables. The API documentation alone does not prove the export fidelity needed for arbitrary production document tables; validate it in the compatibility spike. [xorm package documentation](https://pkg.go.dev/xorm.io/xorm)

**R06 xorm transactions.** xorm transactions use scoped sessions with explicit begin, commit, rollback, and close. A transaction started on one engine or session does not encompass another connection or an external dump process. [xorm transaction documentation](https://xorm.io/docs/chapter-10/readme/)

**R07 MySQL driver behavior.** The maintained Go driver documents TLS, connection timeouts, context cancellation, parseTime, and loc. The loc parameter changes Go time interpretation, not the server session timezone. [Driver documentation](https://github.com/go-sql-driver/mysql)

**R08 Scheduling.** robfig/cron supports an explicit timezone and skipping overlapping invocations. Its documentation warns that schedules during a DST forward gap do not run. Persistent occurrence tracking and cross-process ownership are application concerns. [Scheduler documentation](https://pkg.go.dev/github.com/robfig/cron/v3)

## 13 Questions requiring answers

P0 questions block the affected implementation decisions. P1 questions should be settled before the corresponding release. Suggested answers are proposals and are not automatically accepted.

### 13 1 P0 data and export decisions

**Q01 What exactly is one document?** One table, several tables, one full database, or one row? Suggested interpretation: one table, because the notes explicitly say 100 tables per file.

**Q02 What are the actual index and document schemas?** Provide sanitized CREATE TABLE statements for the index and representative document tables, field mappings, and the logical server layout. No production credentials are needed for design.

**Q03 What exactly makes a document ready?** Name the status values or predicate, who sets it, and whether readiness occurs after all table data is committed. Can readiness be revoked or a ready table be edited?

**Q04 What is the unique identity and ID-to-table rule?** Are IDs numeric, strings with leading zeros, or tenant-scoped? Does the index store the exact schema/table name, or must the tool derive it?

**Q05 Should Phase 1 export all eligible documents every run or only undelivered versions?** Suggested answer: initial full selection followed by incremental exports, with independently configured local history.

**Q06 Is 100 a target batch size or a strict maximum?** Should 101 documents produce one file with 101 tables or files with 100 and 1? Confirm that a remainder of exactly 50 starts a separate file.

**Q07 Does the small remainder merge within the current run or into a previous run's last file?** Suggested answer: current run only; historical append needs a different publication and receipt design.

**Q08 What does 100001-100100 mean in the filename?** Actual document IDs, a fixed ID bucket, export sequence numbers, or a count range? Confirm extension, padding, single-document name, and the merged-file example for 101 documents.

**Q09 What must exist on the third server?** Restored SQL tables, only dump files, or both? The working proposal is a restored database mirror because the notes refer to removing tables.

### 13 2 P0 retention and compatibility decisions

**Q10 Which exact timestamp defines a document's age?** Business date, created_at, ready_at, or another field? What timezone and SQL type does it use? Suggested answer: an explicit authoritative document_date.

**Q11 What is the exact date window?** Rolling 720 hours, the latest 30 local calendar days, or from the 12th of the previous month? Was “the 12th” merely an example? Confirm lower and upper boundaries and inclusivity.

**Q12 Are entire document tables copied and removed by document age, or are individual old rows removed?** Suggested answer: document-level selection and table-level expiration on C only.

**Q13 Can completed documents be revised, and how is a revision identified?** Suggested answer: immutable completed documents, otherwise a reliable version token reflecting both payload and schema changes.

**Q14 Are native dump and import clients allowed, or must delivery be one pure Go binary?** Suggested answer: Go and xorm for the application with approved vendor clients for serialization, subject to deployment constraints.

**Q15 Are document tables independent?** Do they have cross-table foreign keys, triggers, views, procedures, events, or qualified schema references? Decide which objects must be exported and what unsupported dependencies should do.

**Q16 What server, storage-engine, and client versions must be supported?** Identify MySQL versus MariaDB on A, B, and C, InnoDB versus other engines, SQL modes, and whether cross-vendor restore is required.

### 13 3 P1 data quality and destination behavior

**Q17 What are the real volume and load limits?** Current document count, daily arrivals, typical and largest table size, link bandwidth, source query budget, and acceptable run duration determine capacity and concurrency.

**Q18 How should duplicate index entries be resolved?** Which record is authoritative when several rows name the same document or inconsistent source objects?

**Q19 What should happen to null, future, or invalid document timestamps?** Suggested answer: explicit diagnostics and no date-based cleanup without a verified age.

**Q20 Should one missing table fail the whole run or only its batch?** Suggested answer: fail that batch, continue independent batches, mark the overall run partial, and suppress Phase 2 cleanup.

**Q21 How should C handle a document removed from the index or no longer ready?** Keep until normal expiration, remove immediately, or quarantine? A missing index row is not enough to infer a deletion request.

**Q22 May the tool adopt preexisting target tables?** Suggested answer: explicit migration only; unknown tables remain untouched and name collisions are reported.

**Q23 How do consumers read C during publication?** Can tables appear individually? Are per-table swaps acceptable, or is one globally consistent generation required?

**Q24 Should triggers or scheduled events run on C?** Restoring them can create side effects. Define treatment of DEFINER accounts and any executable objects required for consumers.

### 13 4 P1 artifact format and naming

**Q25 How long should local archives remain available?** Is local expiration required, and may old export manifests or receipts be compacted independently of SQL artifacts?

**Q26 Does one file per batch allow a manifest sidecar?** Suggested answer: one SQL payload file plus metadata; if only one physical file is allowed, specify an embedded manifest format.

**Q27 Should SQL files be plain, gzip compressed, or encrypted?** Confirm the consumer's import tooling and whether encryption keys are supplied by deployment secrets.

**Q28 Is there a maximum file size?** Decide whether byte limits override 100-table grouping, and how to handle one document larger than that limit.

**Q29 What is the batch ordering and grouping scope?** Numeric ID, date, or another order? Can one file combine tenants or source schemas?

**Q30 At what point must document counts be exact?** Suggested answer: for the run's frozen selected set; readiness changes after selection are reported on the next run.

### 13 5 P1 scheduling and connectivity

**Q31 What daily time and timezone should production use?** Confirm DST gap/repeat behavior, startup execution, and whether a missed schedule should catch up or be skipped.

**Q32 Can several processes or hosts run this job?** Suggested answer: one runner initially; otherwise specify shared run ownership and fencing.

**Q33 How is configuration and secret rotation managed?** Confirm .env location, environment overrides, restart-based reloading, and restricted native-client options files.

**Q34 How does the runner reach the third server?** Direct SQL connection, SSH tunnel, SFTP plus a remote importer, or another path? Confirm network restrictions and destination authentication.

**Q35 Are there database maintenance windows or bandwidth caps?** Define when dumps may run, how long DDL or replication lag can interrupt them, and whether exports should use an approved replica.

**Q36 May expiration be delayed during a source or transfer outage?** Suggested answer: suppress cleanup until selection and delivery prerequisites succeed. A hard retention deadline changes that policy.

**Q37 Should cleanup have a per-run volume or percentage limit?** If so, specify the limit and how to handle a valid large bootstrap expiration without repeatedly leaving old data behind.

### 13 6 P1 verification and operations

**Q38 Who receives and handles failures?** Define logs, metrics, alert destination, operator ownership, and acceptable time to discover a missed scheduled run.

**Q39 How should the tool recover when state is lost?** Rebuild from retained manifests, inspect a target catalog, or re-export? Confirm recovery granularity and which metadata must be backed up.

**Q40 What proves a restored document is correct?** Artifact checksum, normalized schema comparison, row counts, canonical value hashes, or a combination? Suggested answer: restore verification beyond row count alone.

**Q41 Can document tables lack primary keys?** This affects stable row ordering, data hashing, and the feasibility of canonical comparison.

**Q42 Which unusual types and SQL modes occur?** Confirm binary data, large BLOBs, JSON, unsigned BIGINT, exact decimals, zero dates, generated columns, collation differences, and maximum statement/packet sizes.

**Q43 Where should durable tool state live?** Local journal and atomic snapshots, local SQLite, or an existing dedicated control database? Confirm backup and permission requirements.

**Q44 What deployment constraints apply?** Operating system, Go toolchain, container versus system service, allowed dependencies, native-client installation, and TLS trust material.

**Q45 How should the two phases run once Phase 2 exists?** Sequential local export then remote delivery, independent jobs, or remote-only runs? Suggested answer: shared contracts with separate destination receipts and retry behavior.

**Q46 Which sanitized fixtures and disposable databases are available?** Provide representative schemas and data shapes for compatibility, restoration, and failure tests.

**Q47 What completion and recovery objectives are required?** Set maximum run time, tolerated mirror lag, tolerated archive gaps, and recovery time after interrupted export or import.

**Q48 How are schema and state formats upgraded?** Decide whether existing documents can gain columns and how old manifests, receipts, and artifacts remain interpretable.

**Q49 What operational controls should enable automatic cleanup?** Suggested answer: inspect plan output during rollout, then use explicit deployment configuration with scoped credentials for normal unattended runs.

**Q50 What data may appear in filenames, manifests, and logs?** Confirm whether document IDs or metadata are sensitive, who can read exports, and any audit or encryption requirements.

### 13 7 Minimum material for implementation

To turn this draft into an implementation specification, supply the answers to Q01–Q16, sanitized index and document DDL, a few representative index rows, the expected filename for the 101-document case, one exact dated retention example, and the destination layout. Performance and operations answers can then refine limits without guessing the application's core behavior.
