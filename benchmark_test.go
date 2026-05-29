package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

const benchRows = 1000

func BenchmarkSQLiteComparison(b *testing.B) {
	b.Run("custom/insert_tx", benchCustomInsertTx)
	b.Run("mattn/insert_tx", benchMattnInsertTx)
	b.Run("custom/insert_autocommit", benchCustomInsertAutocommit)
	b.Run("mattn/insert_autocommit", benchMattnInsertAutocommit)
	b.Run("custom/point_lookup_pk", benchCustomPointLookupPK)
	b.Run("mattn/point_lookup_pk", benchMattnPointLookupPK)
	b.Run("custom/indexed_lookup", benchCustomIndexedLookup)
	b.Run("mattn/indexed_lookup", benchMattnIndexedLookup)
	b.Run("custom/full_scan", benchCustomFullScan)
	b.Run("mattn/full_scan", benchMattnFullScan)
	b.Run("custom/update_by_pk", benchCustomUpdateByPK)
	b.Run("mattn/update_by_pk", benchMattnUpdateByPK)
	b.Run("custom/join_aggregate", benchCustomJoinAggregate)
	b.Run("mattn/join_aggregate", benchMattnJoinAggregate)
}

func BenchmarkSQLiteThroughputComparison(b *testing.B) {
	b.Run("custom_fast/insert_autocommit", func(b *testing.B) {
		benchCustomInsertAutocommitWithOptions(b, Options{SyncPolicy: SyncOff})
	})
	b.Run("mattn_fast/insert_autocommit", func(b *testing.B) {
		benchMattnInsertAutocommitWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	})
	b.Run("custom_fast/update_by_pk", func(b *testing.B) {
		benchCustomUpdateByPKWithOptions(b, Options{SyncPolicy: SyncOff})
	})
	b.Run("mattn_fast/update_by_pk", func(b *testing.B) {
		benchMattnUpdateByPKWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	})
}

