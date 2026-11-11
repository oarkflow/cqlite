package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentManyReadersAndOneWriter(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE counters (id INTEGER PRIMARY KEY, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO counters VALUES (?, ?)`, int64(1), int64(0)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 25)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rows, err := d.Query(ctx, `SELECT score FROM counters WHERE id = ?`, int64(1))
				if err != nil {
					errs <- err
					return
				}
				if _, ok := rows.Next(); !ok {
					errs <- errMissingRow
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			if _, err := d.Exec(ctx, `UPDATE counters SET score = score + 1 WHERE id = ?`, int64(1)); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT score FROM counters WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(100) {
		t.Fatalf("score = %#v ok=%v", row, ok)
	}
}

func TestConcurrentWriterSerialization(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE counters (id INTEGER PRIMARY KEY, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO counters VALUES (?, ?)`, int64(1), int64(0)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := d.Exec(ctx, `UPDATE counters SET score = score + 1 WHERE id = ?`, int64(1)); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT score FROM counters WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(400) {
		t.Fatalf("score = %#v ok=%v", row, ok)
	}
}

func TestConcurrentCrossProcessWritersNoLostUpdates(t *testing.T) {
	const updatesPerProcess = 100
	ctx := context.Background()
	if os.Getenv("CQLITE_CROSS_PROCESS_WRITER") == "1" {
		dir := os.Getenv("CQLITE_CROSS_PROCESS_DIR")
		ready := os.Getenv("CQLITE_CROSS_PROCESS_READY")
		start := os.Getenv("CQLITE_CROSS_PROCESS_START")
		d, err := Open(dir, Options{SyncPolicy: SyncFull})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(start); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for parent writer start")
			}
			time.Sleep(time.Millisecond)
		}
		for i := 0; i < updatesPerProcess; i++ {
			if _, err := d.Exec(ctx, `UPDATE shared_counter SET value = value + 1 WHERE id = 1`); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	dir := t.TempDir()
	setup, err := Open(dir, Options{SyncPolicy: SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `CREATE TABLE shared_counter (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `INSERT INTO shared_counter VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	parent, err := Open(dir, Options{SyncPolicy: SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	ready := filepath.Join(dir, "cross-process-ready")
	start := filepath.Join(dir, "cross-process-start")
	cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentCrossProcessWritersNoLostUpdates$")
	cmd.Env = append(os.Environ(),
		"CQLITE_CROSS_PROCESS_WRITER=1",
		"CQLITE_CROSS_PROCESS_DIR="+dir,
		"CQLITE_CROSS_PROCESS_READY="+ready,
		"CQLITE_CROSS_PROCESS_START="+start,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childWaited := false
	defer func() {
		_ = os.WriteFile(start, []byte("start"), 0o600)
		if !childWaited {
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("child writer did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(start, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < updatesPerProcess; i++ {
		if _, err := parent.Exec(ctx, `UPDATE shared_counter SET value = value + 1 WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		childWaited = true
		t.Fatalf("child writer failed: %v", err)
	}
	childWaited = true
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(dir, Options{SyncPolicy: SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(ctx, `SELECT value FROM shared_counter WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(updatesPerProcess*2) {
		t.Fatalf("recovered counter = %#v, ok=%v; want %d", row, ok, updatesPerProcess*2)
	}
}

func TestWALCommitSurvivesAbruptProcessExit(t *testing.T) {
	const dirEnv = "CQLITE_ABRUPT_EXIT_DIR"
	if dir := os.Getenv(dirEnv); dir != "" {
		d, err := Open(dir, Options{SyncPolicy: SyncFull})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(context.Background(), `INSERT INTO durable_after_exit VALUES (1, 'committed')`); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	setup, err := Open(dir, Options{SyncPolicy: SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(context.Background(), `CREATE TABLE durable_after_exit (id INTEGER PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestWALCommitSurvivesAbruptProcessExit$")
	cmd.Env = append(os.Environ(), dirEnv+"="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writer process failed: %v\n%s", err, output)
	}

	recovered, err := Open(dir, Options{SyncPolicy: SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	rows, err := recovered.Query(context.Background(), `SELECT value FROM durable_after_exit WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != "committed" {
		t.Fatalf("recovered row = %#v, ok=%v; want committed value", row, ok)
	}
}

func TestWriterLockWaitHonorsContext(t *testing.T) {
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(context.Background(), `CREATE TABLE locked (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	competing, err := Open(d.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer competing.Close()
	if err := acquireDatabaseWriteLock(competing.lock); err != nil {
		t.Fatal(err)
	}
	defer releaseDatabaseWriteLock(competing.lock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = d.Exec(ctx, `INSERT INTO locked VALUES (1)`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("context cancellation took %s", elapsed)
	}
}

func TestReadUsesCommittedSnapshotWhileAnotherHandleWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writer, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(ctx, `CREATE TABLE snapshot_reads (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writer.Exec(ctx, `INSERT INTO snapshot_reads VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if err := acquireDatabaseWriteLock(writer.lock); err != nil {
		t.Fatal(err)
	}
	rows, err := reader.Query(ctx, `SELECT id FROM snapshot_reads`)
	if err != nil {
		_ = releaseDatabaseWriteLock(writer.lock)
		t.Fatalf("reader errored while writer lock was held: %v", err)
	}
	if _, ok := rows.Next(); ok {
		_ = releaseDatabaseWriteLock(writer.lock)
		t.Fatal("reader should retain its prior committed snapshot during the write lock")
	}
	if err := releaseDatabaseWriteLock(writer.lock); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.Query(ctx, `SELECT id FROM snapshot_reads`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(1) {
		t.Fatalf("reader after writer released lock = %#v, ok=%v; want row 1", row, ok)
	}
}

func TestReadersOnHandleAreNotBlockedByWriterLockWait(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE lock_wait_readers (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	competitor, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer competitor.Close()
	if err := acquireDatabaseWriteLock(competitor.lock); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		close(started)
		_, err := d.Exec(ctx, `INSERT INTO lock_wait_readers VALUES (1)`)
		writeDone <- err
	}()
	<-started
	time.Sleep(25 * time.Millisecond)
	readCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, readErr := d.Query(readCtx, `SELECT id FROM lock_wait_readers`)
	cancel()
	if readErr != nil {
		_ = releaseDatabaseWriteLock(competitor.lock)
		t.Fatalf("read during another writer's lock wait: %v", readErr)
	}
	if err := releaseDatabaseWriteLock(competitor.lock); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("waiting writer: %v", err)
	}
}

func TestConcurrentDBHandlesRebaseTransactions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Exec(ctx, `CREATE TABLE valueset (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, `INSERT INTO valueset VALUES (1, 0), (2, 0)`); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	tx1, err := first.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := second.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Exec(ctx, `UPDATE valueset SET value = 11 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `UPDATE valueset SET value = 22 WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}
	reader, err := second.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	readerRows, err := reader.Query(ctx, `SELECT value FROM valueset WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := readerRows.Next(); !ok || row[0] != int64(11) {
		t.Fatalf("new read transaction did not observe external commit: %#v ok=%v", row, ok)
	}
	if err := reader.Commit(); err != nil {
		t.Fatal(err)
	}
	visible, err := second.Query(ctx, `SELECT value FROM valueset WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := visible.Next(); !ok || row[0] != int64(11) {
		t.Fatalf("second handle did not observe committed value: %#v ok=%v", row, ok)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}

	check, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err := check.Query(ctx, `SELECT id, value FROM valueset ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{int64(1), int64(11)}, {int64(2), int64(22)}}
	for i, expected := range want {
		row, ok := rows.Next()
		if !ok || row[0] != expected[0] || row[1] != expected[1] {
			t.Fatalf("row %d = %#v, ok=%v; want %#v", i, row, ok, expected)
		}
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("unexpected extra row: %#v", row)
	}
}

func TestConcurrentSharedDatabaseHandles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const handles = 4
	const writesPerHandle = 20
	dbs := make([]*DB, handles)
	for i := range dbs {
		d, err := Open(dir, Options{SyncPolicy: SyncFull, BusyTimeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		dbs[i] = d
		if i == 0 {
			if _, err := d.Exec(ctx, `CREATE TABLE shared_counter (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(ctx, `INSERT INTO shared_counter VALUES (1, 0)`); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer func() {
		for _, d := range dbs {
			_ = d.Close()
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, handles)
	for _, d := range dbs {
		wg.Add(1)
		go func(d *DB) {
			defer wg.Done()
			for i := 0; i < writesPerHandle; i++ {
				if _, err := d.Exec(ctx, `UPDATE shared_counter SET value = value + 1 WHERE id = 1`); err != nil {
					errs <- err
					return
				}
			}
		}(d)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := dbs[0].Query(ctx, `SELECT value FROM shared_counter WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(handles*writesPerHandle) {
		t.Fatalf("shared counter = %#v, ok=%v; want %d", row, ok, handles*writesPerHandle)
	}
}

func TestWALRefreshReplaysPrimaryKeyUpdateAfterInsertBatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writer, err := Open(dir, Options{SyncPolicy: SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(ctx, `CREATE TABLE replay_records (id INTEGER PRIMARY KEY, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 32; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO replay_records VALUES (?, ?)`, int64(i), int64(i)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	const handles = 4
	dbs := make([]*DB, handles)
	dbs[0] = writer
	for i := 1; i < handles; i++ {
		dbs[i], err = Open(dir, Options{SyncPolicy: SyncFull})
		if err != nil {
			t.Fatal(err)
		}
		defer dbs[i].Close()
		rows, err := dbs[i].Query(ctx, `SELECT score FROM replay_records WHERE id = ?`, int64(7))
		if err != nil {
			t.Fatal(err)
		}
		row, ok := rows.Next()
		if !ok || row[0] != int64(7) {
			t.Fatalf("handle %d initial score = %#v, ok=%v; want 7", i, row, ok)
		}
	}

	const writesPerHandle = 12
	var wg sync.WaitGroup
	errs := make(chan error, handles)
	for i, handle := range dbs {
		wg.Add(1)
		go func(i int, handle *DB) {
			defer wg.Done()
			for j := 0; j < writesPerHandle; j++ {
				id := int64((i+j)%32 + 1)
				if _, err := handle.Exec(ctx, `UPDATE replay_records SET score = score + 1 WHERE id = ?`, id); err != nil {
					errs <- err
					return
				}
			}
		}(i, handle)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for i, handle := range dbs {
		rows, err := handle.Query(ctx, `SELECT COUNT(*) FROM replay_records`)
		if err != nil {
			t.Fatalf("handle %d count query: %v", i, err)
		}
		row, ok := rows.Next()
		if !ok || row[0] != int64(32) {
			t.Fatalf("handle %d row count = %#v, ok=%v; want 32", i, row, ok)
		}
	}
}

func TestAppendBatchKeepsTransactionSnapshotIsolated(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE snapshot_rows (id INTEGER PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO snapshot_rows VALUES (1, 'old')`); err != nil {
		t.Fatal(err)
	}
	stale, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Rollback()
	if _, err := d.Exec(ctx, `INSERT INTO snapshot_rows VALUES (2, 'new')`); err != nil {
		t.Fatal(err)
	}
	rows, err := stale.Query(ctx, `SELECT count(*) FROM snapshot_rows`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(1) {
		t.Fatalf("stale transaction count = %#v, ok=%v; want 1", row, ok)
	}
}

func TestConcurrentASTQueries(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE ast_reads (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO ast_reads VALUES (1, 20)`); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rows, err := d.Query(ctx, `WITH source AS (SELECT value FROM ast_reads) SELECT value * 2 + 1 FROM source`)
				if err != nil {
					errs <- err
					return
				}
				row, ok := rows.Next()
				if !ok || row[0] != int64(41) {
					errs <- fmt.Errorf("AST query row = %#v, ok=%v", row, ok)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestTriggerScopeDoesNotLeakAcrossDatabases(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE ordinary (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO ordinary VALUES (1, 7)`); err != nil {
		t.Fatal(err)
	}

	triggerScopeMu.Lock()
	restore := withTriggerRows(nil, map[string]any{"value": int64(99)})
	result := make(chan *Rows, 1)
	queryErr := make(chan error, 1)
	go func() {
		rows, err := d.Query(ctx, `SELECT old.value FROM ordinary WHERE id = 1`)
		if err != nil {
			queryErr <- err
			return
		}
		result <- rows
	}()
	select {
	case <-result:
		restore()
		triggerScopeMu.Unlock()
		t.Fatal("ordinary query ran while a trigger scope was active")
	case err := <-queryErr:
		restore()
		triggerScopeMu.Unlock()
		t.Fatal(err)
	case <-time.After(30 * time.Millisecond):
	}
	restore()
	triggerScopeMu.Unlock()
	select {
	case err := <-queryErr:
		t.Fatal(err)
	case rows := <-result:
		row, ok := rows.Next()
		if !ok || row[0] != int64(7) {
			t.Fatalf("ordinary old.value result = %#v ok=%v, want the ordinary row value", row, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("query did not resume after trigger scope ended")
	}
}

func TestDetachWaitsForRoutedOperation(t *testing.T) {
	ctx := context.Background()
	parent, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	childPath := t.TempDir()
	child, err := Open(childPath, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Exec(ctx, `CREATE TABLE items (value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.attachDatabase(ctx, "aux", childPath); err != nil {
		t.Fatal(err)
	}
	ad, err := parent.foreignTarget(`SELECT value FROM aux.items`)
	if err != nil || ad == nil {
		t.Fatalf("foreignTarget() = %v, %v", ad, err)
	}
	detached := make(chan error, 1)
	go func() { detached <- parent.detachDatabase("aux") }()
	deadline := time.Now().Add(time.Second)
	for {
		parent.attachedMu.RLock()
		_, stillAttached := parent.attached["aux"]
		parent.attachedMu.RUnlock()
		if !stillAttached {
			break
		}
		if time.Now().After(deadline) {
			ad.mu.RUnlock()
			t.Fatal("detach did not unregister the database")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-detached:
		ad.mu.RUnlock()
		t.Fatalf("detach completed while a routed operation held its lease: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	ad.mu.RUnlock()
	select {
	case err := <-detached:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("detach did not resume after the routed operation completed")
	}
}

func BenchmarkParallelASTReaders(b *testing.B) {
	ctx := context.Background()
	d, err := Open(b.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE ast_reads (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		b.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO ast_reads VALUES (1, 20)`); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rows, err := d.Query(ctx, `SELECT value * 2 + id FROM ast_reads WHERE id = ?`, int64(1))
			if err != nil {
				b.Error(err)
				return
			}
			if row, ok := rows.Next(); !ok || row[0] != int64(41) {
				b.Errorf("AST query row = %#v, ok=%v", row, ok)
				return
			}
		}
	})
}

func BenchmarkParallelIndependentWriters(b *testing.B) {
	const connections = 8
	ctx := context.Background()
	dbs := make([]*DB, connections)
	for i := range dbs {
		d, err := Open(b.TempDir(), Options{SyncPolicy: SyncOff})
		if err != nil {
			b.Fatal(err)
		}
		dbs[i] = d
		if _, err := d.Exec(ctx, `CREATE TABLE independent (id INTEGER PRIMARY KEY, value TEXT, extra INTEGER)`); err != nil {
			b.Fatal(err)
		}
		if _, err := d.Exec(ctx, `INSERT INTO independent VALUES (1, 'seed', 0)`); err != nil {
			b.Fatal(err)
		}
	}
	defer func() {
		for _, d := range dbs {
			_ = d.Close()
		}
	}()
	var sequence atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		connection := sequence.Add(1) % connections
		d := dbs[connection]
		for pb.Next() {
			if _, err := d.Exec(ctx, `UPDATE independent SET value = ?, extra = extra + 1 WHERE id = 1`, "payload"); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func TestRollbackDuringReaders(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE docs (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO docs VALUES (?, ?)`, int64(1), "kept"); err != nil {
		t.Fatal(err)
	}

	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO docs VALUES (?, ?)`, int64(2), "rolled"); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT body FROM docs WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "kept" {
		t.Fatalf("reader row = %#v ok=%v", row, ok)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	rows, err = d.Query(ctx, `SELECT id FROM docs WHERE id = ?`, int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("rolled back row visible: %#v", row)
	}
}

func TestReadTransactionUsesSnapshot(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `CREATE TABLE docs (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO docs VALUES (?, ?)`, int64(1), "before"); err != nil {
		t.Fatal(err)
	}
	reader, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := d.Exec(ctx, `UPDATE docs SET body = ? WHERE id = ?`, "after", int64(1)); err != nil {
		t.Fatal(err)
	} else if res.RowsAffected != 1 {
		t.Fatalf("update rows affected = %d, want 1", res.RowsAffected)
	}
	rows, err := reader.Query(ctx, `SELECT body FROM docs WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "before" {
		t.Fatalf("snapshot reader row = %#v ok=%v", row, ok)
	}
	if err := reader.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err = d.Query(ctx, `SELECT body FROM docs WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rows.Next()
	if !ok || row[0] != "after" {
		t.Fatalf("post-commit row = %#v ok=%v", row, ok)
	}
}

var errMissingRow = missingRowError{}

type missingRowError struct{}

func (missingRowError) Error() string { return "missing row" }
