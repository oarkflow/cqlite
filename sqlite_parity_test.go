package db

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestSQLiteParityForV1Statements(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}

	setup := []string{
		`CREATE TABLE docs (id INT PRIMARY KEY, title TEXT NOT NULL, score REAL, meta JSON)`,
		`INSERT INTO docs (id, title, score, meta) VALUES (1, 'alpha', 1.5, '{"tag":"a"}')`,
		`INSERT INTO docs (id, title, score, meta) VALUES (2, 'beta', 2.25, '{"tag":"b"}')`,
		`UPDATE docs SET title = 'beta-prime', score = 3.5 WHERE id = 2`,
		`DELETE FROM docs WHERE id = 1`,
		`INSERT INTO docs (id, title, score, meta) VALUES (3, 'gamma', 4.75, '{"tag":"g"}')`,
	}
	queries := []string{
		`SELECT id, title, score, meta FROM docs`,
		`SELECT title FROM docs WHERE id = 2`,
		`SELECT id, score FROM docs WHERE score >= 3.5`,
	}

	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("result mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityUsesTypeAffinity(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE strict_docs (id INT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO strict_docs (id) VALUES ('123')`); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT id FROM strict_docs`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(123) {
		t.Fatalf("custom affinity result = %#v ok=%v", row, ok)
	}

	sqliteDB := t.TempDir() + "/sqlite.db"
	runSQLiteScript(t, sqliteDB, []string{
		`CREATE TABLE strict_docs (id INT PRIMARY KEY)`,
		`INSERT INTO strict_docs (id) VALUES ('123')`,
	})
	got := runSQLiteQuery(t, sqliteDB, `SELECT id FROM strict_docs`)
	if got != "123\n" {
		t.Fatalf("sqlite dynamic typing result = %q", got)
	}
}

