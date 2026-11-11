# Production Readiness Tasks

This roadmap is the canonical checklist for making `github.com/oarkflow/cqlite` production-ready. The project must not be described as production-ready until every P0-P8 milestone is complete and the required correctness, crash, fuzz, race, parity, and benchmark gates pass.

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

The June 3 matrix below is historical and is not a current performance claim. The November 2026 measurements below include workloads where the custom engine trails SQLite; continue targeting those gaps without trading away correctness or durability.

### 2026-11-11 Focused Performance Validation

Recorded on Apple M2 Pro after compact insert-batch WAL encoding, hardware-accelerated CRC32, column-level update rollback, and platform WAL data-sync. The original focused matrix used `-benchtime=500ms -count=3`; the refreshed FULL autocommit row used `-benchtime=1s -count=3`. FULL-sync WAL commits use the platform data-sync primitive (`fdatasync`) and sync the WAL directory when the file is first created. The transaction benchmark keeps `Commit()` inside the timed section; earlier transaction timings excluded the durable commit and were not a valid FULL-sync comparison.

| Operation | Custom | mattn/go-sqlite3 |
| --- | ---: | ---: |
| WAL FULL autocommit insert | `29.6-31.0 us/op` | `27.8-28.9 us/op` |
| WAL NORMAL autocommit insert | `4.15-4.23 us/op` | `7.42-7.63 us/op` |
| WAL OFF autocommit insert | `4.2-4.3 us/op` | `5.5 us/op` |
| FULL-sync large text/blob/JSON insert | `35.0-40.3 us/op` | `48.6-53.2 us/op` |
| 10 inserts in one FULL transaction, total | `68.7 us` | `98.3 us` |
| 100-row transaction batch, SyncOff | `98.6-101.5 us/op` | `97.3-97.9 us/op` |
| large text/blob/JSON autocommit insert, SyncOff | `20.6-22.2 us/op` | `51.2-59.8 us/op` |
| primary-key update, SyncOff | `3.97-4.01 us/op`, `462-463 B/op`, `6 allocs/op` | `5.15-5.22 us/op`, `133 B/op`, `5 allocs/op` |

Additional comparisons on the same host (`-benchtime=250ms -count=2`) show the
custom engine ahead on each tested read and mixed workload:

| Operation | Custom | mattn/go-sqlite3 |
| --- | ---: | ---: |
| Point lookup by primary key | `1.49-1.56 us/op` | `2.39-2.41 us/op` |
| Indexed lookup | `1.40-1.49 us/op` | `4.23-4.24 us/op` |
| Range scan | `2.92-3.11 us/op` | `20.92-21.04 us/op` |
| Full scan | `123.8-130.5 us/op` | `319.8-320.2 us/op` |
| Join and aggregate | `25.86-26.08 us/op` | `167.7-170.6 us/op` |
| Concurrent mixed read/write | `2.05-2.38 us/op` | `4.18-4.58 us/op` |
| Primary-key update | `24.77-25.50 us/op` | `32.37-33.56 us/op` |
| Primary-key delete | `3.82-3.84 us/op` | `5.56-5.61 us/op` |

A refreshed join-and-aggregate comparison after the November 11 positional-row and integer-sum fast-path changes (`-benchtime=1s -count=3`) measured `62.3-64.2 us/op`, `4161-4166 B/op`, and 30 allocations for the custom engine, versus `162.1-176.3 us/op`, `1263-1264 B/op`, and 75 allocations for mattn/go-sqlite3. The custom path returns SQLite-compatible integer sums and text ordering, including column collations; the older row above is retained as a historical measurement.

FULL autocommit is now close to SQLite on this host after switching to data-only WAL sync; NORMAL and OFF remain faster for the custom engine. FULL remains the default to preserve per-commit power-loss durability. Six concurrency tests covering shared handles, cross-process writers, concurrent readers, transaction rebasing, and cancellable lock waits passed 20 runs at `GOMAXPROCS=16` and 10 race-enabled runs. The cross-process lost-update regression stresses 100 updates from each process. An abrupt child-process exit after a FULL-sync commit is also covered; the committed row recovered in 10 normal runs and three race-enabled runs.

A matched `database/sql` prepared-statement FULL-sync insert benchmark (`-benchtime=1s -count=3`) measured `37.5-47.0 us/op` for the custom driver and `35.1-48.5 us/op` for mattn/go-sqlite3 on the same Apple M2 Pro. The ranges overlap and the results do not establish a repeatable winner; retain the direct API benchmark separately and use repeated runs when evaluating further FULL-sync changes.

A follow-up matched prepared FULL-sync run removed per-iteration `fmt.Sprintf` work from both engines. At `-benchtime=1s -count=3`, custom measured `33.7-42.6 us/op`, `875-913 B/op`, and 15 allocations; mattn/go-sqlite3 measured `30.9-34.7 us/op`, `215 B/op`, and 5 allocations. A custom CPU profile attributed over 96% of samples to syscall wrappers, consistent with the durability sync dominating this workload. The custom prepared path still trails on latency and allocations, so keep it as an open optimization target and rerun with alternating engine order or a controlled benchmark host before making a winner claim.

