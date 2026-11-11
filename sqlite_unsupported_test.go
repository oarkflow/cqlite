package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSQLiteUnsupportedFeatureAllowlist pins the statements the engine still
// rejects. Anything listed here must fail loudly rather than return wrong
// answers. Features that become supported move to the formerly-unsupported
// suite below.
func TestSQLiteUnsupportedFeatureAllowlist(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	allowlist := []struct {
		name string
		sql  string
	}{
		// NOTE: test SQL must never name a relative path. A leaked database
		// file in the repository is a bug in the test, not the engine.
		{name: "unknown virtual table module", sql: `CREATE VIRTUAL TABLE v USING nosuchmodule(a)`},
		{name: "trigger on missing table", sql: `CREATE TRIGGER tr AFTER INSERT ON missing BEGIN SELECT 1; END`},
	}
	for _, item := range allowlist {
		t.Run(item.name, func(t *testing.T) {
			_, err := d.Exec(ctx, item.sql)
			if err == nil {
				t.Fatalf("unsupported feature %q unexpectedly succeeded", item.name)
			}
		})
	}
}

func TestUnsupportedWindowFrameFailsExplicitly(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE frame_values (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO frame_values VALUES (1, 10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Query(ctx, `SELECT sum(value) OVER (ORDER BY id, value RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM frame_values`); err == nil || !strings.Contains(err.Error(), "RANGE offset frames require exactly one ORDER BY expression") {
		t.Fatalf("RANGE frame error = %v; want explicit invalid-offset-frame error", err)
	}
}

// TestSQLiteSupportedVirtualTables pins the virtual-table surface that must
// keep working.
func TestSQLiteSupportedVirtualTables(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	steps := []string{
		`CREATE VIRTUAL TABLE ft USING fts5(body, title)`,
		`INSERT INTO ft(body, title) VALUES ('hello world', 'greeting')`,
		`INSERT INTO ft(body, title) VALUES ('goodbye world', 'farewell')`,
	}
	for _, q := range steps {
		if _, err := d.Exec(ctx, q); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	checks := []struct {
		q    string
		want int
	}{
		{`SELECT rowid FROM ft WHERE ft MATCH 'hello'`, 1},
		{`SELECT rowid FROM ft WHERE ft MATCH 'world'`, 2},
		{`SELECT rowid FROM ft WHERE ft MATCH 'nothing'`, 0},
		{`SELECT rowid FROM ft`, 2},
		{`SELECT count(*) FROM ft`, 1},
	}
	for _, c := range checks {
		rows, err := d.Query(ctx, c.q)
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		n := 0
		for {
			if _, ok := rows.Next(); !ok {
				break
			}
			n++
		}
		if c.q == `SELECT count(*) FROM ft` {
			continue
		}
		if n != c.want {
			t.Fatalf("%q returned %d rows, want %d", c.q, n, c.want)
		}
	}
}

// TestSQLiteFormerlyUnsupportedFeatures guards against regressions in features
// that used to be rejected. If any of these start failing again the engine has
// lost SQL compatibility.
func TestSQLiteFormerlyUnsupportedFeatures(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER)`,
		`INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)`,
	} {
		if _, err := d.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	supported := []string{
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 3) SELECT x FROM c`,
		`WITH t2 AS (SELECT id FROM t) SELECT * FROM t2`,
		`SELECT row_number() OVER (ORDER BY id) FROM t`,
		`SELECT sum(n) OVER (PARTITION BY id) FROM t`,
		`SELECT id FROM t UNION SELECT id FROM t`,
		`SELECT DISTINCT n FROM t`,
	}
	for _, q := range supported {
		t.Run(q, func(t *testing.T) {
			rows, err := d.Query(ctx, q)
			if err != nil {
				t.Fatalf("supported feature %q failed: %v", q, err)
			}
			if _, ok := rows.Next(); !ok {
				t.Fatalf("supported feature %q returned no rows", q)
			}
		})
	}
}

// TestSQLiteAttachAndMemoryNames pins two behaviours that must never write a
// database into the caller's working directory: ":memory:" databases are
// ephemeral, and nothing is created for them on disk.
func TestSQLiteAttachAndMemoryNames(t *testing.T) {
	ctx := context.Background()
	before, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := map[string]bool{}
	for _, e := range before {
		beforeNames[e.Name()] = true
	}

	mem, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatalf("Open(:memory:) failed: %v", err)
	}
	if _, err := mem.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Exec(ctx, `INSERT INTO t VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
	rows, err := mem.Query(ctx, `SELECT v FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != "x" {
		t.Fatalf("memory database lost its rows: %v ok=%v", row, ok)
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(":memory:"); err == nil {
		t.Fatal("Open(\":memory:\") created a filesystem entry named :memory:")
	}

	// ATTACH of an in-memory database must not create a directory either.
	d, err := Open(filepath.Join(t.TempDir(), "main.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `ATTACH DATABASE ':memory:' AS mem`); err != nil {
		t.Fatalf("ATTACH :memory: failed: %v", err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE mem.t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after {
		if beforeNames[e.Name()] {
			continue
		}
		t.Fatalf("test created %q in the working directory", e.Name())
	}
}
