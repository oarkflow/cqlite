# Complex Query Examples

This directory contains runnable examples for `github.com/oarkflow/cqlite` plus a dialect corpus for adapter authors.

## Run

```sh
go run ./examples/complex_queries
```

The example persists single-file databases under `examples/complex_queries/data/`.
Each run refreshes only the demo seed rows, so the `.db` files remain on disk and reruns stay deterministic.

## Files

- `main.go` is a runnable Go example using the embedded API and the `database/sql` driver.
- `queries.sql` contains SQLite-compatible query shapes demonstrated by `main.go`.
- `dialect_corpus.sql` preserves richer PostgreSQL/MySQL-style parameter examples such as `RETURNING`, `CALL`, `CREATE MATERIALIZED VIEW`, `INSERT ... SET`, and `make_interval(...)`.

## Parameter Notes

- `?` binds the next positional argument.
- `:name`, `@name`, and `$name` bind named arguments.
- Named arguments can be supplied as `cqlite.Named("name", value)`, `map[string]any`, any map with string keys, or a struct/pointer-to-struct.
- Struct fields bind by exported field name, snake_case field name, and `db`, `sql`, or `json` tag names.
- `$1` is treated as a named parameter named `1`; pass `cqlite.Named("1", value)` when using the embedded API.
- `INSERT ... VALUES :row` expands one map or struct into the explicit insert columns.
- `INSERT ... VALUES :rows` expands a slice or array of maps/structs into multiple rows.
- SQLite-style examples use a follow-up `SELECT` where other dialects would use `RETURNING`.
