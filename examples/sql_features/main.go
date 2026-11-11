// Command sql_features shows selected higher-level SQL features supported by
// the custom engine: CTEs, window functions, JSON functions, triggers, and FTS5.
package main

import (
	"context"
	"fmt"
	"log"

	cqlite "github.com/oarkflow/cqlite"
)

func main() {
	ctx := context.Background()
	db, err := cqlite.Open(":memory:", cqlite.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	statements := []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT, amount REAL, metadata TEXT)`,
		`INSERT INTO orders VALUES (1, 'Ada', 30.5, '{"region":"west"}')`,
		`INSERT INTO orders VALUES (2, 'Ada', 18.0, '{"region":"west"}')`,
		`INSERT INTO orders VALUES (3, 'Grace', 42.0, '{"region":"east"}')`,
		`CREATE TABLE audit (message TEXT)`,
		`CREATE TRIGGER orders_audit AFTER INSERT ON orders BEGIN INSERT INTO audit VALUES ('order added'); END`,
		`CREATE VIRTUAL TABLE docs USING fts5(title, body)`,
		`INSERT INTO docs VALUES ('WAL guide', 'durable writes and recovery')`,
		`INSERT INTO docs VALUES ('Query guide', 'indexes and fast lookups')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(ctx, statement); err != nil {
			log.Fatal(err)
		}
	}

	show := func(label, query string, args ...any) {
		rows, err := db.Query(ctx, query, args...)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(label)
		for {
			row, ok := rows.Next()
			if !ok {
				break
			}
			fmt.Println(" ", row)
		}
	}

	show("CTE + window ranking + JSON extraction:", `
		WITH ranked AS (
			SELECT customer, amount,
			       row_number() OVER (PARTITION BY customer ORDER BY amount DESC) AS position,
			       json_extract(metadata, '$.region') AS region
			FROM orders
		)
		SELECT customer, amount, position, region FROM ranked ORDER BY customer, position`)

	if _, err := db.Exec(ctx, `INSERT INTO orders VALUES (4, 'Lin', 12.0, '{"region":"north"}')`); err != nil {
		log.Fatal(err)
	}
	show("Trigger side effect:", `SELECT message FROM audit`)
	show("Full-text search:", `SELECT title FROM docs WHERE docs MATCH 'durable'`)
}
