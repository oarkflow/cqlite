# Examples

This folder contains runnable end-to-end SQL samples.

## Run all examples

```bash
go run ./examples
```

The runner will:
- Parse every `.sql` file in `examples/sql/`
- Print parse status and statement count
- Print developer-friendly analysis findings (problem + recommendation)
- Print dialect-aware findings for MySQL/Postgres/SQLite
- Print optimization actions and whether SQL was converted for each dialect
- Convert each sample to MySQL, PostgreSQL, and SQLite SQL

### Comprehensive sample

`examples/sql/10_complete_everything.sql` contains an end-to-end SQL script covering:
- Database/table/index/view DDL
- CTEs, joins, subqueries, unions/intersections
- JSONB field usage + operators
- Insert/upsert (`ON DUPLICATE KEY`, `ON CONFLICT`)
- Update/delete with conditions and limits
- Transactions and `CALL`
- Routine/trigger fallback DDL
- Drop/cleanup statements

### Advanced parser coverage

The newer focused examples cover recently expanded parser support:
- `examples/sql/11_advanced_select_window_lateral.sql` — recursive CTEs, `DISTINCT ON`, qualified `table.*`, `FILTER`, `OVER`, named `WINDOW`, `LATERAL`, `VALUES` table sources, alias column lists, `NULLS FIRST/LAST`, and `FOR UPDATE`.
- `examples/sql/12_dml_returning_and_defaults.sql` — `DEFAULT VALUES`, `INSERT ... SET`, `ON CONFLICT ... WHERE`, DML `RETURNING`, qualified update assignments, and MySQL-style multi-table `DELETE`.
- `examples/sql/13_object_ddl_generated_alter.sql` — generated and identity columns, materialized views, function/sequence object DDL, and richer `ALTER TABLE` commands.
- `examples/sql/15_named_params_and_args.sql` — named bind parameters plus named function/procedure arguments with `=>` and `:=`.

## Add your own examples

1. Add a new `*.sql` file under `examples/sql/`
2. Run `go run ./examples`
