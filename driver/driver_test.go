package driver

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	db "github.com/oarkflow/sqlite"
)

type driverTextValue string

func (v driverTextValue) MarshalText() ([]byte, error) { return []byte("typed:" + string(v)), nil }

func TestDatabaseSQLDriver(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE users (id INT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO users VALUES (?, ?)`, int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := database.QueryRow(`SELECT name FROM users WHERE id = ?`, int64(1)).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Ada" {
		t.Fatalf("name = %q", name)
	}
}

func TestDriverSharesFileEngineAcrossConnections(t *testing.T) {
	path := t.TempDir()
	d := &Driver{}
	firstRaw, err := d.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := d.Open(path)
	if err != nil {
		_ = firstRaw.Close()
		t.Fatal(err)
	}
	first, second := firstRaw.(*Conn), secondRaw.(*Conn)
	if first.db != second.db {
		t.Fatal("connections to the same file opened separate engine instances")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.db.Exec(context.Background(), `CREATE TABLE shared_driver (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("closing one connection closed the shared engine: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDriverKeepsMemoryConnectionsPrivate(t *testing.T) {
	d := &Driver{}
	firstRaw, err := d.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := d.Open(":memory:")
	if err != nil {
		_ = firstRaw.Close()
		t.Fatal(err)
	}
	first, second := firstRaw.(*Conn), secondRaw.(*Conn)
	if first.db == second.db {
		t.Fatal("private memory connections unexpectedly share an engine")
	}
	_ = first.Close()
	_ = second.Close()
}

func TestDriverConcurrentOpensShareAndReleaseEngine(t *testing.T) {
	const count = 8
	path := t.TempDir()
	connections := make([]*Conn, count)
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range connections {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			raw, err := (&Driver{}).Open(path)
			if err != nil {
				errs <- err
				return
			}
			connections[i] = raw.(*Conn)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < len(errs); i++ {
		t.Fatal(<-errs)
	}
	for i := 1; i < count; i++ {
		if connections[i].db != connections[0].db {
			t.Fatalf("connection %d opened a distinct engine", i)
		}
	}

	for _, conn := range connections {
		wg.Add(1)
		go func(conn *Conn) {
			defer wg.Done()
			if err := conn.Close(); err != nil {
				errs <- err
			}
		}(conn)
	}
	wg.Wait()
	for i := 0; i < len(errs); i++ {
		t.Fatal(<-errs)
	}

	reopenedRaw, err := (&Driver{}).Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopenedRaw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseSQLPreparedContextAndColumnTypes(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `CREATE TABLE users (id INT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	stmt, err := database.PrepareContext(ctx, `INSERT INTO users VALUES (?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.ExecContext(ctx, int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := database.QueryContext(ctx, `SELECT id, name FROM users`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 {
		t.Fatalf("column count = %d", len(cols))
	}
	if cols[0].DatabaseTypeName() != "INTEGER" {
		t.Fatalf("database type = %q", cols[0].DatabaseTypeName())
	}
	nullable, ok := cols[0].Nullable()
	if !ok || nullable {
		t.Fatalf("nullable = %v ok=%v", nullable, ok)
	}
	if cols[1].DatabaseTypeName() != "TEXT" {
		t.Fatalf("database type = %q", cols[1].DatabaseTypeName())
	}
}

func TestDatabaseSQLTransactionsAndSavepoints(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO items VALUES (1, 'kept')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT s1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO items VALUES (2, 'rolled')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO s1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	tx, err = database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO items VALUES (3, 'rollback')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count after rollback = %d, want 1", count)
	}
}

func TestDatabaseSQLBeginTxOptions(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); !errors.Is(err, db.ErrReadOnlyUnsupported) {
		t.Fatalf("read-only err = %v, want ErrReadOnlyUnsupported", err)
	}
}

func TestDatabaseSQLNamedParameters(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `CREATE TABLE named_params (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO named_params VALUES (:id, :name)`, sql.Named("id", int64(1)), sql.Named("name", "Ada")); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := database.QueryRowContext(ctx, `SELECT name FROM named_params WHERE id = :id`, sql.Named("id", int64(1))).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Ada" {
		t.Fatalf("name = %q", name)
	}
}

func TestDatabaseSQLTextMarshalerBinding(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE marshaled_values (value ANY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO marshaled_values VALUES (?)`, driverTextValue("uuid")); err != nil {
		t.Fatal(err)
	}
	var value, storageClass string
	if err := database.QueryRow(`SELECT value, typeof(value) FROM marshaled_values`).Scan(&value, &storageClass); err != nil {
		t.Fatal(err)
	}
	if value != "typed:uuid" || storageClass != "text" {
		t.Fatalf("value=%q storageClass=%q; want typed text", value, storageClass)
	}
}