func benchCustomInsertTx(b *testing.B) {
	ctx := context.Background()
	d := openBenchCustom(b)
	mustCustomExec(b, d, `CREATE TABLE bench_insert_tx (id INTEGER PRIMARY KEY, name TEXT, score INTEGER, payload JSON)`)
	b.ReportAllocs()
	b.ResetTimer()
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO bench_insert_tx VALUES (?, ?, ?, ?)`, int64(i+1), fmt.Sprintf("name-%d", i), int64(i%100), `{"ok":true}`); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}

func benchMattnInsertTx(b *testing.B) {
	d := openBenchMattn(b)
	mustSQLExec(b, d, `CREATE TABLE bench_insert_tx (id INTEGER PRIMARY KEY, name TEXT, score INTEGER, payload JSON)`)
	b.ReportAllocs()
	b.ResetTimer()
	tx, err := d.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO bench_insert_tx VALUES (?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(int64(i+1), fmt.Sprintf("name-%d", i), int64(i%100), `{"ok":true}`); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}

func benchCustomInsertAutocommit(b *testing.B) {
	benchCustomInsertAutocommitWithOptions(b, Options{SyncPolicy: SyncFull})
}

func benchCustomInsertAutocommitWithOptions(b *testing.B, opts Options) {
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, opts)
	mustCustomExec(b, d, `CREATE TABLE bench_insert_auto (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(ctx, `INSERT INTO bench_insert_auto VALUES (?, ?, ?)`, int64(i+1), fmt.Sprintf("name-%d", i), int64(i%100)); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnInsertAutocommit(b *testing.B) {
	benchMattnInsertAutocommitWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = FULL`)
}

func benchMattnInsertAutocommitWithPragmas(b *testing.B, pragmas ...string) {
	d := openBenchMattnWithPragmas(b, pragmas...)
	mustSQLExec(b, d, `CREATE TABLE bench_insert_auto (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	stmt, err := d.Prepare(`INSERT INTO bench_insert_auto VALUES (?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(int64(i+1), fmt.Sprintf("name-%d", i), int64(i%100)); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomPointLookupPK(b *testing.B) {
	ctx := context.Background()
	d := seedCustomBench(b, benchRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%benchRows + 1)
		rows, err := d.Query(ctx, `SELECT name, score FROM items WHERE id = ?`, id)
		if err != nil {
			b.Fatal(err)
		}
		if row, ok := rows.Next(); !ok || row[0] == nil {
			b.Fatalf("missing row for id %d", id)
		}
	}
}

func benchMattnPointLookupPK(b *testing.B) {
	d := seedMattnBench(b, benchRows)
	stmt, err := d.Prepare(`SELECT name, score FROM items WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%benchRows + 1)
		var name string
		var score int64
		if err := stmt.QueryRow(id).Scan(&name, &score); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomIndexedLookup(b *testing.B) {
	ctx := context.Background()
	d := seedCustomBench(b, benchRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := fmt.Sprintf("name-%03d", i%100)
		rows, err := d.Query(ctx, `SELECT id FROM items WHERE name = ?`, name)
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, ok := rows.Next(); !ok {
				break
			}
		}
	}
}

func benchMattnIndexedLookup(b *testing.B) {
	d := seedMattnBench(b, benchRows)
	stmt, err := d.Prepare(`SELECT id FROM items WHERE name = ?`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := fmt.Sprintf("name-%03d", i%100)
		rows, err := stmt.Query(name)
		if err != nil {
			b.Fatal(err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				b.Fatal(err)
			}
		}
		_ = rows.Close()
	}
}

func benchCustomFullScan(b *testing.B) {
	ctx := context.Background()
	d := seedCustomBench(b, benchRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := d.Query(ctx, `SELECT id, name, score FROM items WHERE score >= 0`)
		if err != nil {
			b.Fatal(err)
		}
		var n int
		for {
			if _, ok := rows.Next(); !ok {
				break
			}
			n++
		}
		if n != benchRows {
			b.Fatalf("scan rows=%d", n)
		}
	}
}

func benchMattnFullScan(b *testing.B) {
	d := seedMattnBench(b, benchRows)
	stmt, err := d.Prepare(`SELECT id, name, score FROM items WHERE score >= 0`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := stmt.Query()
		if err != nil {
			b.Fatal(err)
		}
		var n int
		for rows.Next() {
			var id, score int64
			var name string
			if err := rows.Scan(&id, &name, &score); err != nil {
				b.Fatal(err)
			}
			n++
		}
		_ = rows.Close()
		if n != benchRows {
			b.Fatalf("scan rows=%d", n)
		}
	}
}

func benchCustomUpdateByPK(b *testing.B) {
	benchCustomUpdateByPKWithOptions(b, Options{SyncPolicy: SyncFull})
}

func benchCustomUpdateByPKWithOptions(b *testing.B, opts Options) {
	ctx := context.Background()
	d := seedCustomBenchWithOptions(b, benchRows, opts)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%benchRows + 1)
		if _, err := d.Exec(ctx, `UPDATE items SET score = score + 1 WHERE id = ?`, id); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnUpdateByPK(b *testing.B) {
	benchMattnUpdateByPKWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = FULL`)
}

func benchMattnUpdateByPKWithPragmas(b *testing.B, pragmas ...string) {
	d := seedMattnBenchWithPragmas(b, benchRows, pragmas...)
	stmt, err := d.Prepare(`UPDATE items SET score = score + 1 WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%benchRows + 1)
		if _, err := stmt.Exec(id); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomJoinAggregate(b *testing.B) {
	ctx := context.Background()
	d := seedCustomBench(b, benchRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := d.Query(ctx, `SELECT groups.name, count(*), sum(items.score) FROM items JOIN groups ON items.group_id = groups.id GROUP BY groups.name ORDER BY groups.name`)
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, ok := rows.Next(); !ok {
				break
			}
		}
	}
}

func benchMattnJoinAggregate(b *testing.B) {
	d := seedMattnBench(b, benchRows)
	stmt, err := d.Prepare(`SELECT groups.name, count(*), sum(items.score) FROM items JOIN groups ON items.group_id = groups.id GROUP BY groups.name ORDER BY groups.name`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := stmt.Query()
		if err != nil {
			b.Fatal(err)
		}
		for rows.Next() {
			var name string
			var count int64
			var sum float64
			if err := rows.Scan(&name, &count, &sum); err != nil {
				b.Fatal(err)
			}
		}
		_ = rows.Close()
	}
}

func seedCustomBench(b *testing.B, n int) *DB {
	return seedCustomBenchWithOptions(b, n, Options{SyncPolicy: SyncFull})
}

func seedCustomBenchWithOptions(b *testing.B, n int, opts Options) *DB {
	b.Helper()
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, opts)
	mustCustomExec(b, d, `CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT)`)
	mustCustomExec(b, d, `CREATE TABLE items (id INTEGER PRIMARY KEY, group_id INTEGER, name TEXT, score INTEGER, payload JSON)`)
	mustCustomExec(b, d, `CREATE INDEX items_name_idx ON items (name)`)
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO groups VALUES (?, ?)`, int64(i+1), fmt.Sprintf("group-%02d", i)); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO items VALUES (?, ?, ?, ?, ?)`, int64(i+1), int64(i%10+1), fmt.Sprintf("name-%03d", i%100), int64(i%100), `{"bench":true}`); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return d
}

