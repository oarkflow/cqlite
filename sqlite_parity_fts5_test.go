package db

import (
	"context"
	"fmt"
	"testing"
)

func TestSQLiteParityFTS5(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE VIRTUAL TABLE ft USING fts5(body, title)`,
		`INSERT INTO ft(body, title) VALUES ('hello world', 'greeting')`,
		`INSERT INTO ft(body, title) VALUES ('goodbye world', 'farewell')`,
		`INSERT INTO ft(body, title) VALUES ('hello again', 'greeting')`,
		`INSERT INTO ft(body, title) VALUES ('the quick brown fox', 'animals')`,
	}
	queries := []string{
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hello'`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'world' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hello world' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hello OR goodbye' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hello NOT goodbye' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE body MATCH 'hello' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE title MATCH 'greeting' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hel*' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'quick fox' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH '"hello again"' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'NEAR(hello world, 1)' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'NEAR(hello world, 10)' ORDER BY rowid`,
		`SELECT rowid, body FROM ft`,
		`SELECT count(*) FROM ft`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hello' ORDER BY rowid DESC`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hello' ORDER BY rowid LIMIT 1`,
		`SELECT rowid, highlight(ft, 0, '[', ']') FROM ft WHERE ft MATCH 'hello' ORDER BY rowid`,
		`SELECT rowid, highlight(ft, 1, '<b>', '</b>') FROM ft WHERE ft MATCH 'greeting' ORDER BY rowid`,
		`SELECT rowid, snippet(ft, 0, '[', ']', '...', 5) FROM ft WHERE ft MATCH 'world' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'hello' AND title MATCH 'greeting' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'nothing' ORDER BY rowid`,
	}
	assertFTSParity(t, setup, queries)
}

func TestSQLiteParityFTS5DML(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE VIRTUAL TABLE ft USING fts5(body)`,
		`INSERT INTO ft(body) VALUES ('alpha beta gamma')`,
		`INSERT INTO ft(body) VALUES ('beta gamma delta')`,
	}
	queries := []string{
		`SELECT rowid, body FROM ft WHERE ft MATCH 'beta' ORDER BY rowid`,
		`UPDATE ft SET body = 'alpha beta' WHERE rowid = 1`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'gamma' ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'alpha' ORDER BY rowid`,
		`DELETE FROM ft WHERE rowid = 2`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'delta' ORDER BY rowid`,
		`INSERT INTO ft(ft) VALUES('optimize')`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'alpha' ORDER BY rowid`,
		`SELECT rowid, body FROM ft`,
	}
	assertFTSParity(t, setup, queries)
}

func TestSQLiteParityFTS5ExternalContent(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE docs (docid INTEGER PRIMARY KEY, title TEXT, body TEXT)`,
		`INSERT INTO docs VALUES (1, 'alpha title', 'the live external text')`,
		`INSERT INTO docs VALUES (2, 'beta title', 'another live document')`,
		`CREATE VIRTUAL TABLE ft USING fts5(title, body, content='docs', content_rowid='docid')`,
		`INSERT INTO ft(title, body) VALUES ('ignored title', 'external text')`,
		`INSERT INTO ft(title, body) VALUES ('ignored title', 'live document')`,
	}
	queries := []string{
		`SELECT rowid, title, body FROM ft ORDER BY rowid`,
		`UPDATE docs SET title = 'updated title', body = 'current source' WHERE docid = 1`,
		`SELECT rowid, title, body FROM ft ORDER BY rowid`,
		`SELECT rowid, title, body FROM ft WHERE ft MATCH 'external' ORDER BY rowid`,
		`SELECT rowid, title FROM ft WHERE ft MATCH 'live' ORDER BY rowid`,
	}
	assertFTSParity(t, setup, queries)
}

func TestSQLiteParityFTS5Contentless(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE VIRTUAL TABLE ft USING fts5(body, content='')`,
		`INSERT INTO ft(body) VALUES ('hidden searchable phrase')`,
		`INSERT INTO ft(body) VALUES ('another hidden row')`,
	}
	queries := []string{
		`SELECT rowid, body FROM ft ORDER BY rowid`,
		`SELECT rowid, body FROM ft WHERE ft MATCH 'searchable' ORDER BY rowid`,
		`SELECT count(*) FROM ft`,
		`DELETE FROM ft WHERE rowid = 1`,
		`UPDATE ft SET body = 'replacement' WHERE rowid = 2`,
		`SELECT rowid, body FROM ft ORDER BY rowid`,
	}
	assertFTSParity(t, setup, queries)
}