func TestSQLiteParityForDeclaredDataTypes(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE values_by_type (
			id INTEGER PRIMARY KEY,
			i INT,
			bi BIGINT,
			r REAL,
			n NUMERIC,
			t TEXT,
			bl BLOB,
			bo BOOLEAN,
			ts TIMESTAMP,
			j JSON,
			empty ANY
		)`,
		`INSERT INTO values_by_type (id, i, bi, r, n, t, bl, bo, ts, j, empty)
		 VALUES (1, '42', 9007199254740991, 3.25, '7.5', 99, x'6869', TRUE, '2026-05-29T12:00:00Z', '{"ok":true}', NULL)`,
	}
	queries := []string{
		`SELECT typeof(i), i, typeof(bi), bi, typeof(r), r FROM values_by_type`,
		`SELECT typeof(n), n, typeof(t), t, typeof(bl), hex(bl) FROM values_by_type`,
		`SELECT typeof(bo), bo, typeof(ts), ts, typeof(j), json_valid(j), typeof(empty) FROM values_by_type`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("type result mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForStrictTablesAndAnyType(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE strict_values (id INTEGER PRIMARY KEY, raw ANY, label TEXT) STRICT`,
		`INSERT INTO strict_values (id, raw, label) VALUES (1, '000123', 456)`,
	}
	queries := []string{
		`SELECT typeof(raw), raw, typeof(label), label FROM strict_values`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("strict result mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForJoinsOrderingGroupingAndPragmas(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`PRAGMA user_version = 7`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER, total REAL)`,
		`INSERT INTO users VALUES (1, 'Ada')`,
		`INSERT INTO users VALUES (2, 'Grace')`,
		`INSERT INTO orders VALUES (10, 1, 12.5)`,
		`INSERT INTO orders VALUES (11, 1, 7.5)`,
		`INSERT INTO orders VALUES (12, 2, 20.0)`,
		`CREATE INDEX orders_user_idx ON orders (user_id)`,
		`ALTER TABLE users ADD COLUMN city TEXT DEFAULT 'unknown'`,
	}
	queries := []string{
		`PRAGMA user_version`,
		`PRAGMA table_info(users)`,
		`PRAGMA index_list(orders)`,
		`SELECT users.name, orders.total FROM users JOIN orders ON users.id = orders.user_id ORDER BY orders.total DESC LIMIT 2`,
		`SELECT users.name, count(*), sum(total) FROM users JOIN orders ON users.id = orders.user_id GROUP BY users.name ORDER BY users.name`,
		`SELECT name, city FROM users ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("feature result mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForForeignKeysAndQueryPlan(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE parents (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id), name TEXT)`,
		`CREATE INDEX children_parent_idx ON children (parent_id)`,
		`INSERT INTO parents VALUES (1, 'p1')`,
		`INSERT INTO children VALUES (10, 1, 'c1')`,
	}
	queries := []string{
		`SELECT children.name, parents.name FROM children JOIN parents ON children.parent_id = parents.id WHERE parent_id = 1`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("foreign key result mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}

	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, stmt := range setup {
		if _, err := d.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	pragmaRows, err := d.Query(ctx, `PRAGMA foreign_keys`)
	if err != nil {
		t.Fatal(err)
	}
	pragmaRow, ok := pragmaRows.Next()
	if !ok || pragmaRow[0] != int64(1) {
		t.Fatalf("foreign_keys pragma = %#v ok=%v", pragmaRow, ok)
	}
	if _, err := d.Exec(ctx, `INSERT INTO children VALUES (11, 999, 'bad')`); err == nil {
		t.Fatal("expected foreign key violation")
	}
	rows, err := d.Query(ctx, `EXPLAIN QUERY PLAN SELECT * FROM children WHERE parent_id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || !strings.Contains(fmt.Sprint(row[3]), "USING INDEX children_parent_idx") {
		t.Fatalf("unexpected query plan row: %#v ok=%v", row, ok)
	}
}

func TestSQLiteParityForIndexedEqualityLookup(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE lookup (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`,
		`CREATE INDEX lookup_name_idx ON lookup (name)`,
		`INSERT INTO lookup VALUES (1, 'Ada', 10)`,
		`INSERT INTO lookup VALUES (2, 'Grace', 20)`,
		`INSERT INTO lookup VALUES (3, 'Ada', 30)`,
	}
	queries := []string{
		`SELECT id, score FROM lookup WHERE name = 'Ada' ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("indexed lookup mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForForeignKeyActions(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE parents (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cascade_children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id) ON DELETE CASCADE)`,
		`CREATE TABLE null_children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id) ON DELETE SET NULL)`,
		`CREATE TABLE default_children (id INTEGER PRIMARY KEY, parent_id INTEGER DEFAULT 0 REFERENCES parents(id) ON DELETE SET DEFAULT)`,
		`INSERT INTO parents VALUES (0)`,
		`INSERT INTO parents VALUES (1)`,
		`INSERT INTO cascade_children VALUES (1, 1)`,
		`INSERT INTO null_children VALUES (1, 1)`,
		`INSERT INTO default_children VALUES (1, 1)`,
		`DELETE FROM parents WHERE id = 1`,
	}
	queries := []string{
		`PRAGMA foreign_key_list(cascade_children)`,
		`SELECT count(*) FROM cascade_children`,
		`SELECT parent_id FROM null_children`,
		`SELECT parent_id FROM default_children`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("foreign key action mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForForeignKeyUpdateActions(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE parents (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cascade_children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id) ON UPDATE CASCADE)`,
		`CREATE TABLE null_children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id) ON UPDATE SET NULL)`,
		`CREATE TABLE default_children (id INTEGER PRIMARY KEY, parent_id INTEGER DEFAULT 0 REFERENCES parents(id) ON UPDATE SET DEFAULT)`,
		`INSERT INTO parents VALUES (0)`,
		`INSERT INTO parents VALUES (1)`,
		`INSERT INTO cascade_children VALUES (1, 1)`,
		`INSERT INTO null_children VALUES (1, 1)`,
		`INSERT INTO default_children VALUES (1, 1)`,
		`UPDATE parents SET id = 2 WHERE id = 1`,
	}
	queries := []string{
		`SELECT parent_id FROM cascade_children`,
		`SELECT parent_id FROM null_children`,
		`SELECT parent_id FROM default_children`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("foreign key update action mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForPartialExpressionIndexMetadata(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT, active INTEGER)`,
		`CREATE INDEX people_lower_active_idx ON people (lower(name)) WHERE active = 1`,
	}
	queries := []string{
		`PRAGMA index_list(people)`,
		`SELECT type, name, tbl_name, sql FROM sqlite_schema WHERE type = 'index'`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("partial expression index metadata mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForExpressionSemantics(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE exprs (id INTEGER PRIMARY KEY, name TEXT, n INTEGER, r REAL, maybe TEXT)`,
		`INSERT INTO exprs VALUES (1, 'Ada', 7, 2.5, NULL)`,
		`INSERT INTO exprs VALUES (2, 'Grace', 10, 4.0, 'x')`,
	}
	queries := []string{
		`SELECT name || ':' || n, n % 4, CASE WHEN n BETWEEN 5 AND 8 THEN 'mid' ELSE 'hi' END FROM exprs ORDER BY id`,
		`SELECT name FROM exprs WHERE n IN (7, 9) OR name LIKE 'Gra%' ORDER BY id`,
		`SELECT ifnull(maybe, 'fallback'), coalesce(maybe, name), nullif(name, 'Ada') FROM exprs ORDER BY id`,
		`SELECT lower(name), upper(name), length(name), abs(-r) FROM exprs ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("expression mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForRowIDTables(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE rowids (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO rowids (name) VALUES ('auto')`,
		`INSERT INTO rowids (id, name) VALUES (10, 'manual')`,
		`INSERT INTO rowids (name) VALUES ('after')`,
	}
	queries := []string{
		`SELECT rowid, _rowid_, oid, id, name FROM rowids ORDER BY rowid`,
		`SELECT name FROM rowids ORDER BY rowid DESC LIMIT 2`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("rowid mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForAlterDropColumn(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE alter_me (id INTEGER PRIMARY KEY, keep TEXT, drop_me TEXT)`,
		`INSERT INTO alter_me VALUES (1, 'yes', 'gone')`,
		`ALTER TABLE alter_me DROP COLUMN drop_me`,
	}
	queries := []string{
		`PRAGMA table_info(alter_me)`,
		`SELECT id, keep FROM alter_me`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("alter drop column mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func runCustomDB(t *testing.T, setup, queries []string) [][][]string {
	t.Helper()
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, stmt := range setup {
		if _, err := d.Exec(ctx, stmt); err != nil {
			t.Fatalf("custom exec %q: %v", stmt, err)
		}
	}
	var out [][][]string
	for _, query := range queries {
		rows, err := d.Query(ctx, query)
		if err != nil {
			t.Fatalf("custom query %q: %v", query, err)
		}
		var result [][]string
		for {
			row, ok := rows.Next()
			if !ok {
				break
			}
			result = append(result, normalizeRow(row))
		}
		out = append(out, result)
	}
	return out
}

func runSQLite(t *testing.T, setup, queries []string) [][][]string {
	t.Helper()
	dbPath := t.TempDir() + "/sqlite.db"
	runSQLiteScript(t, dbPath, setup)
	var out [][][]string
	for _, query := range queries {
		raw := runSQLiteQuery(t, dbPath, query)
		var result [][]string
		for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
			if line == "" {
				continue
			}
			result = append(result, strings.Split(line, "\t"))
		}
		out = append(out, result)
	}
	return out
}

func runSQLiteScript(t *testing.T, dbPath string, statements []string) {
	t.Helper()
	script := strings.Join(statements, ";\n")
	if strings.TrimSpace(script) != "" {
		script += ";\n"
	}
	cmd := exec.Command("sqlite3", "-batch", dbPath)
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sqlite exec script: %v\n%s\nscript:\n%s", err, stderr.String(), script)
	}
}

func runSQLiteQuery(t *testing.T, dbPath, query string) string {
	t.Helper()
	cmd := exec.Command("sqlite3", "-batch", "-noheader", "-separator", "\t", "-nullvalue", "NULL", dbPath, query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sqlite query %q: %v\n%s", query, err, stderr.String())
	}
	return stdout.String()
}

func normalizeRow(row []any) []string {
	out := make([]string, len(row))
	for i, v := range row {
		switch x := v.(type) {
		case nil:
			out[i] = "NULL"
		case bool:
			if x {
				out[i] = "1"
			} else {
				out[i] = "0"
			}
		case float64:
			if x == float64(int64(x)) {
				out[i] = fmt.Sprintf("%.1f", x)
			} else {
				out[i] = fmt.Sprint(x)
			}
		default:
			out[i] = fmt.Sprint(x)
		}
	}
	return out
}
