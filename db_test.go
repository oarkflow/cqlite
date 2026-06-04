package db

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sujit/sqlite/internal/page"
	"github.com/sujit/sqlite/internal/pager"
	"github.com/sujit/sqlite/internal/wal"
)

func TestDatabaseFileLockAllowsCompetingReaders(t *testing.T) {
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestDatabaseFileLockAllowsCompetingReaders$", "-test.v")
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
	if err != nil {
		t.Fatalf("competing reader open failed: %v", err)
	}
	_ = competing.Close()
}

func TestDatabaseWriteLockRejectsCompetingWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows locking shim is currently advisory no-op")
	}
	if os.Getenv("SUJIT_SQLITE_WRITE_LOCK_HELPER") == "1" {
		dir := os.Getenv("SUJIT_SQLITE_LOCK_DIR")
		ready := os.Getenv("SUJIT_SQLITE_LOCK_READY")
		done := os.Getenv("SUJIT_SQLITE_LOCK_DONE")
		d, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := acquireDatabaseWriteLock(d.lock); err != nil {
			t.Fatal(err)
		}
		defer releaseDatabaseWriteLock(d.lock)
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
	ctx := context.Background()
	setup, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `CREATE TABLE write_lock_items (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(dir, "write-ready")
	done := filepath.Join(dir, "write-done")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDatabaseWriteLockRejectsCompetingWriter$", "-test.v")
	cmd.Env = append(os.Environ(),
		"SUJIT_SQLITE_WRITE_LOCK_HELPER=1",
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
			t.Fatal("helper did not acquire database write lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	competing, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer competing.Close()
	_, err = competing.Exec(ctx, `INSERT INTO write_lock_items VALUES (1)`)
	if !errors.Is(err, ErrDatabaseLocked) {
		t.Fatalf("competing write err = %v, want ErrDatabaseLocked", err)
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

func TestNamedParameterMapAndStructSources(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE named_sources (id INTEGER PRIMARY KEY, account_id INTEGER, status TEXT, total REAL)`); err != nil {
		t.Fatal(err)
	}
	type orderParams struct {
		ID        int64   `db:"id"`
		AccountID int64   `db:"account_id"`
		Status    string  `json:"status"`
		Total     float64 `sql:"total"`
	}
	if _, err := d.Exec(ctx,
		`INSERT INTO named_sources VALUES (:id, :account_id, @status, ?)`,
		orderParams{ID: 1, AccountID: 101, Status: "open"},
		42.5,
	); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{
		"account_id": int64(101),
		"status":     "open",
		"min_total":  40.0,
	}
	rows, err := d.Query(ctx,
		`SELECT id, status, total FROM named_sources WHERE account_id = :account_id AND status = @status AND total >= $min_total`,
		params,
	)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(1) || row[1] != "open" || row[2] != 42.5 {
		t.Fatalf("row = %#v ok=%v", row, ok)
	}
}

