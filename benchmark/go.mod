module github.com/oarkflow/sqlite/benchmark

go 1.26.0

require (
	github.com/mattn/go-sqlite3 v1.14.44
	github.com/oarkflow/sqlite v0.0.0
)

require github.com/oarkflow/sqlparser v0.0.2 // indirect

replace github.com/oarkflow/sqlite => ../
