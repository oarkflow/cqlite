# Production Readiness Tasks

This roadmap is the canonical checklist for making `github.com/sujit/sqlite` production-ready. The project must not be described as production-ready until every P0-P8 milestone is complete and the required correctness, crash, fuzz, race, parity, and benchmark gates pass.

Status legend:

- `[ ]` todo
- `[~]` in progress
- `[x]` done
- `[!]` blocked

## Production Readiness Gates

- [x] Complete all P0-P8 milestones.
- [x] Pass `GOCACHE=$(pwd)/.gocache go test ./...`.
- [x] Pass `GOCACHE=$(pwd)/.gocache go test -race ./...`.
- [x] Pass `GOCACHE=$(pwd)/.gocache go test -bench . -benchmem -run '^$'`.
- [x] Pass sqlite3 parity suites using local `/usr/bin/sqlite3`.
- [x] Pass crash/recovery suites.
- [x] Pass fuzz suites with persisted failing corpus.
- [x] Confirm every performance optimization preserves durability and SQLite-compatible behavior.

## P0: Safety Cleanup And Baseline

- [x] Remove or quarantine benchmark-shaped logic; keep only schema/AST-driven generic planner paths.
- [x] Add a static check or regression test that rejects hardcoded benchmark table/query fast paths.
- [x] Record a benchmark baseline against `github.com/mattn/go-sqlite3`.
- [x] Document required CI commands:
  - `GOCACHE=$(pwd)/.gocache go test ./...`
  - `GOCACHE=$(pwd)/.gocache go test -race ./...`
  - `GOCACHE=$(pwd)/.gocache go test -bench . -benchmem -run '^$'`
  - sqlite parity suite with local `sqlite3`
- [x] Completion gate: safety checks and baseline benchmarks are committed.

### P0 Benchmark Baseline

Recorded on Apple M2 Pro with `GOCACHE=$(pwd)/.gocache go test -bench ... -benchmem -run '^$'`. The latest write-path remediation removed eager checkpoints, staged insert-only transaction rows, added positional hot inserts, and generalized indexed update fast paths.

| Operation | Custom | mattn/go-sqlite3 |
| --- | ---: | ---: |
| insert transaction | `574-666 ns/op`, `706-760 B/op`, `9-11 allocs/op` | `899-1358 ns/op`, `288 B/op`, `8-9 allocs/op` |
| insert autocommit SyncFull | `23.5-31.0 us/op`, `964-989 B/op`, `17-18 allocs/op` | `36.8-94.5 us/op`, `255 B/op`, `8 allocs/op` |
| point lookup primary key | `720 ns/op`, `357 B/op`, `10 allocs/op` | `2481 ns/op`, `608 B/op`, `22 allocs/op` |
| indexed lookup | `2144 ns/op`, `1736 B/op`, `52 allocs/op` | `4014 ns/op`, `627 B/op`, `33 allocs/op` |
| range scan | `25.7 us/op`, `5312 B/op`, `39 allocs/op` | `20.9 us/op`, `1891 B/op`, `188 allocs/op` |
| full scan | `126 us/op`, `49.6 KB/op`, `10 allocs/op` | `337 us/op`, `62.5 KB/op`, `4764 allocs/op` |
| update by primary key SyncFull | `33.3 us/op`, `831 B/op`, `10 allocs/op` | `50.1 us/op`, `133 B/op`, `5 allocs/op` |
| join aggregate | `77.3 us/op`, `5706 B/op`, `121 allocs/op` | `170 us/op`, `1248 B/op`, `75 allocs/op` |
| insert autocommit SyncOff | `6088 ns/op`, `986 B/op`, `18 allocs/op` | `8065 ns/op`, `256 B/op`, `8 allocs/op` |
| insert batch SyncOff | `109-119 us/op`, `136-139 KB/op`, `1479-1579 allocs/op` | `100-121 us/op`, `26.4 KB/op`, `920 allocs/op` |
| update by primary key SyncOff | `6063 ns/op`, `831 B/op`, `10 allocs/op` | `6630 ns/op`, `133 B/op`, `5 allocs/op` |
| update by secondary index SyncOff | `11.3 us/op`, `5630 B/op`, `66 allocs/op` | `38.2 us/op`, `152 B/op`, `7 allocs/op` |
| delete by primary key SyncOff | `5092 ns/op`, `344 B/op`, `7 allocs/op` | `7824 ns/op`, `135 B/op`, `5 allocs/op` |
| delete by secondary index SyncOff | `5880 ns/op`, `744 B/op`, `13 allocs/op` | `11.4 us/op`, `168 B/op`, `7 allocs/op` |
| concurrent mixed read/write SyncOff | `1363 ns/op`, `313 B/op`, `9 allocs/op` | `5342 ns/op`, `627 B/op`, `19 allocs/op` |

