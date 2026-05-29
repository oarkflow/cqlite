package db

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sujit/sqlite/internal/page"
	"github.com/sujit/sqlite/internal/pager"
	"github.com/sujit/sqlite/internal/wal"
)

func TestDatabaseFileLockRejectsCompetingProcess(t *testing.T) {
	if os.Getenv("SUJIT_SQLITE_LOCK_HELPER") == "1" {
		dir := os.Getenv("SUJIT_SQLITE_LOCK_DIR")
		ready := os.Getenv("SUJIT_SQLITE_LOCK_READY")
		done := os.Getenv("SUJIT_SQLITE_LOCK_DONE")
		d, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(done); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for parent")
	}

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	done := filepath.Join(dir, "done")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDatabaseFileLockRejectsCompetingProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		"SUJIT_SQLITE_LOCK_HELPER=1",
		"SUJIT_SQLITE_LOCK_DIR="+dir,
		"SUJIT_SQLITE_LOCK_READY="+ready,
		"SUJIT_SQLITE_LOCK_DONE="+done,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(done, []byte("done"), 0o600)
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("helper did not acquire database lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	competing, err := Open(dir, Options{})
	if err == nil {
		_ = competing.Close()
		t.Fatal("expected competing open to fail while another process holds the database lock")
	}
	if !errors.Is(err, ErrDatabaseLocked) {
		t.Fatalf("competing open err = %v, want ErrDatabaseLocked", err)
	}
}

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

func TestUpdateBySecondaryIndex(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE indexed_updates (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE INDEX indexed_updates_name_idx ON indexed_updates (name)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := d.Exec(ctx, `INSERT INTO indexed_updates VALUES (?, ?, ?)`, int64(i), "same", int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(ctx, `INSERT INTO indexed_updates VALUES (?, ?, ?)`, int64(4), "other", int64(10)); err != nil {
		t.Fatal(err)
	}
	res, err := d.Exec(ctx, `UPDATE indexed_updates SET score = score + 2 WHERE name = ?`, "same")
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsAffected != 3 {
		t.Fatalf("rows affected = %d, want 3", res.RowsAffected)
	}
	rows, err := d.Query(ctx, `SELECT score FROM indexed_updates WHERE name = ? ORDER BY id`, "same")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []any{int64(3), int64(4), int64(5)} {
		row, ok := rows.Next()
		if !ok || row[0] != want {
			t.Fatalf("updated row = %#v ok=%v, want %v", row, ok, want)
		}
	}
}

func TestSQLTransactionCommandsOnDB(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE sql_tx (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO sql_tx VALUES (1, 'one')`); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT msg FROM sql_tx WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != "one" {
		t.Fatalf("active transaction query = %#v ok=%v", row, ok)
	}
	if _, err := d.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	rows, err = d.Query(ctx, `SELECT msg FROM sql_tx WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("rolled back row visible: %#v", row)
	}

	if _, err := d.Exec(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO sql_tx VALUES (2, 'two')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `SAVEPOINT s1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO sql_tx VALUES (3, 'three')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `ROLLBACK TO s1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `RELEASE SAVEPOINT s1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	rows, err = d.Query(ctx, `SELECT id FROM sql_tx ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(2) {
		t.Fatalf("committed row = %#v ok=%v", row, ok)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("unexpected savepoint row: %#v", row)
	}
}

func TestNamedParameters(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE named_params (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO named_params VALUES (:id, $name)`, Named("id", int64(1)), Named("name", "Ada")); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT name FROM named_params WHERE id = @id`, Named("id", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "Ada" {
		t.Fatalf("row = %#v ok=%v", row, ok)
	}
	if _, err := d.Query(ctx, `SELECT name FROM named_params WHERE id = :missing`); err == nil {
		t.Fatal("expected missing named parameter error")
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
	recovered, err := Open(dir, Options{DisableFileLock: true})
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
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
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
	if _, err := d.Exec(ctx, `INSERT INTO counters VALUES (?, ?, ?)`, int64(2), "deleted", int64(20)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `DELETE FROM counters WHERE id = ?`, int64(2)); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
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
	deleted, err := recovered.Query(ctx, `SELECT name FROM counters WHERE id = ?`, int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := deleted.Next(); ok {
		t.Fatalf("deleted WAL row recovered: %#v", row)
	}
	_ = d.Close()
}

func TestUncommittedTransactionNotRecovered(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE txlogs (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO txlogs VALUES (1, 'rolled')`); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT id FROM txlogs`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("uncommitted row recovered: %#v", row)
	}
	_ = tx.Rollback()
	_ = d.Close()
}

func TestTransactionCommitUsesBatchWALNotStateSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE txbatch (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO txbatch VALUES (1, 'one')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT s1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO txbatch VALUES (2, 'rolled')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO s1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO txbatch VALUES (3, 'three')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	log, err := wal.Open(filepath.Join(dir, "db.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var snapshots int
	var txBatches int
	if err := log.Replay(func(rec wal.Record) error {
		if rec.Type == wal.RecordSnapshot {
			snapshots++
		}
		if _, ok, err := decodeTransactionWAL(rec.Data); err != nil {
			return err
		} else if ok {
			txBatches++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 {
		t.Fatalf("transaction commit wrote %d state snapshot WAL records", snapshots)
	}
	if txBatches == 0 {
		t.Fatal("expected at least one transaction batch WAL record")
	}

	recovered, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT id, msg FROM txbatch ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(1) || row[1] != "one" {
		t.Fatalf("first recovered row = %#v ok=%v", row, ok)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(3) || row[1] != "three" {
		t.Fatalf("second recovered row = %#v ok=%v", row, ok)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("unexpected recovered row after savepoint rollback: %#v", row)
	}
	_ = d.Close()
}

func TestWALCrashStyleRecoveryWithIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE indexed_logs (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE INDEX indexed_logs_name_idx ON indexed_logs (name)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO indexed_logs VALUES (?, ?)`, int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT id FROM indexed_logs WHERE name = ?`, "Ada")
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(1) {
		t.Fatalf("indexed WAL recovered row = %#v ok=%v", row, ok)
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
	tablePath := singleGlob(t, filepath.Join(dir, "table_checkpointed_*.pages"))
	raw, err := os.ReadFile(tablePath)
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
	path := singleGlob(t, filepath.Join(dir, "index_indexed_indexed_name_idx_*.pages"))
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
	pg, err := pager.Open(path, page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	records, err := pg.ReadRecords()
	if closeErr := pg.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("index records = %d, want 2", len(records))
	}
	first, err := decodeIndexRecord(records[0].Values)
	if err != nil {
		t.Fatal(err)
	}
	if first.RowID != 1 || len(first.Key) != 1 || first.Key[0] != "Ada" {
		t.Fatalf("first index record = %#v", first)
	}
}

func TestIndexPageCorruptionIsRejectedOnReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE corrupt_idx (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE INDEX corrupt_idx_name_idx ON corrupt_idx (name)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO corrupt_idx VALUES (1, 'Ada')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	path := singleGlob(t, filepath.Join(dir, "index_corrupt_idx_corrupt_idx_name_idx_*.pages"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir, Options{}); err == nil {
		_ = reopened.Close()
		t.Fatal("expected corrupt index page to fail reopen")
	}
}

func TestInterruptedCheckpointGenerationIsIgnored(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE checkpoint_gen (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO checkpoint_gen VALUES (1, 'committed')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = Open(dir, Options{DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	futureGeneration := uint64(99999999)
	if err := d.writeTablePages("checkpoint_gen", d.state.Catalog.Tables["checkpoint_gen"], d.state.Tables["checkpoint_gen"], futureGeneration); err != nil {
		t.Fatal(err)
	}
	futurePath := d.tablePagerPath("checkpoint_gen", futureGeneration)
	raw, err := os.ReadFile(futurePath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(futurePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT msg FROM checkpoint_gen WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "committed" {
		t.Fatalf("row after ignored checkpoint = %#v ok=%v", row, ok)
	}
	_ = d.Close()
}

func TestCheckpointGenerationCleanup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE checkpoint_cleanup (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO checkpoint_cleanup VALUES (1, 'one')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	first := singleGlob(t, filepath.Join(dir, "table_checkpoint_cleanup_*.pages"))
	d, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO checkpoint_cleanup VALUES (2, 'two')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "table_checkpoint_cleanup_*.pages"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("checkpoint generations = %#v, want one current generation", matches)
	}
	if matches[0] == first {
		t.Fatalf("old checkpoint generation was not replaced: %s", first)
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

func singleGlob(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("glob %q matched %d files: %#v", pattern, len(matches), matches)
	}
	return matches[0]
}