The latest 100-row SyncOff insert-batch microbenchmark is faster than SQLite (`92.6-96.6 us/op` custom, `99.0-99.4 us/op` SQLite). Geometric row reservation and packed B-tree key/value storage reduced custom allocations from about `96 KB/op, 1035 allocs/op` to `69.7-70.7 KB/op, 829 allocs/op` (SQLite: `26.4 KB/op, 919 allocs/op`). The append optimization preserves snapshot isolation; its regression test passed 20 normal runs and 10 race-enabled runs.

A matched prepared `database/sql` concurrent FULL-sync workload with four connections, 95% point reads, and 5% updates measured `3.93-4.85 us/op`, `860 B/op`, and 20 allocations for the custom driver versus `4.47-5.54 us/op`, `648 B/op`, and 19 allocations for mattn/go-sqlite3 (`-benchtime=1s -count=3`). The custom driver shares one engine per file path across pooled connections, warms and retains all four connections before timing, and avoids a redundant argument-slice allocation and column-slice copy. The ranges overlap; allocations remain slightly higher. The earlier slower result included custom connection opens and WAL replay inside the timed section because the default idle pool held only two connections.

A rerun of the matched prepared FULL-sync insert benchmark after stack-backed driver arguments, a reusable WAL append header, skipping normalization for canonical strings, per-table positioned-value storage, and an integer-key B-tree arena measured `31.4-33.4 us/op`, `789-816 B/op`, and 8 allocations for the custom driver. mattn/go-sqlite3 measured `31.3-36.4 us/op`, `215 B/op`, and 5 allocations in this run. Latency ranges overlap; the custom path is down to 8 allocations but still allocates more bytes and objects than mattn.

After moving simple-select projector construction to an 8-column stack buffer with heap fallback, a matched direct API point-lookup run measured `1.08-1.66 us/op`, `406 B/op`, and 6 allocations for the custom engine versus `2.35-2.42 us/op`, `624 B/op`, and 22 allocations for mattn/go-sqlite3 (`-benchtime=1s -count=3`). The concurrent FULL prepared benchmark remains slower, so the direct API result does not establish a prepared-driver win. Full tests and race tests passed after this change.

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
- [x] Add fuzz coverage for the FTS5 MATCH expression parser.
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
- [x] Register the custom `database/sql` driver as `cqlite`.
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

## P9: SQL Feature Completeness (2026-10-01)

The P5 milestone claimed broad SQLite SQL compatibility, but a live probe against
`/usr/bin/sqlite3` found that several headline features were either missing or,
worse, silently returning wrong answers. This milestone records the corrective
work and the defects found along the way.

### Correctness defects found and fixed

These were silent wrong answers, not missing features. Each has a regression
test in the parity suites.

- [x] Compound SELECT (`UNION`, `UNION ALL`, `INTERSECT`, `EXCEPT`) had **no
      implementation at all**: only the left arm was returned. Implemented with
      SQLite's exact semantics — `UNION` de-duplicates with the *last* arm's
      value winning and the result sorted; `INTERSECT`/`EXCEPT` keep the left
      arm's value and are always distinct; `UNION ALL` concatenates in arm
      order.
- [x] `SELECT DISTINCT` was silently ignored. Implemented as first-occurrence
      wins in input order, which is what SQLite does (unlike compound `UNION`).
- [x] Rows were **lost on reopen**. The string-driven fast select path answered
      before lazily-loaded checkpoint rows were materialised, so a database that
      had been checkpointed reopened with an empty table. The fast path now
      declines when rows are still deferred.
- [x] `sum()` returned `REAL` where SQLite returns `INTEGER`; `1/2` returned
      `0.5` instead of `0`; division by zero raised instead of returning NULL.
      Arithmetic now follows SQLite's integer/real promotion rules.
- [x] `MATCH` was **silently dropped** by the parser, so `WHERE ft MATCH 'x'`
      matched every row. Added as an infix operator.
- [x] A data race: schema inspection parsed SQL outside `parseMu`, sharing the
      parser's arena across goroutines.
- [x] `Open(":memory:")` created a real directory named `:memory:` in the
      working directory. Memory databases are now ephemeral and leave no
      filesystem entry.
- [x] The join-and-aggregate fast path returned `REAL` for integer-only
      `sum()`, sorted numeric text incorrectly, and matched NULL join keys. It
      now preserves SQLite sum types and overflow errors, applies text
      collations and storage-class ordering, skips NULL joins, and recognizes
      SQLite's numeric prefixes in text and blob aggregate inputs.
- [x] File-backed `database/sql` connections opened independent engine handles,
      causing redundant WAL replay and a primary-key update replay failure under
      pooled concurrent reads. The driver now shares one ref-counted engine per
      file path, keeps in-memory connections private, and falls back to generic
      execution when replay cannot use the primary-key update fast path.

