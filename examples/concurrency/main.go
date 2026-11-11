// Command concurrency shares one database handle among readers and writers.
package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	sqlite "github.com/oarkflow/sqlite"
)

func main() {
	ctx := context.Background()
	db, err := sqlite.Open(":memory:", sqlite.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `CREATE TABLE events (id INTEGER PRIMARY KEY, message TEXT NOT NULL)`); err != nil {
		log.Fatal(err)
	}

	const writers, perWriter, readers = 4, 25, 3
	start := make(chan struct{})
	errs := make(chan error, writers+readers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			<-start
			for n := 0; n < perWriter; n++ {
				id := int64(writer*perWriter + n + 1)
				if _, err := db.Exec(ctx, `INSERT INTO events VALUES (:id, :message)`, sqlite.Named("id", id), sqlite.Named("message", fmt.Sprintf("writer-%d item-%d", writer, n))); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < 10; n++ {
				rows, err := db.Query(ctx, `SELECT count(*) FROM events`)
				if err != nil {
					errs <- err
					return
				}
				if _, ok := rows.Next(); !ok {
					errs <- fmt.Errorf("count query returned no rows")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		log.Fatal(err)
	}

	rows, err := db.Query(ctx, `SELECT count(*) FROM events`)
	if err != nil {
		log.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok {
		log.Fatal("count query returned no rows")
	}
	fmt.Printf("inserted %v rows with %d concurrent readers\n", row[0], readers)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Query(canceled, `SELECT count(*) FROM events`); err != context.Canceled {
		log.Fatalf("canceled query error = %v, want context.Canceled", err)
	}
	fmt.Println("canceled query stopped with context.Canceled")
}
