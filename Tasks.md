# Production Readiness Tasks

This roadmap is the canonical checklist for making `github.com/sujit/sqlite` production-ready. The project must not be described as production-ready until every P0-P8 milestone is complete and the required correctness, crash, fuzz, race, parity, and benchmark gates pass.

Status legend:

- `[ ]` todo
- `[~]` in progress
- `[x]` done
- `[!]` blocked

## Production Readiness Gates

- [ ] Complete all P0-P8 milestones.
- [x] Pass `GOCACHE=$(pwd)/.gocache go test ./...`.
- [x] Pass `GOCACHE=$(pwd)/.gocache go test -race ./...`.
- [x] Pass `GOCACHE=$(pwd)/.gocache go test -bench . -benchmem -run '^$'`.
- [x] Pass sqlite3 parity suites using local `/usr/bin/sqlite3`.
- [ ] Pass crash/recovery suites.
- [ ] Pass fuzz suites with persisted failing corpus.
- [ ] Confirm every performance optimization preserves durability and SQLite-compatible behavior.

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

Recorded on Apple M2 Pro with `GOCACHE=$(pwd)/.gocache go test -bench . -benchmem -benchtime=3000x -run '^$'`.

| Operation | Custom | mattn/go-sqlite3 |
| --- | ---: | ---: |
| insert transaction | `776.7 ns/op`, `595 B/op`, `10 allocs/op` | `795.2 ns/op`, `284 B/op`, `8 allocs/op` |
| insert autocommit SyncFull | `21.7 us/op`, `747 B/op`, `11 allocs/op` | `37.1 us/op`, `252 B/op`, `8 allocs/op` |
| point lookup primary key | `632.8 ns/op`, `341 B/op`, `10 allocs/op` | `2285 ns/op`, `608 B/op`, `22 allocs/op` |
| indexed lookup | `2144 ns/op`, `1736 B/op`, `52 allocs/op` | `4014 ns/op`, `627 B/op`, `33 allocs/op` |
| range scan | `7312 ns/op`, `5304 B/op`, `39 allocs/op` | `19.8 us/op`, `1891 B/op`, `188 allocs/op` |
| full scan | `61.7 us/op`, `49.5 KB/op`, `10 allocs/op` | `325 us/op`, `62.5 KB/op`, `4764 allocs/op` |
| update by primary key SyncFull | `20.5 us/op`, `125 B/op`, `3 allocs/op` | `38.1 us/op`, `133 B/op`, `5 allocs/op` |
| join aggregate | `77.3 us/op`, `5706 B/op`, `121 allocs/op` | `170 us/op`, `1248 B/op`, `75 allocs/op` |
| insert autocommit SyncOff | `2278 ns/op`, `747 B/op`, `11 allocs/op` | `7539 ns/op`, `252 B/op`, `8 allocs/op` |
| insert batch SyncOff | `8.43 ms/op`, `13.8 MB/op`, `154912 allocs/op` | `101 us/op`, `26.4 KB/op`, `920 allocs/op` |
| update by primary key SyncOff | `1845 ns/op`, `125 B/op`, `3 allocs/op` | `7401 ns/op`, `133 B/op`, `5 allocs/op` |
| update by secondary index SyncOff | `267 us/op`, `5119 B/op`, `81 allocs/op` | `29.3 us/op`, `152 B/op`, `7 allocs/op` |
| delete by primary key SyncOff | `3693 ns/op`, `135 B/op`, `4 allocs/op` | `6033 ns/op`, `135 B/op`, `5 allocs/op` |
| delete by secondary index SyncOff | `2333 ns/op`, `199 B/op`, `8 allocs/op` | `8736 ns/op`, `168 B/op`, `7 allocs/op` |
| concurrent mixed read/write SyncOff | `943.9 ns/op`, `271 B/op`, `9 allocs/op` | `4579 ns/op`, `627 B/op`, `19 allocs/op` |

## P1: Real Pager-Backed Storage

