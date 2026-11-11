package parser_test

import (
	"os"
	"path/filepath"
	"testing"

	sqlparser "github.com/oarkflow/sqlite/internal/sqlparser"
)

// FuzzParser tests the parser with random input to ensure it never panics.
// Parse errors are expected and OK; panics are not.
func FuzzParser(f *testing.F) {
	seeds := []string{
		"SELECT * FROM users",
		"SELECT id, name FROM t WHERE id = 1",
		"INSERT INTO t (a) VALUES (1)",
		"UPDATE t SET x = 1",
		"DELETE FROM t WHERE id = 1",
		"CREATE TABLE t (id INT)",
		"DROP TABLE t",
		"ALTER TABLE t ADD COLUMN x INT",
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
		"",
		"SELECT",
		"SELECT FROM",
		"INSERT",
		"CREATE",
		";;;",
		"SELECT 'unterminated",
		"SELECT 1 + 2 * 3 - 4 / 5",
		"SELECT * FROM t WHERE x IN (1,2,3) AND y BETWEEN 1 AND 10",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	for _, path := range exampleSQLPaths() {
		if b, err := os.ReadFile(path); err == nil {
			f.Add(string(b))
		}
	}

	f.Fuzz(func(t *testing.T, sql string) {
		// We don't care about errors, only panics
		sqlparser.ParseStatements(sql)
	})
}

func FuzzParseDocument(f *testing.F) {
	for _, path := range exampleSQLPaths() {
		if b, err := os.ReadFile(path); err == nil {
			f.Add(string(b))
		}
	}
	f.Add("/* keep */ SELECT 1;\nCREATE FUNCTION f AS 'select 1';")
	f.Fuzz(func(t *testing.T, sql string) {
		doc, err := sqlparser.ParseDocumentString(sql)
		if err != nil {
			return
		}
		if doc.SQL() != sql {
			t.Fatalf("document SQL mismatch")
		}
		for _, span := range doc.Statements {
			if span.Start < 0 || span.End < span.Start || int(span.End) > len(sql) {
				t.Fatalf("invalid span: %#v", span)
			}
		}
	})
}

func exampleSQLPaths() []string {
	paths, _ := filepath.Glob("../examples/sql/*.sql")
	return paths
}