### 2026-06-03 Benchmark Validation

Recorded on Apple M2 Pro with `GOCACHE=$(pwd)/.gocache go test -bench 'BenchmarkSQLite(Comparison|ThroughputComparison)' -benchmem -run '^$'` after binary state WAL fallback records, lazy runtime rebuilds on checkpoint load, pager freelist dirty tracking, streaming range scans, lazy checkpoint row loading, clean-checkpoint WAL replay elision, and lower-allocation transaction command classification.

| Operation | Custom | mattn/go-sqlite3 |
| --- | ---: | ---: |
| insert transaction | `493.8 ns/op`, `616 B/op`, `7 allocs/op` | `940.2 ns/op`, `288 B/op`, `8 allocs/op` |
| insert autocommit SyncFull | `26.0 us/op`, `800 B/op`, `14 allocs/op` | `29.8 us/op`, `255 B/op`, `8 allocs/op` |
| insert batch SyncOff | `100.5 us/op`, `124.8 KB/op`, `1279 allocs/op` | `106.3 us/op`, `26.4 KB/op`, `920 allocs/op` |
| point lookup primary key | `888 ns/op`, `869 B/op`, `11 allocs/op` | `2467 ns/op`, `608 B/op`, `22 allocs/op` |
| indexed lookup | `826 ns/op`, `544 B/op`, `10 allocs/op` | `4117 ns/op`, `627 B/op`, `33 allocs/op` |
| range scan | `2570 ns/op`, `2792 B/op`, `17 allocs/op` | `21.7 us/op`, `1891 B/op`, `188 allocs/op` |
| full scan | `129.7 us/op`, `56.2 KB/op`, `14 allocs/op` | `340.2 us/op`, `62.5 KB/op`, `4764 allocs/op` |
| update by primary key SyncFull | `26.5 us/op`, `615 B/op`, `7 allocs/op` | `29.9 us/op`, `133 B/op`, `5 allocs/op` |
| update by secondary index SyncOff | `7947 ns/op`, `5423 B/op`, `64 allocs/op` | `25.0 us/op`, `152 B/op`, `7 allocs/op` |
| delete by primary key SyncOff | `2774 ns/op`, `207 B/op`, `4 allocs/op` | `5592 ns/op`, `135 B/op`, `5 allocs/op` |
| delete by secondary index SyncOff | `3262 ns/op`, `607 B/op`, `10 allocs/op` | `8296 ns/op`, `168 B/op`, `7 allocs/op` |
| join aggregate | `20.5 us/op`, `1432 B/op`, `12 allocs/op` | `167.5 us/op`, `1248 B/op`, `75 allocs/op` |
| reopen/recovery | `153.9 us/op`, `58.8 KB/op`, `123 allocs/op` | `355.8 us/op`, `2841 B/op`, `66 allocs/op` |
| large payload | `25.1 us/op`, `96.4 KB/op`, `19 allocs/op` | `52.4 us/op`, `303 B/op`, `8 allocs/op` |
| concurrent mixed read/write SyncOff | `1341 ns/op`, `546 B/op`, `10 allocs/op` | `4382 ns/op`, `627 B/op`, `19 allocs/op` |
| insert autocommit SyncOff | `3236 ns/op`, `784 B/op`, `14 allocs/op` | `6199 ns/op`, `256 B/op`, `8 allocs/op` |
| update by primary key SyncOff | `3462 ns/op`, `619 B/op`, `8 allocs/op` | `6047 ns/op`, `133 B/op`, `5 allocs/op` |

Custom is faster on every broad comparison workload and both SyncOff throughput workloads in the latest full matrix. The custom engine still allocates more on many write-heavy paths, so future work should target allocation reduction without trading away correctness or durability.

## P1: Real Pager-Backed Storage

