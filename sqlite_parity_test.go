package cqlite

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
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

func TestSQLiteParityForJoinAggregateTextOrderAndCollation(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT COLLATE NOCASE)`,
		`CREATE TABLE items (id INTEGER PRIMARY KEY, group_id INTEGER, score INTEGER)`,
		`INSERT INTO groups VALUES (1, '2'), (2, '10'), (3, 'a'), (4, 'B'), (5, 'null-key')`,
		`INSERT INTO items VALUES (1, 1, 2), (2, 2, 10), (3, 3, 3), (4, 4, 4), (5, NULL, 100), (6, 1, '2text'), (7, 1, 'not numeric')`,
		`CREATE TABLE numeric_groups (id INTEGER PRIMARY KEY, bucket ANY)`,
		`CREATE TABLE numeric_items (id INTEGER PRIMARY KEY, group_id INTEGER, score INTEGER)`,
		`INSERT INTO numeric_groups VALUES (1, 1), (2, 1.0)`,
		`INSERT INTO numeric_items VALUES (1, 1, 2), (2, 2, 3)`,
		`CREATE TABLE sum_inputs (value ANY)`,
		`INSERT INTO sum_inputs VALUES ('2text'), ('1e2tail'), ('1e'), (x'33'), ('not numeric')`,
	}
	queries := []string{
		`SELECT groups.name, count(*), sum(items.score) FROM groups JOIN items ON groups.id = items.group_id GROUP BY groups.name ORDER BY groups.name`,
		`SELECT numeric_groups.bucket, count(*), sum(numeric_items.score) FROM numeric_groups JOIN numeric_items ON numeric_groups.id = numeric_items.group_id GROUP BY numeric_groups.bucket ORDER BY numeric_groups.bucket`,
		`SELECT sum(score), avg(score), total(score) FROM items`,
		`SELECT sum(value), avg(value), total(value) FROM sum_inputs`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("join aggregate text order mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestFastJoinAggregateIntegerSumOverflow(t *testing.T) {
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE items (id INTEGER PRIMARY KEY, group_id INTEGER, score INTEGER)`,
		`INSERT INTO groups VALUES (1, 'one')`,
		`INSERT INTO items VALUES (1, 1, 9223372036854775807), (2, 1, 1)`,
	} {
		if _, err := d.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Query(ctx, `SELECT groups.name, count(*), sum(items.score) FROM groups JOIN items ON groups.id = items.group_id GROUP BY groups.name ORDER BY groups.name`); err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("join aggregate overflow error = %v; want integer overflow", err)
	}
}

