// Command quickstart shows a small file-backed database lifecycle.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	sqlite "github.com/sujit/sqlite"
)

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "sqlite-quickstart-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := sqlite.Open(filepath.Join(dir, "app.db"), sqlite.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(ctx, `CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		email TEXT UNIQUE
	)`); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users(name, email) VALUES (?, ?)`, "Ada", "ada@example.test"); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users(name, email) VALUES (:name, :email)`,
		sqlite.Named("name", "Grace"), sqlite.Named("email", "grace@example.test")); err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(ctx, `SELECT id, name, email FROM users WHERE name <> :skip ORDER BY id`, sqlite.Named("skip", "Nobody"))
	if err != nil {
		log.Fatal(err)
	}
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		fmt.Printf("id=%v name=%v email=%v\n", row[0], row[1], row[2])
	}
	if err := db.Checkpoint(ctx); err != nil {
		log.Fatal(err)
	}
}
