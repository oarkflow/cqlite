package db

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/sujit/sqlite/internal/page"
)

func TestSQLStrictTypesAndPersistence(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, active BOOLEAN, profile JSON)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO users (id, name, active, profile) VALUES (?, ?, ?, ?)`, int64(1), "Ada", true, `{"lang":"go"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO users (id, name) VALUES (?, ?)`, "2", "Grace"); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT id, name, active, profile FROM users WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok {
		t.Fatal("missing row")
	}
	if row[0] != int64(1) || row[1] != "Ada" || row[2] != int64(1) || row[3] != `{"lang":"go"}` {
		t.Fatalf("unexpected row: %#v", row)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err = d.Query(ctx, `SELECT name FROM users WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rows.Next()
	if !ok || row[0] != "Ada" {
		t.Fatalf("recovered row = %#v ok=%v", row, ok)
	}
}

func TestUpdateDelete(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE tasks (id INT PRIMARY KEY, title TEXT, is_done BOOLEAN)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO tasks (id, title, is_done) VALUES (1, 'ship', ?)`, false); err != nil {
		t.Fatal(err)
	}
	if res, err := d.Exec(ctx, `UPDATE tasks SET is_done = ? WHERE id = 1`, true); err != nil || res.RowsAffected != 1 {
		t.Fatalf("update res=%+v err=%v", res, err)
	}
	rows, err := d.Query(ctx, `SELECT is_done FROM tasks WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := rows.Next()
	if row[0] != int64(1) {
		t.Fatalf("done = %#v", row[0])
	}
	if res, err := d.Exec(ctx, `DELETE FROM tasks WHERE id = 1`); err != nil || res.RowsAffected != 1 {
		t.Fatalf("delete res=%+v err=%v", res, err)
	}
	rows, err = d.Query(ctx, `SELECT id FROM tasks`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("expected no rows, got %#v", row)
	}
}

func TestSavepoints(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO items VALUES (1, 'kept')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT s1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO items VALUES (2, 'rolled')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO s1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT id, name FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(1) || row[1] != "kept" {
		t.Fatalf("row = %#v ok=%v", row, ok)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("unexpected row after savepoint rollback: %#v", row)
	}
}

func TestWALCrashStyleRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE logs (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO logs VALUES (1, 'committed')`); err != nil {
		t.Fatal(err)
	}
	// Intentionally do not Close before reopening, approximating a process crash
	// where committed WAL records must still be recoverable.
	recovered, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT msg FROM logs WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "committed" {
		t.Fatalf("recovered row = %#v ok=%v", row, ok)
	}
	_ = d.Close()
}

func TestSimpleBinaryWALRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE counters (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO counters VALUES (?, ?, ?)`, int64(1), "fast", int64(10)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `UPDATE counters SET score = score + 5 WHERE id = ?`, int64(1)); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT name, score FROM counters WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "fast" || row[1] != int64(15) {
		t.Fatalf("binary WAL recovered row = %#v ok=%v", row, ok)
	}
	_ = d.Close()
}

func TestPagerBackedTableCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE checkpointed (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO checkpointed VALUES (1, 'page-backed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO checkpointed VALUES (2, 'row-page')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "db.pages")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "table_checkpointed.pages")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "table_checkpointed.pages"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < page.DefaultSize*3 {
		t.Fatalf("expected header plus two row pages, got %d bytes", len(raw))
	}
	if string(raw[page.HeaderSize:page.HeaderSize+8]) != "SQLTREC1" {
		t.Fatalf("table file does not start with record header")
	}
	if got := binary.LittleEndian.Uint64(raw[page.HeaderSize+8 : page.HeaderSize+16]); got != 2 {
		t.Fatalf("record count = %d, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "db.wal.snapshot")); !os.IsNotExist(err) {
		t.Fatalf("legacy json snapshot should not exist, stat err=%v", err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT body FROM checkpointed ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "page-backed" {
		t.Fatalf("reopened row = %#v ok=%v", row, ok)
	}
	row, ok = rows.Next()
	if !ok || row[0] != "row-page" {
		t.Fatalf("second reopened row = %#v ok=%v", row, ok)
	}
}

func TestPagerBackedIndexCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE indexed (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE INDEX indexed_name_idx ON indexed (name)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO indexed VALUES (1, 'Ada')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO indexed VALUES (2, 'Grace')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "index_indexed_indexed_name_idx.pages")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < page.DefaultSize*3 {
		t.Fatalf("expected header plus two index pages, got %d bytes", len(raw))
	}
	if string(raw[page.HeaderSize:page.HeaderSize+8]) != "SQLTREC1" {
		t.Fatalf("index file does not start with record header")
	}
	if got := binary.LittleEndian.Uint64(raw[page.HeaderSize+8 : page.HeaderSize+16]); got != 2 {
		t.Fatalf("index record count = %d, want 2", got)
	}
}

func TestReopenFromPagerCheckpointWithoutWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE durable (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO durable VALUES (1, 'from-pages')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "db.wal")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT msg FROM durable WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "from-pages" {
		t.Fatalf("pager-only reopen row = %#v ok=%v", row, ok)
	}
}