func TestSQLiteParityForSQLTransactionCommands(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE tx_items (id INTEGER PRIMARY KEY, msg TEXT)`,
		`BEGIN IMMEDIATE`,
		`INSERT INTO tx_items VALUES (1, 'rolled back')`,
		`ROLLBACK`,
		`BEGIN EXCLUSIVE`,
		`INSERT INTO tx_items VALUES (2, 'kept')`,
		`SAVEPOINT s1`,
		`INSERT INTO tx_items VALUES (3, 'discarded')`,
		`ROLLBACK TO s1`,
		`RELEASE SAVEPOINT s1`,
		`COMMIT`,
	}
	queries := []string{
		`SELECT id, msg FROM tx_items ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("transaction command mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForLeftAndCrossJoins(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE join_users (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE join_orders (id INTEGER PRIMARY KEY, user_id INTEGER, total REAL)`,
		`INSERT INTO join_users VALUES (1, 'Ada')`,
		`INSERT INTO join_users VALUES (2, 'Grace')`,
		`INSERT INTO join_orders VALUES (1, 1, 10.5)`,
	}
	queries := []string{
		`SELECT join_users.name, join_orders.total FROM join_users LEFT JOIN join_orders ON join_users.id = join_orders.user_id ORDER BY join_users.id`,
		`SELECT join_users.name, join_orders.total FROM join_users CROSS JOIN join_orders ORDER BY join_users.id, join_orders.id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("left/cross join mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForInSubqueries(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE sub_users (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE sub_orders (id INTEGER PRIMARY KEY, user_id INTEGER, total REAL)`,
		`INSERT INTO sub_users VALUES (1, 'Ada')`,
		`INSERT INTO sub_users VALUES (2, 'Grace')`,
		`INSERT INTO sub_users VALUES (3, 'Linus')`,
		`INSERT INTO sub_orders VALUES (1, 1, 10.0)`,
		`INSERT INTO sub_orders VALUES (2, 1, 20.0)`,
		`INSERT INTO sub_orders VALUES (3, 2, 5.0)`,
		`UPDATE sub_users SET name = name || '!' WHERE id IN (SELECT user_id FROM sub_orders WHERE total >= 20)`,
		`DELETE FROM sub_orders WHERE user_id NOT IN (SELECT id FROM sub_users WHERE name LIKE 'A%')`,
	}
	queries := []string{
		`SELECT id, name FROM sub_users WHERE id IN (SELECT user_id FROM sub_orders WHERE total >= 10) ORDER BY id`,
		`SELECT id, user_id, total FROM sub_orders ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("IN subquery mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForScalarSubqueries(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE scalar_users (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE scalar_orders (id INTEGER PRIMARY KEY, user_id INTEGER, total REAL)`,
		`INSERT INTO scalar_users VALUES (1, 'Ada')`,
		`INSERT INTO scalar_users VALUES (2, 'Grace')`,
		`INSERT INTO scalar_orders VALUES (1, 1, 10.0)`,
		`INSERT INTO scalar_orders VALUES (2, 1, 20.0)`,
		`INSERT INTO scalar_orders VALUES (3, 2, 5.0)`,
	}
	queries := []string{
		`SELECT (SELECT max(total) FROM scalar_orders)`,
		`SELECT name FROM scalar_users WHERE id = (SELECT user_id FROM scalar_orders WHERE total = 20.0)`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("scalar subquery mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForExistsSubqueries(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE exists_users (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE exists_orders (id INTEGER PRIMARY KEY, user_id INTEGER, total REAL)`,
		`INSERT INTO exists_users VALUES (1, 'Ada')`,
		`INSERT INTO exists_users VALUES (2, 'Grace')`,
		`INSERT INTO exists_orders VALUES (1, 1, 10.0)`,
	}
	queries := []string{
		`SELECT name FROM exists_users WHERE EXISTS (SELECT id FROM exists_orders WHERE total >= 10) ORDER BY id`,
		`SELECT name FROM exists_users WHERE NOT EXISTS (SELECT id FROM exists_orders WHERE total > 99) ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("EXISTS subquery mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForAggregateFunctionsAndHaving(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE agg_items (id INTEGER PRIMARY KEY, bucket INTEGER, score REAL)`,
		`INSERT INTO agg_items VALUES (1, 1, 10.0)`,
		`INSERT INTO agg_items VALUES (2, 1, 20.0)`,
		`INSERT INTO agg_items VALUES (3, 2, 5.0)`,
		`INSERT INTO agg_items VALUES (4, 2, NULL)`,
	}
	queries := []string{
		`SELECT count(*), count(score), sum(score), avg(score), min(score), max(score) FROM agg_items`,
		`SELECT bucket, avg(score), min(score), max(score) FROM agg_items GROUP BY bucket HAVING avg(score) >= 5 ORDER BY bucket`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("aggregate/having mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
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

func TestSQLiteParityForIndexedRangeLookup(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE range_items (id INTEGER PRIMARY KEY, bucket INTEGER, name TEXT)`,
		`CREATE INDEX range_items_bucket_idx ON range_items (bucket)`,
		`INSERT INTO range_items VALUES (1, 1, 'one')`,
		`INSERT INTO range_items VALUES (2, 2, 'two')`,
		`INSERT INTO range_items VALUES (3, 3, 'three')`,
		`INSERT INTO range_items VALUES (4, 4, 'four')`,
	}
	queries := []string{
		`SELECT id, bucket FROM range_items WHERE id >= 2 AND id < 4 ORDER BY id`,
		`SELECT id, bucket FROM range_items WHERE bucket >= 2 AND bucket <= 3 ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("indexed range mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForIndexedOrderByLimit(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE ordered_items (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`,
		`CREATE INDEX ordered_items_score_idx ON ordered_items (score)`,
		`INSERT INTO ordered_items VALUES (1, 'one', 30)`,
		`INSERT INTO ordered_items VALUES (2, 'two', 10)`,
		`INSERT INTO ordered_items VALUES (3, 'three', 20)`,
		`INSERT INTO ordered_items VALUES (4, 'four', 40)`,
	}
	queries := []string{
		`SELECT id, score FROM ordered_items ORDER BY id DESC LIMIT 2`,
		`SELECT id, score FROM ordered_items ORDER BY score LIMIT 3`,
		`SELECT id, score FROM ordered_items WHERE score >= 20 ORDER BY score DESC LIMIT 2`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("indexed order by limit mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
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

func TestSQLiteParityForPragmaIndexInfo(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE pragma_items (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`,
		`CREATE INDEX pragma_items_score_name_idx ON pragma_items (score, name)`,
	}
	queries := []string{
		`PRAGMA index_info(pragma_items_score_name_idx)`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("index_info mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForBuiltInCollations(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE collated_items (id INTEGER PRIMARY KEY, name TEXT COLLATE NOCASE UNIQUE, code TEXT COLLATE RTRIM)`,
		`INSERT INTO collated_items VALUES (1, 'bravo', 'x  ')`,
		`INSERT INTO collated_items VALUES (2, 'Alpha', 'x')`,
		`INSERT INTO collated_items VALUES (3, 'charlie', 'y')`,
	}
	queries := []string{
		`SELECT id, name FROM collated_items WHERE name = 'alpha'`,
		`SELECT name FROM collated_items ORDER BY name`,
		`SELECT id FROM collated_items ORDER BY code`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("collation mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForCheckConstraints(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE checked_items (id INTEGER PRIMARY KEY, score INTEGER CHECK (score >= 0), name TEXT, CHECK (id > 0 AND name IS NOT NULL))`,
		`INSERT INTO checked_items VALUES (1, 0, 'zero')`,
		`INSERT INTO checked_items VALUES (2, NULL, 'unknown')`,
	}
	queries := []string{
		`SELECT id, score, name FROM checked_items ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("check constraint valid-row mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}

	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, stmt := range setup[:1] {
		if _, err := d.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(ctx, `INSERT INTO checked_items VALUES (3, -1, 'bad')`); err == nil {
		t.Fatal("expected custom CHECK failure")
	}
	if _, err := d.Exec(ctx, `INSERT INTO checked_items VALUES (-1, 1, 'bad')`); err == nil {
		t.Fatal("expected custom table CHECK failure")
	}
	dir := t.TempDir()
	persisted, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persisted.Exec(ctx, setup[0]); err != nil {
		t.Fatal(err)
	}
	if err := persisted.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Exec(ctx, `INSERT INTO checked_items VALUES (4, -2, 'bad')`); err == nil {
		t.Fatal("expected persisted custom CHECK failure after reopen")
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
		`SELECT name FROM exprs WHERE name LIKE 'A!_a' ESCAPE '!' ORDER BY id`,
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

func TestSQLiteParityForRandomIndexedDML(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE random_items (id INTEGER PRIMARY KEY, bucket INTEGER, name TEXT)`,
		`CREATE INDEX random_items_bucket_idx ON random_items (bucket)`,
	}
	rng := rand.New(rand.NewSource(7))
	live := make(map[int]bool)
	for i := 0; i < 80; i++ {
		id := rng.Intn(30) + 1
		switch rng.Intn(3) {
		case 0:
			setup = append(setup, fmt.Sprintf(`DELETE FROM random_items WHERE id = %d`, id))
			delete(live, id)
		case 1:
			if live[id] {
				bucket := rng.Intn(5)
				setup = append(setup, fmt.Sprintf(`UPDATE random_items SET bucket = %d, name = 'u%d' WHERE id = %d`, bucket, i, id))
			}
		default:
			bucket := rng.Intn(5)
			if live[id] {
				setup = append(setup, fmt.Sprintf(`UPDATE random_items SET bucket = %d, name = 'n%d' WHERE id = %d`, bucket, i, id))
			} else {
				setup = append(setup, fmt.Sprintf(`INSERT INTO random_items (id, bucket, name) VALUES (%d, %d, 'n%d')`, id, bucket, i))
			}
			live[id] = true
		}
	}
	queries := []string{
		`SELECT id, bucket, name FROM random_items ORDER BY id`,
		`SELECT id, name FROM random_items WHERE bucket = 2 ORDER BY id`,
		`SELECT bucket, count(*) FROM random_items GROUP BY bucket ORDER BY bucket`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("random indexed DML mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForInsertOrReplace(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE replace_items (id INTEGER PRIMARY KEY, code TEXT UNIQUE, name TEXT)`,
		`INSERT INTO replace_items VALUES (1, 'a', 'old')`,
		`INSERT OR REPLACE INTO replace_items VALUES (1, 'a', 'new')`,
		`INSERT OR REPLACE INTO replace_items VALUES (2, 'a', 'by-code')`,
	}
	queries := []string{
		`SELECT id, code, name FROM replace_items ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("insert or replace mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForInsertSelect(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE source_items (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`,
		`CREATE TABLE copied_items (id INTEGER PRIMARY KEY, label TEXT)`,
		`INSERT INTO source_items VALUES (1, 'Ada', 10)`,
		`INSERT INTO source_items VALUES (2, 'Grace', 5)`,
		`INSERT INTO copied_items (id, label) SELECT id + 10, name || ':' || score FROM source_items WHERE score >= 10`,
	}
	queries := []string{
		`SELECT id, label FROM copied_items ORDER BY id`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("insert select mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestSQLiteParityForCreateTableAsSelect(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
	setup := []string{
		`CREATE TABLE ctas_source (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`,
		`INSERT INTO ctas_source VALUES (1, 'Ada', 10)`,
		`INSERT INTO ctas_source VALUES (2, 'Grace', 5)`,
		`CREATE TABLE ctas_copy AS SELECT id, name || ':' || score AS label FROM ctas_source WHERE score >= 10`,
	}
	queries := []string{
		`SELECT id, label FROM ctas_copy ORDER BY id`,
		`PRAGMA table_info(ctas_copy)`,
	}
	custom := runCustomDB(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
		t.Fatalf("create table as select mismatch\ncustom: %#v\nsqlite: %#v", custom, sqlite)
	}
}

func TestExplainQueryPlanAccessPaths(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, stmt := range []string{
		`CREATE TABLE plan_items (id INTEGER PRIMARY KEY, bucket INTEGER, name TEXT)`,
		`CREATE INDEX plan_items_bucket_idx ON plan_items (bucket)`,
		`INSERT INTO plan_items VALUES (1, 2, 'one')`,
	} {
		if _, err := d.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	assertPlanContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items WHERE id = 1`, "USING INTEGER PRIMARY KEY")
	assertPlanContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items WHERE id >= 1`, "USING INTEGER PRIMARY KEY")
	assertPlanContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items WHERE bucket = 2`, "USING INDEX plan_items_bucket_idx")
	assertPlanContains(t, d, `EXPLAIN QUERY PLAN SELECT bucket FROM plan_items WHERE bucket = 2`, "USING COVERING INDEX plan_items_bucket_idx")
	assertPlanContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items WHERE bucket >= 2`, "USING INDEX plan_items_bucket_idx")
	assertPlanContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items WHERE name = 'one'`, "SCAN plan_items")
	assertPlanNotContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items ORDER BY id`, "USE TEMP B-TREE")
	assertPlanNotContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items ORDER BY bucket`, "USE TEMP B-TREE")
	assertPlanAnyContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items ORDER BY name`, "USE TEMP B-TREE")
	assertPlanContains(t, d, `EXPLAIN QUERY PLAN SELECT * FROM plan_items p JOIN plan_items q ON p.bucket = q.bucket`, "NESTED LOOP JOIN")
}

func assertPlanContains(t *testing.T, d *DB, query, want string) {
	t.Helper()
	rows, err := d.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || !strings.Contains(fmt.Sprint(row[3]), want) {
		t.Fatalf("plan for %q = %#v ok=%v, want %q", query, row, ok, want)
	}
}

func assertPlanNotContains(t *testing.T, d *DB, query, unwanted string) {
	t.Helper()
	rows, err := d.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	for {
		row, ok := rows.Next()
		if !ok {
			return
		}
		if strings.Contains(fmt.Sprint(row[3]), unwanted) {
			t.Fatalf("plan for %q contains %q in %#v", query, unwanted, row)
		}
	}
}

func assertPlanAnyContains(t *testing.T, d *DB, query, want string) {
	t.Helper()
	rows, err := d.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	for {
		row, ok := rows.Next()
		if !ok {
			t.Fatalf("plan for %q did not contain %q", query, want)
		}
		if strings.Contains(fmt.Sprint(row[3]), want) {
			return
		}
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
		// A query that matched no rows emits nothing at all, while a query that
		// matched one row holding an empty string emits a single empty line.
		// Only the trailing newline is structural.
		if raw == "" {
			out = append(out, nil)
			continue
		}
		text := strings.TrimSuffix(raw, "\n")
		var result [][]string
		for _, line := range strings.Split(text, "\n") {
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
				out[i] = fmt.Sprintf("%.15g", x)
			}
		case []byte:
			// sqlite3's CLI prints blob payloads raw, so compare them the same
			// way rather than through Go's slice formatting.
			out[i] = string(x)
		default:
			out[i] = fmt.Sprint(x)
		}
	}
	return out
}
