// Command transactions demonstrates commit and rollback with the embedded API.
package main

import (
	"context"
	"fmt"
	"log"

	sqlite "github.com/sujit/sqlite"
)

func main() {
	ctx := context.Background()
	db, err := sqlite.Open(":memory:", sqlite.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `CREATE TABLE ledger (id INTEGER PRIMARY KEY, account TEXT, cents INTEGER)`); err != nil {
		log.Fatal(err)
	}

	tx, err := db.Begin(ctx, sqlite.TxOptions{})
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range []struct {
		id      int64
		account string
		cents   int64
	}{{1, "checking", -2500}, {2, "savings", 2500}} {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger VALUES (?, ?, ?)`, entry.id, entry.account, entry.cents); err != nil {
			_ = tx.Rollback()
			log.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}

	rollback, err := db.Begin(ctx, sqlite.TxOptions{})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := rollback.Exec(ctx, `INSERT INTO ledger VALUES (?, ?, ?)`, int64(3), "checking", int64(-999)); err != nil {
		log.Fatal(err)
	}
	if err := rollback.Rollback(); err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(ctx, `SELECT account, sum(cents) FROM ledger GROUP BY account ORDER BY account`)
	if err != nil {
		log.Fatal(err)
	}
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		fmt.Printf("%s balance: %d cents\n", row[0], row[1])
	}
}
