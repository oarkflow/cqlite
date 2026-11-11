// Command data_types shows common Go values stored with SQLite storage classes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	sqlite "github.com/oarkflow/sqlite"
)

type Profile struct {
	Nickname string `json:"nickname"`
	Active   bool   `json:"active"`
}

func main() {
	ctx := context.Background()
	db, err := sqlite.Open(":memory:", sqlite.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(ctx, `CREATE TABLE samples (
		id INTEGER PRIMARY KEY, label TEXT, score REAL, enabled INTEGER,
		payload BLOB, created_at TEXT, metadata TEXT, optional TEXT
	)`)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE profiles (nickname TEXT, active INTEGER)`); err != nil {
		log.Fatal(err)
	}

	profile := Profile{Nickname: "ada", Active: true}
	if _, err := db.Exec(ctx, `INSERT INTO profiles (nickname, active) VALUES :profile`, sqlite.Named("profile", profile)); err != nil {
		log.Fatal(err)
	}
	metadata, err := json.Marshal(profile)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO samples VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		int64(1), "sample", float64(98.5), true, []byte{0, 1, 2, 255},
		time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC), string(metadata), nil)
	if err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(ctx, `SELECT id, label, score, enabled, payload, created_at, metadata, optional FROM samples`)
	if err != nil {
		log.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok {
		log.Fatal("expected one row")
	}
	fmt.Printf("id=%T(%v) label=%T(%v) score=%T(%v) enabled=%T(%v)\n", row[0], row[0], row[1], row[1], row[2], row[2], row[3], row[3])
	fmt.Printf("blob=%T(%v) timestamp=%T(%v) json=%s optional=%v\n", row[4], row[4], row[5], row[5], row[6], row[7])
}
