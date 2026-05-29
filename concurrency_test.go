package db

import (
	"context"
	"sync"
	"testing"
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
