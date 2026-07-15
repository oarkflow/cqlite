package db

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCrashDataConsistency(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}

	crash := func() {
		// Simulate crash by closing without allowing Close() to checkpoint.
		// We intentionally discard the DB handle without calling Close(),
		// then force-close the underlying WAL file so no clean marker is written.
		_ = d.wal.Close()
		_ = d.pager.Close()
		d = nil
	}

	if _, err := d.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, value TEXT, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO items VALUES (?, ?, ?)`, int64(1), "alpha", int64(10)); err != nil {
		t.Fatal(err)
	}
	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT value, score FROM items WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "alpha" || row[1] != int64(10) {
		t.Fatalf("crash after DDL + insert: recovered row = %#v ok=%v", row, ok)
	}

	// Crash after update
	if _, err := d.Exec(ctx, `UPDATE items SET score = ? WHERE id = ?`, int64(25), int64(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO items VALUES (?, ?, ?)`, int64(2), "beta", int64(20)); err != nil {
		t.Fatal(err)
	}
	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err = d.Query(ctx, `SELECT id, value, score FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(1) || row[1] != "alpha" || row[2] != int64(25) {
		t.Fatalf("crash after update: first row = %#v ok=%v", row, ok)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(2) || row[1] != "beta" || row[2] != int64(20) {
		t.Fatalf("crash after update: second row = %#v ok=%v", row, ok)
	}

	// Crash after delete
	if _, err := d.Exec(ctx, `DELETE FROM items WHERE id = ?`, int64(1)); err != nil {
		t.Fatal(err)
	}
	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err = d.Query(ctx, `SELECT id FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(2) {
		t.Fatalf("crash after delete: surviving row = %#v ok=%v", row, ok)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("crash after delete: unexpected extra row = %#v", row)
	}

	_ = d.Close()
}

func TestCrashDataConsistencyWithIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}

	crash := func() {
		_ = d.wal.Close()
		_ = d.pager.Close()
		d = nil
	}

	if _, err := d.Exec(ctx, `CREATE TABLE idx_items (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE UNIQUE INDEX idx_items_name ON idx_items (name)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE INDEX idx_items_score ON idx_items (score)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO idx_items VALUES (?, ?, ?)`, int64(1), "alice", int64(100)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO idx_items VALUES (?, ?, ?)`, int64(2), "bob", int64(200)); err != nil {
		t.Fatal(err)
	}
	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}

	// Verify PK lookup
	rows, err := d.Query(ctx, `SELECT name, score FROM idx_items WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "alice" || row[1] != int64(100) {
		t.Fatalf("pk lookup after crash: row = %#v ok=%v", row, ok)
	}

	// Verify unique index lookup
	rows, err = d.Query(ctx, `SELECT id, score FROM idx_items WHERE name = ?`, "bob")
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(2) || row[1] != int64(200) {
		t.Fatalf("unique index lookup after crash: row = %#v ok=%v", row, ok)
	}

	// Verify secondary index range scan
	rows, err = d.Query(ctx, `SELECT name FROM idx_items WHERE score >= ? ORDER BY score`, int64(100))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		names = append(names, row[0].(string))
	}
	if len(names) != 2 || names[0] != "alice" || names[1] != "bob" {
		t.Fatalf("index range scan after crash: names = %#v", names)
	}

	// Crash after update + insert + delete with indexes
	if _, err := d.Exec(ctx, `UPDATE idx_items SET score = ? WHERE name = ?`, int64(150), "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO idx_items VALUES (?, ?, ?)`, int64(3), "charlie", int64(300)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `DELETE FROM idx_items WHERE name = ?`, "bob"); err != nil {
		t.Fatal(err)
	}
	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}

	rows, err = d.Query(ctx, `SELECT id, name, score FROM idx_items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(1) || row[1] != "alice" || row[2] != int64(150) {
		t.Fatalf("after crash with index ops: row1 = %#v ok=%v", row, ok)
	}
	row, ok = rows.Next()
	if !ok || row[0] != int64(3) || row[1] != "charlie" || row[2] != int64(300) {
		t.Fatalf("after crash with index ops: row2 = %#v ok=%v", row, ok)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("after crash with index ops: extra row = %#v", row)
	}

	// Verify unique constraint via index is still correct
	if _, err := d.Exec(ctx, `INSERT INTO idx_items VALUES (?, ?, ?)`, int64(4), "alice", int64(400)); err == nil {
		t.Fatal("unique constraint on name should still be enforced after crash recovery")
	}

	_ = d.Close()
}

func TestCrashDuringTransactionCommitsAtomically(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}

	crash := func() {
		_ = d.wal.Close()
		_ = d.pager.Close()
		d = nil
	}

	if _, err := d.Exec(ctx, `CREATE TABLE tx_crash (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}

	// Uncommitted transaction should not survive crash
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tx_crash VALUES (?, ?)`, int64(1), "should-not-survive"); err != nil {
		t.Fatal(err)
	}
	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(ctx, `SELECT id FROM tx_crash`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); ok {
		t.Fatalf("uncommitted transaction survived crash: %#v", row)
	}

	// Committed transaction should survive crash
	tx, err = d.Begin(ctx, TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tx_crash VALUES (?, ?)`, int64(1), "survived"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err = d.Query(ctx, `SELECT msg FROM tx_crash WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "survived" {
		t.Fatalf("committed transaction lost after crash: row = %#v ok=%v", row, ok)
	}

	_ = d.Close()
}

func TestCrashDuringRandomOpsDataConsistency(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for iteration := 0; iteration < 10; iteration++ {
		dir := t.TempDir()

		d, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := d.Exec(ctx, `CREATE TABLE scores (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(ctx, `CREATE INDEX scores_name ON scores (name)`); err != nil {
			t.Fatal(err)
		}

		expected := make(map[int64]struct {
			name  string
			score int64
		})

		ops := rng.Intn(20) + 5
		for i := 0; i < ops; i++ {
			switch rng.Intn(4) {
			case 0: // insert
				id := int64(rng.Intn(1000) + 1)
				if _, exists := expected[id]; exists {
					continue
				}
				name := randomName(rng)
				score := int64(rng.Intn(1000))
				if _, err := d.Exec(ctx, `INSERT INTO scores VALUES (?, ?, ?)`, id, name, score); err != nil {
					t.Fatal(err)
				}
				expected[id] = struct {
					name  string
					score int64
				}{name, score}
			case 1: // update
				if len(expected) == 0 {
					continue
				}
				var id int64
				for id = range expected {
					break
				}
				delta := int64(rng.Intn(100) + 1)
				if _, err := d.Exec(ctx, `UPDATE scores SET score = score + ? WHERE id = ?`, delta, id); err != nil {
					t.Fatal(err)
				}
				e := expected[id]
				e.score += delta
				expected[id] = e
			case 2: // delete
				if len(expected) == 0 {
					continue
				}
				var id int64
				for id = range expected {
					break
				}
				if _, err := d.Exec(ctx, `DELETE FROM scores WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
				delete(expected, id)
			case 3: // range update
				if len(expected) == 0 {
					continue
				}
				if _, err := d.Exec(ctx, `UPDATE scores SET score = score + 1 WHERE score >= ?`, int64(0)); err != nil {
					t.Fatal(err)
				}
				for id, e := range expected {
					e.score++
					expected[id] = e
				}
			}

			// Random crash
			if rng.Intn(5) == 0 {
				_ = d.wal.Close()
				_ = d.pager.Close()
				d = nil

				d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
				if err != nil {
					t.Fatalf("iteration %d: reopen after crash: %v", iteration, err)
				}

				break
			}
		}

		// Verify data after all ops and crashes
		rows, err := d.Query(ctx, `SELECT id, name, score FROM scores ORDER BY id`)
		if err != nil {
			t.Fatalf("iteration %d: query: %v", iteration, err)
		}

		recovered := make(map[int64][2]any)
		for {
			row, ok := rows.Next()
			if !ok {
				break
			}
			id := row[0].(int64)
			recovered[id] = [2]any{row[1], row[2]}
		}

		for id, e := range expected {
			got, ok := recovered[id]
			if !ok {
				t.Fatalf("iteration %d: missing id %d (expected name=%s score=%d)", iteration, id, e.name, e.score)
			}
			if got[0] != e.name {
				t.Fatalf("iteration %d: id %d name = %v, want %s", iteration, id, got[0], e.name)
			}
			if got[1] != e.score {
				t.Fatalf("iteration %d: id %d score = %v, want %d", iteration, id, got[1], e.score)
			}
		}
		for id := range recovered {
			if _, ok := expected[id]; !ok {
				t.Fatalf("iteration %d: unexpected recovered id %d", iteration, id)
			}
		}

		_ = d.Close()
	}
}

func TestConcurrentCrashDataConsistency(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.Exec(ctx, `CREATE TABLE concurrent_items (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 100; i++ {
		if _, err := d.Exec(ctx, `INSERT INTO concurrent_items VALUES (?, ?)`, i, int64(0)); err != nil {
			t.Fatal(err)
		}
	}

	crash := func() {
		_ = d.wal.Close()
		_ = d.pager.Close()
		d = nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := d.Exec(ctx, `UPDATE concurrent_items SET value = value + 1 WHERE id = ?`, int64(1)); err != nil {
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := d.Exec(ctx, `UPDATE concurrent_items SET value = value + 1 WHERE id = ?`, int64(2)); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	crash()

	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := d.Query(ctx, `SELECT value FROM concurrent_items WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != int64(100) {
		t.Fatalf("concurrent crash: id=1 value = %v, want 100", row[0])
	}

	rows, err = d.Query(ctx, `SELECT id, value FROM concurrent_items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		_, ok := rows.Next()
		if !ok {
			break
		}
		count++
	}
	if count != 100 {
		t.Fatalf("concurrent crash: expected 100 rows, got %d", count)
	}

	_ = d.Close()
}

func TestCrashAfterPartialWALIsRecoverable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE partial_wal (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO partial_wal VALUES (?, ?)`, int64(1), "safe"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	// Append a partial (incomplete) WAL record by hand, simulating torn write
	f, err := os.OpenFile(filepath.Join(dir, "db.wal"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Write a header claiming 1MB of data, but write nothing after
	var header [21]byte
	header[0] = 2 // RecordCommit
	header[9] = 0xff
	header[10] = 0xff
	header[11] = 0x0f // length = 0x0fffff (1MB - 1)
	if _, err := f.Write(header[:]); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Should still open and recover the committed data before the torn record
	reopened, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT msg FROM partial_wal WHERE id = ?`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != "safe" {
		t.Fatalf("after partial WAL recovery: row = %#v ok=%v", row, ok)
	}
}

func TestCrashCheckpointInterruptionIsRecoverable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Insert some data and checkpoint normally
	d, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE cp_items (id INTEGER PRIMARY KEY, msg TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 10; i++ {
		if _, err := d.Exec(ctx, `INSERT INTO cp_items VALUES (?, ?)`, i, fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen, add more changes that are NOT checkpointed, then destroy checkpoint pages
	d, err = Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO cp_items VALUES (?, ?)`, int64(11), "checkpoint-survivor"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `DELETE FROM cp_items WHERE id = ?`, int64(5)); err != nil {
		t.Fatal(err)
	}

	// Destroy checkpoint pages to simulate interrupted checkpoint that lost pages
	for _, pattern := range []string{
		filepath.Join(dir, "db.pages"),
		filepath.Join(dir, "table_cp_items_*.pages"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			_ = os.Remove(match)
		}
	}
	_ = d.wal.Close()
	_ = d.pager.Close()
	d = nil

	// WAL should still restore all committed data
	reopened, err := Open(dir, Options{SyncPolicy: SyncFull, DisableFileLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	rows, err := reopened.Query(ctx, `SELECT id, msg FROM cp_items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		ids = append(ids, row[0].(int64))
	}
	if len(ids) != 10 {
		t.Fatalf("after checkpoint crash: expected 10 rows, got %d", len(ids))
	}
	if ids[4] == 5 {
		t.Fatalf("deleted id 5 survived checkpoint crash")
	}
}

func randomName(rng *rand.Rand) string {
	names := []string{"alice", "bob", "charlie", "diana", "eve", "frank", "grace", "henry"}
	return names[rng.Intn(len(names))]
}

var _ = randomName