func TestFTS5OptimizeCompactsShadowVersions(t *testing.T) {
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE VIRTUAL TABLE ft USING fts5(body)`,
		`INSERT INTO ft(body) VALUES ('old search term')`,
		`UPDATE ft SET body = 'new search term' WHERE rowid = 1`,
		`DELETE FROM ft WHERE rowid = 1`,
		`INSERT INTO ft(body) VALUES ('current searchable row')`,
	} {
		if _, err := d.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	d.mu.RLock()
	vt := d.state.Catalog.VirtualTables["ft"]
	obsolete := 0
	for _, name := range vt.Shadows {
		data := d.state.Tables[name]
		for _, row := range data.Rows {
			if row.Deleted != 0 {
				obsolete++
			}
		}
	}
	d.mu.RUnlock()
	if obsolete == 0 {
		t.Fatal("setup did not leave obsolete FTS shadow versions")
	}
	if _, err := d.Exec(ctx, `INSERT INTO ft(ft) VALUES('optimize')`); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	d.mu.RLock()
	for _, name := range vt.Shadows {
		data := d.state.Tables[name]
		if len(data.Pending) != 0 {
			d.mu.RUnlock()
			t.Fatalf("%s retains %d pending shadow rows after optimize", name, len(data.Pending))
		}
		for _, row := range data.Rows {
			if row.Deleted != 0 {
				d.mu.RUnlock()
				t.Fatalf("%s retains tombstoned row %d after optimize", name, row.RowID)
			}
		}
	}
	d.mu.RUnlock()
	rows, err := d.Query(ctx, `SELECT rowid, body FROM ft WHERE ft MATCH 'searchable'`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(2) || row[1] != "current searchable row" {
		t.Fatalf("post-optimize row = %#v, ok=%v", row, ok)
	}
}

func TestFTS5TransactionInsertVisibleAfterCommit(t *testing.T) {
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.Exec(ctx, `CREATE VIRTUAL TABLE ft USING fts5(body)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"common document token-0001", "common document token-0002 needle"} {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO ft(body) VALUES ('%s')`, body)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT rowid FROM ft WHERE ft MATCH 'needle'`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(2) {
		t.Fatalf("committed FTS match = %#v, ok=%v", row, ok)
	}
}

func TestSQLiteParityFTS5Tokenize(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE VIRTUAL TABLE p USING fts5(body, tokenize='porter')`,
		`INSERT INTO p(body) VALUES ('running runners ran')`,
		`INSERT INTO p(body) VALUES ('compute computer computing')`,
		`CREATE VIRTUAL TABLE tr USING fts5(body, tokenize='trigram')`,
		`INSERT INTO tr(body) VALUES ('hello world')`,
		`INSERT INTO tr(body) VALUES ('yellow moon')`,
	}
	queries := []string{
		`SELECT rowid, body FROM p WHERE p MATCH 'run' ORDER BY rowid`,
		`SELECT rowid, body FROM p WHERE p MATCH 'running' ORDER BY rowid`,
		`SELECT rowid, body FROM p WHERE p MATCH 'comput' ORDER BY rowid`,
		`SELECT rowid, body FROM tr WHERE tr MATCH 'ell' ORDER BY rowid`,
		`SELECT rowid, body FROM tr WHERE tr MATCH 'hello' ORDER BY rowid`,
	}
	assertFTSParity(t, setup, queries)
}

// assertFTSParity compares FTS5 results with sqlite3. Relevance scores are
// excluded because they depend on index internals; row selection and text
// results are compared exactly.
func assertFTSParity(t *testing.T, setup, statements []string) {
	t.Helper()
	custom := runCustomMixedErr(t, setup, statements)
	sqlite := runSQLiteMixedErr(t, setup, statements)
	for i, q := range statements {
		if got, want := formatParity(custom[i]), formatParity(sqlite[i]); got != want {
			t.Errorf("query %d mismatch: %s\n  custom: %s\n  sqlite: %s", i, q, got, want)
		}
	}
}
