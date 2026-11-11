// Command database_sql uses the custom engine through database/sql.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/sujit/sqlite/driver"
)

func main() {
	ctx := context.Background()
	file, err := os.CreateTemp("", "sqlite-driver-*.db")
	if err != nil {
		log.Fatal(err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		log.Fatal(err)
	}
	defer os.Remove(path)

	db, err := sql.Open("cqlite", path)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	if _, err := db.ExecContext(ctx, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`); err != nil {
		log.Fatal(err)
	}

	stmt, err := db.PrepareContext(ctx, `INSERT INTO notes(body) VALUES (:body)`)
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()
	if _, err := stmt.ExecContext(ctx, sql.Named("body", "hello from database/sql")); err != nil {
		log.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notes(body) VALUES (?)`, "transactional row"); err != nil {
		_ = tx.Rollback()
		log.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}

	var body string
	if err := db.QueryRowContext(ctx, `SELECT body FROM notes WHERE id = ?`, int64(1)).Scan(&body); err != nil {
		log.Fatal(err)
	}
	fmt.Println(body)
}
