package driver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"unsafe"

	db "github.com/oarkflow/cqlite"
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

func TestDatabaseSQLCompositePrimaryKeyPrefixWithOrderBy(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `CREATE TABLE tenant_policies (
		tenant_id TEXT NOT NULL, rev INTEGER NOT NULL, body TEXT NOT NULL,
		PRIMARY KEY (tenant_id, rev)
	)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		tenant string
		rev    int
		body   string
	}{
		{"t1", 1, "first"}, {"t1", 2, "latest"}, {"t2", 1, "other"},
	} {
		if _, err := database.ExecContext(ctx, `INSERT INTO tenant_policies VALUES (?, ?, ?)`, row.tenant, row.rev, row.body); err != nil {
			t.Fatal(err)
		}
	}
	var body string
	var rev int
	if err := database.QueryRowContext(ctx, `SELECT body, rev FROM tenant_policies WHERE tenant_id = ? ORDER BY rev DESC LIMIT 1`, "t1").Scan(&body, &rev); err != nil {
		t.Fatal(err)
	}
	if body != "latest" || rev != 2 {
		t.Fatalf("got (%q, %d), want (%q, 2)", body, rev, "latest")
	}
}

func TestDatabaseSQLAndPredicateBindsInOrder(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `CREATE TABLE scoped_rows (id TEXT PRIMARY KEY, subject_id TEXT NOT NULL, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO scoped_rows VALUES (?, ?, ?)`, "row-1", "subject-1", "found"); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := database.QueryRowContext(ctx, `SELECT value FROM scoped_rows WHERE id = ? AND subject_id = ?`, "row-1", "subject-1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "found" {
		t.Fatalf("value = %q, want found", value)
	}
}

func TestDatabaseSQLWhereAndLimitParametersKeepOrder(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `CREATE TABLE due_rows (id TEXT PRIMARY KEY, status TEXT, due_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO due_rows VALUES (?, ?, ?)`, "d1", "pending", "2026-11-12T21:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := database.QueryRowContext(ctx, `SELECT id FROM due_rows WHERE status = 'pending' AND due_at <= ? ORDER BY due_at LIMIT ?`, "2026-11-13T00:00:00Z", 1).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != "d1" {
		t.Fatalf("id = %q, want d1", id)
	}
}

func TestDatabaseSQLPartialUniqueIndex(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `CREATE TABLE accounts (tenant_id TEXT NOT NULL, phone_bidx TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `CREATE UNIQUE INDEX accounts_phone ON accounts (tenant_id, phone_bidx) WHERE phone_bidx <> ''`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := database.ExecContext(ctx, `INSERT INTO accounts VALUES (?, ?)`, "t1", ""); err != nil {
			t.Fatalf("insert empty phone %d: %v", i, err)
		}
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO accounts VALUES (?, ?)`, "t1", "phone-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO accounts VALUES (?, ?)`, "t1", "phone-hash"); err == nil {
		t.Fatal("duplicate indexed phone was accepted")
	}
}

func TestDatabaseSQLVacuumInto(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	destination := filepath.Join(dir, "snapshot.db")
	database, err := sql.Open(Name, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE snapshot_rows (id INTEGER PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO snapshot_rows VALUES (1, 'kept')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`VACUUM INTO ?`, destination); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sql.Open(Name, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var value string
	if err := snapshot.QueryRow(`SELECT value FROM snapshot_rows WHERE id=1`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "kept" {
		t.Fatalf("snapshot value = %q, want kept", value)
	}
	if _, err := database.Exec(`VACUUM INTO ?`, destination); err == nil {
		t.Fatal("VACUUM INTO overwrote an existing destination")
	}
}

func TestDatabaseSQLIntegrityCheck(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE integrity_rows (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result != "ok" {
		t.Fatalf("integrity_check = %q, want ok", result)
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

func TestDatabaseSQLCopiesBoundTextValues(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE text_values (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	buffer := []byte("case-id")
	aliased := unsafe.String(&buffer[0], len(buffer))
	if _, err := database.Exec(`INSERT INTO text_values VALUES (?)`, aliased); err != nil {
		t.Fatal(err)
	}
	copy(buffer, []byte("mutated"))
	var got string
	if err := database.QueryRow(`SELECT value FROM text_values`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "case-id" {
		t.Fatalf("stored value = %q, want case-id", got)
	}
}

func TestDatabaseSQLPackedFileStableAcrossReadOnlyOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stable.db")
	database, err := sql.Open(Name, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE stable_rows (id INTEGER PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO stable_rows VALUES (1, 'kept')`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(original)
	for i := 0; i < 3; i++ {
		database, err := sql.Open(Name, path)
		if err != nil {
			t.Fatal(err)
		}
		var value string
		if err := database.QueryRow(`SELECT value FROM stable_rows WHERE id=1`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != "kept" {
			t.Fatalf("value = %q, want kept", value)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(got); sum != want {
			t.Fatalf("archive changed after read-only open %d", i+1)
		}
	}
}