### Features implemented

- [x] CTEs: `WITH`, multiple CTEs, later CTEs referencing earlier ones, and
      `WITH RECURSIVE` with SQLite's working-table (frontier) semantics.
- [x] Window functions: `row_number`, `rank`, `dense_rank`, `percent_rank`,
      `cume_dist`, `ntile`, `lag`, `lead`, `first_value`, `last_value`,
      `nth_value`, and aggregates over frames, with named `WINDOW` clauses and
      nested aggregates such as `sum(sum(n)) OVER (...)`.
- [x] ~30 scalar functions: `substr`, `replace`, `instr`, `trim`/`ltrim`/`rtrim`,
      `printf`, `quote`, `char`, `unicode`, `hex`/`unhex`, `zeroblob`,
      `randomblob`, `round`, `mod`, `sign`, `iif`, `min`/`max` (scalar),
      `concat`/`concat_ws`, `octet_length`, the math family, `like`, `glob`.
- [x] Date/time family: `date`, `time`, `datetime`, `julianday`, `unixepoch`,
      `strftime`, `timediff`, with SQLite's modifiers including `start of
      month` overflow behaviour and `weekday N`.
- [x] JSON family: `json`, `json_array`, `json_object`, `json_extract`,
      `json_insert`/`replace`/`set`/`remove`, `json_patch`, `json_type`,
      `json_quote`, `json_valid`, `json_error_position`, `json_array_length`.
      Parsing accepts the JSON5 extensions SQLite accepts (unquoted keys,
      single quotes, trailing commas, hex, comments) while `json_valid`
      remains strict RFC 8259. Object key order is preserved.
- [x] Triggers: `CREATE`/`DROP TRIGGER`, `BEFORE`/`AFTER`/`INSTEAD OF`,
      `UPDATE OF`, `WHEN`, `OLD`/`NEW`, multi-statement and nested bodies,
      `SELECT RAISE(IGNORE|FAIL|ABORT|ROLLBACK, msg)`, and `INSERT OR` conflict
      actions. Fast mutation paths decline on trigger-armed tables so no change
      is applied without firing the trigger program.
- [x] `ATTACH`/`DETACH DATABASE` with schema-qualified names and
      `PRAGMA database_list`. Each attached file is an independently persisted
      database. A statement naming a schema that is not attached errors rather
      than silently falling back to `main`.
- [x] Virtual tables and FTS5: `CREATE VIRTUAL TABLE ... USING fts5(...)`,
      inverted index in shadow tables that ride the ordinary WAL/checkpoint
      machinery, `MATCH` with phrases, `AND`/`OR`/`NOT`, prefix `*`, `^`,
      column filters, `NEAR`, plus `rank`, `bm25()`, `highlight()`,
      `snippet()`. Tokenizers: `unicode61`, `porter`, `ascii`/`simple`,
      `trigram`.
- [x] `ALTER TABLE ... RENAME COLUMN` and the `ALTER COLUMN` actions.

### Known limitations (not yet implemented)

- [x] SQLite byte-for-byte file format remains out of scope; files are not
      interchangeable with other SQLite tools.
- [ ] Cross-database writes and atomic transactions across attached files
      remain unsupported. Read-only `SELECT` statements can join tables from
      `main` and attached schemas using isolated per-database snapshots.
- [ ] FTS5 `content=` external-content tables now read live rows from the
      configured table and honor `content_rowid=`; `detail=none/column` and
      contentless tables support scans and MATCH with NULL content columns,
      and reject UPDATE/DELETE as FTS5 requires. `optimize` compacts obsolete
      shadow-table row versions. `prefix=` and `securedelete` are accepted but
      do not tune storage or indexing yet.
- [ ] FTS5 relevance scores (`rank`, `bm25()`) use the standard BM25 formula but
      will not match SQLite's values exactly because the index layout differs;
      row selection matches exactly.
- [ ] `ROWS` and `GROUPS` frames support numeric/unbounded/current-row bounds;
      `RANGE` supports unbounded/current-row and numeric offsets for one ORDER
      BY expression. `EXCLUDE CURRENT ROW`, `GROUP`, `TIES`, and `NO OTHERS` are
      supported for aggregate and value window functions.
- [ ] `SELECT ... FROM ft` without a `MATCH` constraint scans the whole virtual
      table through the generic executor; there is no FTS-specific skip-scan.
- [ ] MySQL wire protocol remains deferred.

### Gates re-verified

- [x] `GOCACHE=$(pwd)/.gocache go test ./...` — green across 11 packages.
- [x] `GOCACHE=$(pwd)/.gocache go test -race ./...` — green.
- [x] sqlite3 parity suites (compound, CTE, window, functions, date/time, JSON,
      triggers, ATTACH, FTS5) against local `/usr/bin/sqlite3`.
- [x] Crash/recovery suites.
- [x] Fuzz suites (`FuzzRowCodecDecode` clean over 527k execs).