func seedMattnBench(b *testing.B, n int) *sql.DB {
	return seedMattnBenchWithPragmas(b, n, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = FULL`)
}

func seedMattnBenchWithPragmas(b *testing.B, n int, pragmas ...string) *sql.DB {
	b.Helper()
	d := openBenchMattnWithPragmas(b, pragmas...)
	mustSQLExec(b, d, `CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT)`)
	mustSQLExec(b, d, `CREATE TABLE items (id INTEGER PRIMARY KEY, group_id INTEGER, name TEXT, score INTEGER, payload JSON)`)
	mustSQLExec(b, d, `CREATE INDEX items_name_idx ON items (name)`)
	tx, err := d.Begin()
	if err != nil {
		b.Fatal(err)
	}
	groupStmt, err := tx.Prepare(`INSERT INTO groups VALUES (?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := groupStmt.Exec(int64(i+1), fmt.Sprintf("group-%02d", i)); err != nil {
			b.Fatal(err)
		}
	}
	_ = groupStmt.Close()
	itemStmt, err := tx.Prepare(`INSERT INTO items VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := itemStmt.Exec(int64(i+1), int64(i%10+1), fmt.Sprintf("name-%03d", i%100), int64(i%100), `{"bench":true}`); err != nil {
			b.Fatal(err)
		}
	}
	_ = itemStmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return d
}

func openBenchCustom(b *testing.B) *DB {
	return openBenchCustomWithOptions(b, Options{SyncPolicy: SyncFull})
}

func openBenchCustomWithOptions(b *testing.B, opts Options) *DB {
	b.Helper()
	d, err := Open(filepath.Join(b.TempDir(), "custom"), opts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = d.Close()
	})
	return d
}

func openBenchMattn(b *testing.B) *sql.DB {
	return openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = FULL`)
}

func openBenchMattnWithPragmas(b *testing.B, pragmas ...string) *sql.DB {
	b.Helper()
	path := filepath.Join(b.TempDir(), "sqlite.db")
	d, err := sql.Open("sqlite3", path)
	if err != nil {
		b.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	for _, pragma := range pragmas {
		mustSQLExec(b, d, pragma)
	}
	mustSQLExec(b, d, `PRAGMA foreign_keys = ON`)
	b.Cleanup(func() {
		_ = d.Close()
	})
	return d
}

func mustCustomExec(b *testing.B, d *DB, query string, args ...any) {
	b.Helper()
	if _, err := d.Exec(context.Background(), query, args...); err != nil {
		b.Fatalf("custom exec %q: %v", query, err)
	}
}

func mustSQLExec(b *testing.B, d *sql.DB, query string, args ...any) {
	b.Helper()
	if _, err := d.Exec(query, args...); err != nil {
		b.Fatalf("sqlite exec %q: %v", query, err)
	}
}
