# cQlite examples

Runnable Go examples for the embedded engine and its `database/sql` driver.
Run commands from the repository root with Go 1.26 or newer:

```sh
go run ./examples/quickstart
go run ./examples/transactions
go run ./examples/data_types
go run ./examples/database_sql
go run ./examples/concurrency
go run ./examples/sql_features
go run ./examples/complex_queries
```

All examples except `complex_queries` use private temporary databases, so they
can be rerun without cleanup. `complex_queries` demonstrates a larger
persistent application schema and stores its demo files in
`examples/complex_queries/data/`.

## Example guide

- **quickstart** — open and close a database, create a schema, bind positional
  and named parameters, query rows, and checkpoint a file-backed database.
- **transactions** — commit a group of changes atomically and roll back a
  second transaction.
- **data_types** — bind integers, floating-point values, text, blobs, booleans,
  NULL, timestamps, JSON, and Go structs; inspect the returned values.
- **database_sql** — open the `cqlite` driver with `database/sql`, prepared statements, `sql.Named`,
  transactions, `QueryRow`, and `Scan`.
- **concurrency** — share one `*DB` across goroutines, mix reads and writes,
  and stop work through a context.
- **sql_features** — run CTE and window queries, JSON extraction, a trigger,
  and FTS5 full-text search.
- **complex_queries** — see joins, aggregates, subqueries, views, indexes,
  `EXPLAIN QUERY PLAN`, struct/map row binding, and the driver in one application.

## Basic API

The embedded API accepts a context and variadic bind arguments:

```go
db, err := cqlite.Open("app.db", cqlite.Options{})
if err != nil { return err }
defer db.Close()

_, err = db.Exec(ctx, `INSERT INTO users(name) VALUES (:name)`,
    cqlite.Named("name", "Ada"))
if err != nil { return err }
rows, err := db.Query(ctx, `SELECT id, name FROM users WHERE id = ?`, 1)
```

Use `?` for positional arguments and `:name`, `@name`, or `$name` for named
arguments. The embedded API accepts `cqlite.Named`, maps, and structs. The
`database/sql` driver accepts the standard `sql.Named` arguments.

## Durability and file layout

`cqlite.Open(path, cqlite.Options{})` uses FULL synchronization by default.
Choose `SyncNormal` or `SyncOff` only when their durability/performance tradeoff
fits the application. `:memory:` and an empty path create private temporary
databases that are removed on close. File-backed databases recover committed
changes from the write-ahead log; call `Checkpoint(ctx)` to checkpoint WAL data.

## Errors and contexts

Check every returned error, including `Close`, `Commit`, and `Rollback` errors.
Pass a deadline or cancellable context to operations that may wait on another
writer. `Options.BusyTimeout` can bound waiting for a writer in another process;
the operation context can cancel that wait sooner.

The engine implements a substantial SQLite-compatible SQL subset. Consult
[`../Tasks.md`](../Tasks.md) for known compatibility boundaries and unsupported
features; SQLite file-format compatibility is not a goal.
