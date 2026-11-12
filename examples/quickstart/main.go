// Command quickstart shows a small file-backed database lifecycle.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	cqlite "github.com/oarkflow/cqlite"
)

func main() {
	ctx := context.Background()
	dir := "./data/"
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		log.Fatal(err)
	}
	// defer os.RemoveAll(dir)

	db, err := cqlite.Open(filepath.Join(dir, "app.db"), cqlite.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(ctx, `CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		email TEXT UNIQUE,
		profile JSON
	)`); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users(name, email, profile) VALUES (?, ?, ?)`, "Ada", "ada@example.test", `{"role":"admin","active":true}`); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users(name, email, profile) VALUES (:name, :email, :profile)`,
		cqlite.Named("name", "Grace"), cqlite.Named("email", "grace@example.test"), cqlite.Named("profile", map[string]any{"role": "editor", "active": true})); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE users SET profile = json_set(profile, '$.role', 'writer') WHERE json_extract(profile, '$.role') = 'editor'`); err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(ctx, `SELECT id, name, email, json_extract(profile, '$.role') FROM users WHERE name <> :skip ORDER BY id`, cqlite.Named("skip", "Nobody"))
	if err != nil {
		log.Fatal(err)
	}
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		fmt.Printf("id=%v name=%v email=%v role=%v\n", row[0], row[1], row[2], row[3])
	}
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE json_extract(profile, '$.role') = 'writer'`); err != nil {
		log.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		log.Fatal(err)
	}
}
