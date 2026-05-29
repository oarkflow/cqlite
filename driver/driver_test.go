package driver

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	db "github.com/sujit/sqlite"
)

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