- [x] Replace row storage as `map[string]any` with schema-positioned row values.
- [x] Add schema-positioned row value slices beside the legacy map view, with codec, clone, ALTER TABLE, update, FK-action, and checkpoint synchronization.
- [x] Make committed table/index persistence page-backed by default, not close-only snapshot-first.
- [x] Store table leaf pages, overflow pages, and free-list pages through the pager.
- [x] Store index pages through the same pager path.
- [x] Add page split/merge tests.
- [x] Add overflow page tests.
- [x] Add free-list reuse tests.
- [x] Add reopen tests for table and index persistence.
- [x] Store checkpointed table row page payloads with a schema-positioned binary row codec instead of JSON row blobs.
- [x] Store checkpointed index payloads with a binary index record codec instead of JSON index entries.
- [x] Store pager record payloads as binary bytes instead of JSON-wrapped records, with legacy JSON read fallback.
- [x] Use generation-stamped checkpoint files and clean up old committed generations.
- [x] Remove whole-state JSON snapshot WAL writes from autocommit and explicit transaction commit paths.
- [x] Completion gate: no normal DML path depends on whole-state JSON snapshots for persistence.

## P2: B+ Tree Table And Index Engine

- [x] Use B+ trees for runtime table primary-key/rowid access.
- [x] Use B+ trees for runtime secondary indexes.
- [x] Support runtime B+tree point lookup, range scan, ordered scan, insert, update, delete, split, merge, and cursor iteration.
- [x] Keep secondary indexes transactionally consistent with table changes.
- [x] Add randomized operation tests comparing table/index results to sqlite3.
- [x] Completion gate: indexed scans and primary-key scans read from runtime B+ trees, not in-memory helper maps.

## P3: WAL, Recovery, And Crash Matrix

- [x] Replace new logical SQL replay with binary mutation/state WAL records for normal storage mutations; keep legacy logical replay only for older WAL compatibility.
- [x] Replay simple autocommit and simple explicit-transaction DML from binary mutation WAL records without reconstructing SQL text.
- [x] Add physical page WAL records and open-time replay for checkpoint metadata pager pages.
- [x] Keep checksummed records and committed-transaction boundaries.
- [x] Add physical page WAL record encoding, replay decoding, and fuzz coverage.
- [x] Persist explicit transactions as one committed WAL batch instead of a full state snapshot.
- [x] Add crash test: committed transaction before checkpoint survives.
- [x] Add crash test: uncommitted transaction before checkpoint is absent.
- [x] Add crash test: partial WAL record is ignored or rejected safely.
- [x] Add crash test: corrupt checksum is rejected safely.
- [x] Add crash test: checkpoint generation interruption recovers safely.
- [x] Add crash test: mixed table/index updates recover atomically.
- [x] Add crash test: committed WAL without checkpoint survives reopen.
- [x] Add crash test: committed WAL survives missing checkpoint files.
- [x] Add checkpoint policy tests: no default autocommit checkpoint, manual checkpoint, and transaction-threshold checkpoint.
- [x] Completion gate: crash/reopen tests prove committed data survives and uncommitted data is absent.

## P4: Transactions, Locking, And Concurrency

- [x] Implement conservative database file locks for cross-process safety.
- [x] Support SQLite-style shared/reserved writer file lock states for cross-process reader opens and serialized writers.
- [x] Support SQLite-style read transactions in the current single-process snapshot model.
- [x] Enforce one writer at a time within a process.
- [x] Support `BEGIN`, `BEGIN DEFERRED`, `BEGIN IMMEDIATE`, `BEGIN EXCLUSIVE`, `COMMIT`, `ROLLBACK`, and savepoints in embedded DB execution and explicit `Tx` execution.
- [x] Ensure readers do not observe partial writer state in current single-process snapshot model.
- [x] Add concurrent test: many readers.
- [x] Add concurrent test: writer serialization.
- [x] Add concurrent test: conflicting writers.
- [x] Add concurrent test: rollback during readers.
- [x] Add multi-process test: competing opens and writes.
- [x] Fix parser AST race by serializing parse-and-execute use of the oarkflow parser arena.
- [x] Completion gate: `go test -race ./...` and multi-process locking tests pass.

## P5: SQLite SQL Compatibility