func TestInsertValuesNamedRowAndRows(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE insert_named_rows (id INTEGER PRIMARY KEY, account_id INTEGER, status TEXT, total REAL)`); err != nil {
		t.Fatal(err)
	}
	type orderRow struct {
		ID        int64   `db:"id"`
		AccountID int64   `db:"account_id"`
		Status    string  `json:"status"`
		Total     float64 `sql:"total"`
	}
	if _, err := d.Exec(ctx,
		`INSERT INTO insert_named_rows (id, account_id, status, total) VALUES :row`,
		Named("row", orderRow{ID: 1, AccountID: 101, Status: "open", Total: 12.5}),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx,
		`INSERT INTO insert_named_rows (id, account_id, status, total) VALUES :row`,
		map[string]any{"row": map[string]any{"id": int64(2), "account_id": int64(101), "status": "review", "total": 19.25}},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx,
		`INSERT INTO insert_named_rows (id, account_id, status, total) VALUES :rows`,
		Named("rows", []orderRow{
			{ID: 3, AccountID: 102, Status: "open", Total: 7.75},
			{ID: 4, AccountID: 102, Status: "closed", Total: 33},
		}),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx,
		`INSERT INTO insert_named_rows (id, account_id, status, total) VALUES :rows`,
		map[string]any{"rows": []map[string]any{
			{"id": int64(5), "account_id": int64(103), "status": "open", "total": 44.5},
			{"id": int64(6), "account_id": int64(103), "status": "closed", "total": 55.5},
		}},
	); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT id, account_id, status, total FROM insert_named_rows ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for wantID := int64(1); wantID <= 6; wantID++ {
		row, ok := rows.Next()
		if !ok {
			t.Fatalf("missing row id %d", wantID)
		}
		if row[0] != wantID {
			t.Fatalf("row id = %#v, want %d", row[0], wantID)
		}
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("unexpected row: %#v", row)
	}
}

func TestCreateTableIfNotExistsPreservesRows(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE preserve_rows (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO preserve_rows VALUES (?, ?)`, int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE IF NOT EXISTS preserve_rows (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT name FROM preserve_rows WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "Ada" {
		t.Fatalf("row = %#v ok=%v", row, ok)
	}
}

func TestSingleFileDatabasePersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "single.db")
	d, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE single_file (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO single_file VALUES (?, ?)`, int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatalf("database path is a directory: %s", path)
	}
	d, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(ctx, `SELECT name FROM single_file WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "Ada" {
		t.Fatalf("row = %#v ok=%v", row, ok)
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
	var simpleTxBatches int
	var stateBatches int
	if err := log.Replay(func(rec wal.Record) error {
		if rec.Type == wal.RecordSnapshot {
			snapshots++
		}
		if records, ok, err := decodeSimpleTransactionWAL(rec.Data); err != nil {
			return err
		} else if ok {
			simpleTxBatches++
			if len(records) == 0 {
				t.Fatal("simple transaction WAL batch was empty")
			}
		}
		if _, ok, err := decodeTransactionWAL(rec.Data); err != nil {
			return err
		} else if ok {
			txBatches++
		}
		if _, ok, err := decodeStateWAL(rec.Data); err != nil {
			return err
		} else if ok {
			stateBatches++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 {
		t.Fatalf("transaction commit wrote %d state snapshot WAL records", snapshots)
	}
	if txBatches+simpleTxBatches+stateBatches == 0 {
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

func TestParameterizedTransactionUsesSimpleMutationWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE simple_tx_wal (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO simple_tx_wal VALUES (?, ?)`, int64(1), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO simple_tx_wal VALUES (?, ?)`, int64(2), "two"); err != nil {
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
	var simpleTxBatches int
	if err := log.Replay(func(rec wal.Record) error {
		records, ok, err := decodeSimpleTransactionWAL(rec.Data)
		if err != nil {
			return err
		}
		if ok {
			simpleTxBatches++
			if len(records) != 2 {
				t.Fatalf("simple mutation WAL records = %d, want 2", len(records))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if simpleTxBatches != 1 {
		t.Fatalf("simple transaction WAL batches = %d, want 1", simpleTxBatches)
	}
	_ = d.Close()

	recovered, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT msg FROM simple_tx_wal WHERE id = ?`, int64(2))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "two" {
		t.Fatalf("recovered simple mutation WAL row = %#v ok=%v", row, ok)
	}
}

func TestNonSimpleDMLUsesBinaryStateWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE state_wal (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := d.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO state_wal (msg, id) VALUES (?, ?)`, "from-state", int64(1)); err != nil {
		t.Fatal(err)
	}

	log, err := wal.Open(filepath.Join(dir, "db.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var stateRecords int
	var logicalRecords int
	if err := log.Replay(func(rec wal.Record) error {
		if rec.Type != wal.RecordCommit {
			return nil
		}
		if _, ok, err := decodeStateWAL(rec.Data); err != nil {
			return err
		} else if ok {
			stateRecords++
			return nil
		}
		var logical logicalRecord
		if err := json.Unmarshal(rec.Data, &logical); err == nil && logical.SQL != "" {
			logicalRecords++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stateRecords == 0 {
		t.Fatal("expected non-simple DML to write binary state WAL")
	}
	if logicalRecords != 0 {
		t.Fatalf("non-simple DML wrote %d logical SQL WAL records", logicalRecords)
	}

	reopened, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT msg FROM state_wal WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "from-state" {
		t.Fatalf("recovered state WAL row = %#v ok=%v", row, ok)
	}
	_ = d.Close()
}

func TestFailedPendingOnlyCommitDoesNotAppendRecoverableWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE failed_commit_wal (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO failed_commit_wal VALUES (?, ?)`, int64(1), "tx"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO failed_commit_wal VALUES (?, ?)`, int64(1), "live"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("conflicting transaction commit succeeded")
	}
	_ = d.Close()

	recovered, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT msg FROM failed_commit_wal WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "live" {
		t.Fatalf("recovered row after failed commit = %#v ok=%v", row, ok)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("unexpected extra row after failed commit recovery: %#v", row)
	}
}

func TestTransactionDeferredRuntimeSeesOwnRows(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE tx_visible (id INTEGER PRIMARY KEY, name TEXT UNIQUE, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(ctx, `INSERT INTO tx_visible VALUES (?, ?, ?)`, int64(1), "ada", int64(10)); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT score FROM tx_visible WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(10) {
		t.Fatalf("tx-local primary-key lookup row = %#v ok=%v", row, ok)
	}
	if _, err := tx.Exec(ctx, `UPDATE tx_visible SET score = score + 2 WHERE id = ?`, int64(1)); err != nil {
		t.Fatal(err)
	}
	rows, err = tx.Query(ctx, `SELECT score FROM tx_visible WHERE name = ?`, "ada")
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(12) {
		t.Fatalf("tx-local indexed lookup after update row = %#v ok=%v", row, ok)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tx_visible WHERE id = ?`, int64(1)); err != nil {
		t.Fatal(err)
	}
	rows, err = tx.Query(ctx, `SELECT score FROM tx_visible WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("deleted tx-local row remained visible: %#v", row)
	}
}

func TestTransactionDeferredRuntimeRejectsDuplicateRows(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE tx_dupes (id INTEGER PRIMARY KEY, name TEXT UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(ctx, `INSERT INTO tx_dupes VALUES (?, ?)`, int64(1), "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tx_dupes VALUES (?, ?)`, int64(1), "grace"); err == nil {
		t.Fatal("duplicate primary key inside transaction was accepted")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tx_dupes VALUES (?, ?)`, int64(2), "ada"); err == nil {
		t.Fatal("duplicate unique index value inside transaction was accepted")
	}
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
	if err := os.Remove(filepath.Join(dir, "db.wal")); err != nil {
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

func TestTablePageCorruptionIsRejectedOnReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE corrupt_table (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO corrupt_table VALUES (1, 'Ada')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "db.wal")); err != nil {
		t.Fatal(err)
	}
	path := singleGlob(t, filepath.Join(dir, "table_corrupt_table_*.pages"))
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
		t.Fatal("expected corrupt table page to fail reopen")
	}
}

func TestMissingCheckpointTableGenerationIsRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE missing_gen (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO missing_gen VALUES (1, 'Ada')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "db.wal")); err != nil {
		t.Fatal(err)
	}
	path := singleGlob(t, filepath.Join(dir, "table_missing_gen_*.pages"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir, Options{}); err == nil {
		_ = reopened.Close()
		t.Fatal("expected missing checkpoint table generation to fail reopen")
	}
}

func TestCorruptCheckpointMetadataIsRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE corrupt_meta (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO corrupt_meta VALUES (1, 'Ada')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "db.wal")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "db.pages"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < page.DefaultSize {
		t.Fatalf("db.pages too small: %d", len(raw))
	}
	raw[page.HeaderSize+20] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, "db.pages"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir, Options{}); err == nil {
		_ = reopened.Close()
		t.Fatal("expected corrupt checkpoint metadata to fail reopen")
	}
}

func TestPhysicalPageWALRestoresCheckpointMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE physical_wal_restore (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO physical_wal_restore VALUES (1, 'Ada')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "db.pages"))
	if err != nil {
		t.Fatal(err)
	}
	for i := page.HeaderSize; i < page.HeaderSize+32 && i < len(raw); i++ {
		raw[i] ^= 0xff
	}
	if err := os.WriteFile(filepath.Join(dir, "db.pages"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT name FROM physical_wal_restore WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "Ada" {
		t.Fatalf("restored row = %#v ok=%v", row, ok)
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

func TestCommittedWALWithoutCheckpointSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE wal_only (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO wal_only VALUES (?, ?)`, int64(1), "from-wal"); err != nil {
		t.Fatal(err)
	}
	if d.checkpointed != 0 {
		t.Fatalf("checkpointed = %d, want 0", d.checkpointed)
	}
	reopened, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT msg FROM wal_only WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "from-wal" {
		t.Fatalf("wal-only recovered row = %#v ok=%v", row, ok)
	}
	_ = d.Close()
}

func TestCommittedWALSurvivesMissingCheckpointFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE wal_missing_pages (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO wal_missing_pages VALUES (?, ?)`, int64(1), "from-wal"); err != nil {
		t.Fatal(err)
	}
	if err := d.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{
		filepath.Join(dir, "db.pages"),
		filepath.Join(dir, "table_wal_missing_pages_*.pages"),
		filepath.Join(dir, "index_wal_missing_pages_*.pages"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			_ = os.Remove(match)
			_ = os.Remove(match + ".free")
		}
	}
	reopened, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT msg FROM wal_missing_pages WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "from-wal" {
		t.Fatalf("wal recovered row after missing checkpoint = %#v ok=%v", row, ok)
	}
	_ = d.Close()
}

func TestCommittedDMLDoesNotCheckpointBeforeCloseByDefault(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE eager_pages (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO eager_pages VALUES (?, ?)`, int64(1), "committed"); err != nil {
		t.Fatal(err)
	}
	if d.checkpointed != 0 {
		t.Fatalf("checkpointed = %d, want 0 before close/manual checkpoint", d.checkpointed)
	}
	if _, err := d.pager.ReadBlob(); err == nil {
		t.Fatal("unexpected checkpoint metadata before close/manual checkpoint")
	}
}

func TestManualCheckpointWritesPagerBeforeClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE manual_pages (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO manual_pages VALUES (?, ?)`, int64(1), "committed"); err != nil {
		t.Fatal(err)
	}
	if err := d.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	metaBlob, err := d.pager.ReadBlob()
	if err != nil {
		t.Fatal(err)
	}
	var meta checkpointMeta
	if err := json.Unmarshal(metaBlob, &meta); err != nil {
		t.Fatal(err)
	}
	table, err := d.loadTablePages("manual_pages", d.state.Catalog.Tables["manual_pages"], meta.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) != 1 || table.Rows[0].Values["msg"] != "committed" {
		t.Fatalf("pager table rows = %#v", table.Rows)
	}
	if d.checkpointed != d.applied {
		t.Fatalf("checkpointed = %d, applied = %d", d.checkpointed, d.applied)
	}
}

func TestCheckpointThresholdTriggers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, DisableFileLock: true, CheckpointIntervalTx: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE threshold_pages (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if d.checkpointed != 0 {
		t.Fatalf("checkpointed after first tx = %d, want 0", d.checkpointed)
	}
	if _, err := d.Exec(ctx, `INSERT INTO threshold_pages VALUES (?, ?)`, int64(1), "one"); err != nil {
		t.Fatal(err)
	}
	if d.checkpointed != d.applied {
		t.Fatalf("checkpointed = %d, applied = %d", d.checkpointed, d.applied)
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