- [~] Replace row storage as `map[string]any` with schema-positioned row values.
- [~] Make table reads/writes page-backed by default, not snapshot-first.
- [~] Store table leaf pages, overflow pages, and free-list pages through the pager.
- [~] Store index pages through the same pager path.
- [ ] Add page split/merge tests.
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

- [~] Use B+ trees for table primary storage.
- [x] Use B+ trees for runtime secondary indexes.
- [~] Support point lookup, range scan, ordered scan, insert, update, delete, split, merge, and cursor iteration.
- [~] Keep secondary indexes transactionally consistent with table changes.
- [x] Add randomized operation tests comparing table/index results to sqlite3.
- [ ] Completion gate: indexed scans and primary-key scans read from B+ trees, not in-memory helper maps.

## P3: WAL, Recovery, And Crash Matrix

- [ ] Replace logical SQL replay with page/physical WAL records for normal storage mutations.
- [x] Keep checksummed records and committed-transaction boundaries.
- [x] Add physical page WAL record encoding, replay decoding, and fuzz coverage.
- [x] Persist explicit transactions as one committed WAL batch instead of a full state snapshot.
- [x] Add crash test: committed transaction before checkpoint survives.
- [x] Add crash test: uncommitted transaction before checkpoint is absent.
- [x] Add crash test: partial WAL record is ignored or rejected safely.
- [x] Add crash test: corrupt checksum is rejected safely.
- [x] Add crash test: checkpoint generation interruption recovers safely.
- [x] Add crash test: mixed table/index updates recover atomically.
- [ ] Completion gate: crash/reopen tests prove committed data survives and uncommitted data is absent.

## P4: Transactions, Locking, And Concurrency

- [x] Implement conservative database file locks for cross-process safety.
- [ ] Support SQLite-style shared/reserved/exclusive file lock states.
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
- [ ] Completion gate: `go test -race ./...` and multi-process locking tests pass.

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
- [ ] Completion gate: every supported SQL behavior has parity coverage; every unsupported behavior has a documented error.

## P6: Query Planner And Executor

- [ ] Replace narrow fast paths with planner rules.
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
- [ ] Completion gate: benchmark wins come from general planner rules, not query-specific shortcuts.

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
- [ ] Keep MySQL server scaffold deprioritized until embedded engine passes P0-P6.
- [ ] Completion gate: standard `database/sql` integration tests and CLI smoke tests pass.

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
- [x] Add corruption tests for WAL records.
- [x] Expand benchmarks for inserts: transaction, autocommit, batch, prepared.
- [x] Expand benchmarks for updates/deletes by primary key and index.
- [x] Expand benchmarks for point, range, index, and full scans.
- [x] Expand benchmarks for joins and aggregates.
- [x] Expand benchmarks for reopen/recovery time.
- [x] Expand benchmarks for large text/blob/json payloads.
- [x] Expand benchmarks for concurrent readers and writer.
- [ ] Completion gate: custom engine is faster or acceptably close on broad workloads, with no correctness regressions.

## Public Interfaces And Compatibility

- [x] Preserve `Open(path string, opts Options) (*DB, error)`.
- [x] Preserve `Exec(ctx, sql, args...)`.
- [x] Preserve `Query(ctx, sql, args...)`.
- [x] Preserve `Begin(ctx, TxOptions)`.
- [x] Preserve driver name `sujitsqlite`.
- [x] Add richer `Rows`/column metadata for `database/sql` compatibility.
- [x] Add explicit crash-test lock bypass through `Options.DisableFileLock`.
- [ ] Add optional diagnostic planner output only if needed for observability or planner tests.
- [ ] Do not implement SQLite byte-for-byte file-format compatibility unless explicitly requested later.

## Assumptions

- [ ] “Fix everything” means production-readiness work, not cosmetic benchmark-only optimization.
- [ ] SQLite-compatible behavior is the target.
- [ ] SQLite byte-for-byte file format is out of scope.
- [ ] Pure Go remains required.
- [ ] Correctness and durability outrank benchmark wins.
- [ ] MySQL wire protocol remains deferred until the embedded engine is substantially production-ready.