- [x] Expand parity tests against `/usr/bin/sqlite3`.
- [x] Cover DDL behavior.
- [x] Cover DML behavior.
- [x] Cover expressions, casts, arithmetic, functions, and NULL logic.
- [x] Cover type affinity and STRICT tables.
- [x] Cover constraints, defaults, generated columns where parser support allows, and foreign keys.
- [x] Enforce persisted column and table `CHECK` constraints for simple comparison, `AND`, and null-check expressions.
- [x] Cover indexes, including unique, composite, partial, expression, and covering indexes where supported.
- [x] Cover joins, aggregates, ordering, limits, and subqueries.
- [x] Add parity coverage and execution support for scalar subqueries, `EXISTS`, and `IN (SELECT ...)` predicates in simple query/update/delete paths.
- [x] Cover PRAGMAs, collations, and schema metadata.
- [x] Add basic built-in collation support for declared `NOCASE` and `RTRIM` columns in comparisons, uniqueness checks, and ordering.
- [x] Add an explicit allowlist for unsupported SQLite features.
- [x] Completion gate: every supported SQL behavior has parity coverage; every unsupported behavior has a documented error.

## P6: Query Planner And Executor

- [x] Replace narrow fast paths with planner rules.
- [x] Add table scan plan.
- [x] Add primary-key lookup plan.
- [x] Add secondary-index lookup plan.
- [x] Add range scan plan.
- [x] Add generic compiled simple-select path for bare-column single-table scans, point lookups, index lookups, and range scans.
- [x] Add covering index scan plan.
- [x] Add nested-loop join plan.
- [x] Add aggregate/group-by plan.
- [x] Add order-by index usage.
- [x] Add `EXPLAIN QUERY PLAN` assertions for chosen indexes and access paths.
- [x] Completion gate: benchmark wins come from general planner rules, not query-specific shortcuts.

## P7: `database/sql`, CLI, And Server Reliability

- [x] Complete driver support for prepared statements.
- [x] Complete driver support for typed result values and inferred column storage classes.
- [x] Complete driver and embedded API support for named parameters.
- [x] Complete driver support for column metadata.
- [x] Complete driver support for transactions and savepoints.
- [x] Complete driver support for context cancellation.
- [x] Complete basic driver error mapping for transaction option errors and database locks.
- [x] Add CLI multi-line statement support.
- [x] Add CLI `.schema`, `.tables`, `.mode`, `.headers`, and `.quit`.
- [x] Keep MySQL server scaffold deprioritized until embedded engine passes P0-P6.
- [x] Completion gate: standard `database/sql` integration tests and CLI smoke tests pass.

## P8: Fuzzing, Corruption, And Performance Validation

- [x] Add fuzz tests for row decoding.
- [x] Add fuzz tests for page decoding.
- [x] Add fuzz tests for WAL replay.
- [x] Add fuzz tests for SQL expression evaluation.
- [x] Add fuzz tests for random operation sequences compared with sqlite3.
- [x] Add corruption tests for page/pager headers.
- [x] Add corruption tests for checksums.
- [x] Add corruption tests for overflow chains.
- [x] Add corruption tests for index pages.
- [x] Add corruption tests for table pages and checkpoint metadata.
- [x] Add corruption tests for WAL records.
- [x] Add torn-write simulation for partial trailing WAL payloads.
- [x] Add destructive missing-generation simulation for checkpoint table pages.
- [x] Expand benchmarks for inserts: transaction, autocommit, batch, prepared.
- [x] Expand benchmarks for updates/deletes by primary key and index.
- [x] Expand benchmarks for point, range, index, and full scans.
- [x] Expand benchmarks for joins and aggregates.
- [x] Expand benchmarks for reopen/recovery time.
- [x] Expand benchmarks for large text/blob/json payloads.
- [x] Expand benchmarks for concurrent readers and writer.
- [x] Completion gate: custom engine is faster or acceptably close on broad workloads, with no correctness regressions.

## Public Interfaces And Compatibility

- [x] Preserve `Open(path string, opts Options) (*DB, error)`.
- [x] Preserve `Exec(ctx, sql, args...)`.
- [x] Preserve `Query(ctx, sql, args...)`.
- [x] Preserve `Begin(ctx, TxOptions)`.
- [x] Preserve driver name `sujitsqlite`.
- [x] Add richer `Rows`/column metadata for `database/sql` compatibility.
- [x] Add explicit crash-test lock bypass through `Options.DisableFileLock`.
- [x] Add optional diagnostic planner output only if needed for observability or planner tests.
- [x] Do not implement SQLite byte-for-byte file-format compatibility unless explicitly requested later.

## Assumptions

- [x] “Fix everything” means production-readiness work, not cosmetic benchmark-only optimization.
- [x] SQLite-compatible behavior is the target.
- [x] SQLite byte-for-byte file format is out of scope.
- [x] Pure Go remains required.
- [x] Correctness and durability outrank benchmark wins.
- [x] MySQL wire protocol remains deferred until the embedded engine is substantially production-ready.
