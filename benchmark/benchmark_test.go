package benchmark

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	db "github.com/oarkflow/cqlite"
	customdriver "github.com/oarkflow/cqlite/driver"

	_ "github.com/mattn/go-sqlite3"
)

const benchRows = 1000

func BenchmarkSQLiteComparison(b *testing.B) {
	b.Run("custom/insert_tx", benchCustomInsertTx)
	b.Run("mattn/insert_tx", benchMattnInsertTx)
	b.Run("custom/insert_autocommit", benchCustomInsertAutocommit)
	b.Run("mattn/insert_autocommit", benchMattnInsertAutocommit)
	b.Run("custom/insert_batch", benchCustomInsertBatch)
	b.Run("mattn/insert_batch", benchMattnInsertBatch)
	b.Run("custom/point_lookup_pk", benchCustomPointLookupPK)
	b.Run("mattn/point_lookup_pk", benchMattnPointLookupPK)
	b.Run("custom/indexed_lookup", benchCustomIndexedLookup)
	b.Run("mattn/indexed_lookup", benchMattnIndexedLookup)
	b.Run("custom/range_scan", benchCustomRangeScan)
	b.Run("mattn/range_scan", benchMattnRangeScan)
	b.Run("custom/full_scan", benchCustomFullScan)
	b.Run("mattn/full_scan", benchMattnFullScan)
	b.Run("custom/update_by_pk", benchCustomUpdateByPK)
	b.Run("mattn/update_by_pk", benchMattnUpdateByPK)
	b.Run("custom/update_by_index", benchCustomUpdateByIndex)
	b.Run("mattn/update_by_index", benchMattnUpdateByIndex)
	b.Run("custom/delete_by_pk", benchCustomDeleteByPK)
	b.Run("mattn/delete_by_pk", benchMattnDeleteByPK)
	b.Run("custom/delete_by_index", benchCustomDeleteByIndex)
	b.Run("mattn/delete_by_index", benchMattnDeleteByIndex)
	b.Run("custom/join_aggregate", benchCustomJoinAggregate)
	b.Run("mattn/join_aggregate", benchMattnJoinAggregate)
	b.Run("custom/reopen_recovery", benchCustomReopenRecovery)
	b.Run("mattn/reopen_recovery", benchMattnReopenRecovery)
	b.Run("custom/large_payload", benchCustomLargePayload)
	b.Run("mattn/large_payload", benchMattnLargePayload)
	b.Run("custom/concurrent_mixed", benchCustomConcurrentMixed)
	b.Run("mattn/concurrent_mixed", benchMattnConcurrentMixed)
}

// BenchmarkSQLiteFTSMatch compares selective MATCH queries on a moderately
// sized index. Setup is outside the timed region; neither engine ranks results
// because the query only returns rowid.
func BenchmarkSQLiteFTSMatch(b *testing.B) {
	b.Run("custom", benchCustomFTSMatch)
	b.Run("mattn", benchMattnFTSMatch)
}

func benchCustomFTSMatch(b *testing.B) {
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, db.Options{SyncPolicy: db.SyncOff})
	mustCustomExec(b, d, `CREATE VIRTUAL TABLE ft USING fts5(body)`)
	tx, err := d.Begin(ctx, db.TxOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		body := fmt.Sprintf("common document token-%04d", i)
		if i == 1379 {
			body += " needle"
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO ft(body) VALUES ('%s')`, body)); err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	// The first read can build deferred runtime indexes after a large write
	// transaction. Keep that one-time setup cost out of the steady-state MATCH
	// query measurement; the SQLite setup path builds its index before timing.
	warm, err := d.Query(ctx, `SELECT rowid FROM ft WHERE ft MATCH 'needle'`)
	if err != nil {
		b.Fatal(err)
	}
	if _, ok := warm.Next(); !ok {
		b.Fatal("MATCH returned no row during warmup")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := d.Query(ctx, `SELECT rowid FROM ft WHERE ft MATCH 'needle'`)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := rows.Next(); !ok {
			b.Fatal("MATCH returned no row")
		}
	}
}

func benchMattnFTSMatch(b *testing.B) {
	d := openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	if _, err := d.Exec(`CREATE VIRTUAL TABLE ft USING fts5(body)`); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such module: fts5") {
			b.Skip("mattn/go-sqlite3 was built without FTS5; rerun with -tags sqlite_fts5")
		}
		b.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO ft(body) VALUES (?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		body := fmt.Sprintf("common document token-%04d", i)
		if i == 1379 {
			body += " needle"
		}
		if _, err := stmt.Exec(body); err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := d.Query(`SELECT rowid FROM ft WHERE ft MATCH 'needle'`)
		if err != nil {
			b.Fatal(err)
		}
		if !rows.Next() {
			_ = rows.Close()
			b.Fatal("MATCH returned no row")
		}
		_ = rows.Close()
	}
}

func BenchmarkSQLiteThroughputComparison(b *testing.B) {
	b.Run("custom_fast/insert_autocommit", func(b *testing.B) {
		benchCustomInsertAutocommitWithOptions(b, db.Options{SyncPolicy: db.SyncOff})
	})
	b.Run("mattn_fast/insert_autocommit", func(b *testing.B) {
		benchMattnInsertAutocommitWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	})
	b.Run("custom_fast/update_by_pk", func(b *testing.B) {
		benchCustomUpdateByPKWithOptions(b, db.Options{SyncPolicy: db.SyncOff})
	})
	b.Run("mattn_fast/update_by_pk", func(b *testing.B) {
		benchMattnUpdateByPKWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	})
}

// BenchmarkSQLiteConcurrentFull compares the same database/sql prepared
// statement mix with four connections and FULL durability on both engines.
// It exercises concurrent point reads and serialized autocommit updates.
func BenchmarkSQLiteConcurrentFull(b *testing.B) {
	b.Run("custom", benchCustomConcurrentFull)
	b.Run("mattn", benchMattnConcurrentFull)
}

func benchCustomConcurrentFull(b *testing.B) {
	d, err := sql.Open(customdriver.Name, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	d.SetMaxOpenConns(4)
	d.SetMaxIdleConns(4)
	defer d.Close()
	seedConcurrentFull(b, d)
	warmConcurrentFullConnections(b, d)
	benchConcurrentFullStatements(b, d)
}

func benchMattnConcurrentFull(b *testing.B) {
	d := openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = FULL`, `PRAGMA busy_timeout = 5000`)
	d.SetMaxOpenConns(4)
	d.SetMaxIdleConns(4)
	seedConcurrentFull(b, d)
	warmConcurrentFullConnections(b, d)
	benchConcurrentFullStatements(b, d)
}

func warmConcurrentFullConnections(b *testing.B, d *sql.DB) {
	b.Helper()
	conns := make([]*sql.Conn, 0, 4)
	for i := 0; i < cap(conns); i++ {
		conn, err := d.Conn(context.Background())
		if err != nil {
			for _, openConn := range conns {
				_ = openConn.Close()
			}
			b.Fatal(err)
		}
		conns = append(conns, conn)
		var score int64
		if err := conn.QueryRowContext(context.Background(), `SELECT score FROM concurrent_full WHERE id = ?`, int64(1)).Scan(&score); err != nil {
			for _, openConn := range conns {
				_ = openConn.Close()
			}
			b.Fatalf("warm connection %d cannot read seeded row: %v", i, err)
		}
		if score != 0 {
			b.Fatalf("warm connection %d read score %d, want 0", i, score)
		}
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func seedConcurrentFull(b *testing.B, d *sql.DB) {
	b.Helper()
	if _, err := d.Exec(`CREATE TABLE concurrent_full (id INTEGER PRIMARY KEY, score INTEGER)`); err != nil {
		b.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO concurrent_full VALUES (?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	for i := 0; i < benchRows; i++ {
		if _, err := stmt.Exec(int64(i+1), int64(i)); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}

func benchConcurrentFullStatements(b *testing.B, d *sql.DB) {
	b.Helper()
	readStmt, err := d.Prepare(`SELECT score FROM concurrent_full WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer readStmt.Close()
	writeStmt, err := d.Prepare(`UPDATE concurrent_full SET score = score + 1 WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer writeStmt.Close()
	var seq uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddUint64(&seq, 1)
			id := int64(n%benchRows + 1)
			if n%20 == 0 {
				if _, err := writeStmt.Exec(id); err != nil {
					b.Fatal(err)
				}
				continue
			}
			var score int64
			if err := readStmt.QueryRow(id).Scan(&score); err != nil {
				var count int64
				countErr := d.QueryRow(`SELECT COUNT(*) FROM concurrent_full`).Scan(&count)
				b.Fatalf("read id %d: %v (count=%d, count error=%v)", id, err, count, countErr)
			}
		}
	})
}

// BenchmarkSQLiteSyncNormalComparison compares WAL autocommit throughput with
// both engines using NORMAL synchronization, separate from the default FULL
// durability comparison above.
func BenchmarkSQLiteSyncNormalComparison(b *testing.B) {
	b.Run("custom/insert_autocommit", func(b *testing.B) {
		benchCustomInsertAutocommitWithOptions(b, db.Options{Synchronous: "NORMAL"})
	})
	b.Run("mattn/insert_autocommit", func(b *testing.B) {
		benchMattnInsertAutocommitWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = NORMAL`)
	})
}

// BenchmarkSQLitePreparedFullInsert compares the same database/sql prepared
// statement path on each engine with FULL durability enabled.
func BenchmarkSQLitePreparedFullInsert(b *testing.B) {
	b.Run("custom", benchPreparedCustomFullInsert)
	b.Run("mattn", benchPreparedMattnFullInsert)
}

func benchPreparedCustomFullInsert(b *testing.B) {
	d, err := sql.Open(customdriver.Name, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`CREATE TABLE prepared_full (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`); err != nil {
		b.Fatal(err)
	}
	stmt, err := d.Prepare(`INSERT INTO prepared_full VALUES (?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(int64(i+1), "name", int64(i%100)); err != nil {
			b.Fatal(err)
		}
	}
}

func benchPreparedMattnFullInsert(b *testing.B) {
	d := openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = FULL`)
	mustSQLExec(b, d, `CREATE TABLE prepared_full (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	stmt, err := d.Prepare(`INSERT INTO prepared_full VALUES (?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(int64(i+1), "name", int64(i%100)); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomInsertTx(b *testing.B) {
	ctx := context.Background()
	d := openBenchCustom(b)
	mustCustomExec(b, d, `CREATE TABLE bench_insert_tx (id INTEGER PRIMARY KEY, name TEXT, score INTEGER, payload JSON)`)
	b.ReportAllocs()
	b.ResetTimer()
	tx, err := d.Begin(ctx, db.TxOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO bench_insert_tx VALUES (?, ?, ?, ?)`, int64(i+1), fmt.Sprintf("name-%d", i), int64(i%100), `{"ok":true}`); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
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
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
}

func benchCustomInsertAutocommit(b *testing.B) {
	benchCustomInsertAutocommitWithOptions(b, db.Options{SyncPolicy: db.SyncFull})
}

func benchCustomInsertAutocommitWithOptions(b *testing.B, opts db.Options) {
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

func benchCustomInsertBatch(b *testing.B) {
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, db.Options{SyncPolicy: db.SyncOff})
	mustCustomExec(b, d, `CREATE TABLE bench_insert_batch (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	const batchSize = 100
	b.ReportAllocs()
	b.ResetTimer()
	nextID := int64(1)
	for i := 0; i < b.N; i++ {
		tx, err := d.Begin(ctx, db.TxOptions{})
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < batchSize; j++ {
			id := nextID
			nextID++
			if _, err := tx.Exec(ctx, `INSERT INTO bench_insert_batch VALUES (?, ?, ?)`, id, fmt.Sprintf("name-%d", id), id%100); err != nil {
				_ = tx.Rollback()
				b.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnInsertBatch(b *testing.B) {
	d := openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	mustSQLExec(b, d, `CREATE TABLE bench_insert_batch (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	const batchSize = 100
	b.ReportAllocs()
	b.ResetTimer()
	nextID := int64(1)
	for i := 0; i < b.N; i++ {
		tx, err := d.Begin()
		if err != nil {
			b.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT INTO bench_insert_batch VALUES (?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
		for j := 0; j < batchSize; j++ {
			id := nextID
			nextID++
			if _, err := stmt.Exec(id, fmt.Sprintf("name-%d", id), id%100); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				b.Fatal(err)
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
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

func benchCustomRangeScan(b *testing.B) {
	ctx := context.Background()
	d := seedCustomBench(b, benchRows)
	mustCustomExec(b, d, `CREATE INDEX items_score_idx ON items (score)`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo := int64(i % 90)
		rows, err := d.Query(ctx, `SELECT id FROM items WHERE score >= ? AND score < ?`, lo, lo+10)
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

func benchMattnRangeScan(b *testing.B) {
	d := seedMattnBench(b, benchRows)
	mustSQLExec(b, d, `CREATE INDEX items_score_idx ON items (score)`)
	stmt, err := d.Prepare(`SELECT id FROM items WHERE score >= ? AND score < ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo := int64(i % 90)
		rows, err := stmt.Query(lo, lo+10)
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

func benchCustomReopenRecovery(b *testing.B) {
	ctx := context.Background()
	dir := filepath.Join(b.TempDir(), "custom")
	d, err := db.Open(dir, db.Options{SyncPolicy: db.SyncFull})
	if err != nil {
		b.Fatal(err)
	}
	mustCustomExec(b, d, `CREATE TABLE reopen_items (id INTEGER PRIMARY KEY, msg TEXT)`)
	for i := 0; i < 100; i++ {
		mustCustomExec(b, d, `INSERT INTO reopen_items VALUES (?, ?)`, int64(i+1), fmt.Sprintf("msg-%d", i))
	}
	if err := d.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reopened, err := db.Open(dir, db.Options{SyncPolicy: db.SyncFull})
		if err != nil {
			b.Fatal(err)
		}
		rows, err := reopened.Query(ctx, `SELECT msg FROM reopen_items WHERE id = 50`)
		if err != nil {
			b.Fatal(err)
		}
		if row, ok := rows.Next(); !ok || row[0] == nil {
			b.Fatal("missing reopen row")
		}
		if err := reopened.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnReopenRecovery(b *testing.B) {
	path := filepath.Join(b.TempDir(), "sqlite.db")
	d, err := sql.Open("sqlite3", path)
	if err != nil {
		b.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	mustSQLExec(b, d, `PRAGMA journal_mode = WAL`)
	mustSQLExec(b, d, `PRAGMA synchronous = FULL`)
	mustSQLExec(b, d, `CREATE TABLE reopen_items (id INTEGER PRIMARY KEY, msg TEXT)`)
	for i := 0; i < 100; i++ {
		mustSQLExec(b, d, `INSERT INTO reopen_items VALUES (?, ?)`, int64(i+1), fmt.Sprintf("msg-%d", i))
	}
	_ = d.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reopened, err := sql.Open("sqlite3", path)
		if err != nil {
			b.Fatal(err)
		}
		reopened.SetMaxOpenConns(1)
		var msg string
		if err := reopened.QueryRow(`SELECT msg FROM reopen_items WHERE id = 50`).Scan(&msg); err != nil {
			b.Fatal(err)
		}
		if err := reopened.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomLargePayload(b *testing.B) {
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, db.Options{SyncPolicy: db.SyncOff})
	mustCustomExec(b, d, `CREATE TABLE large_payloads (id INTEGER PRIMARY KEY, body TEXT, raw BLOB, meta JSON)`)
	body := strings.Repeat("text-", 2048)
	raw := []byte(strings.Repeat("blob-", 2048))
	meta := `{"payload":"` + strings.Repeat("json", 1024) + `"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(ctx, `INSERT INTO large_payloads VALUES (?, ?, ?, ?)`, int64(i+1), body, raw, meta); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnLargePayload(b *testing.B) {
	d := openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	mustSQLExec(b, d, `CREATE TABLE large_payloads (id INTEGER PRIMARY KEY, body TEXT, raw BLOB, meta JSON)`)
	stmt, err := d.Prepare(`INSERT INTO large_payloads VALUES (?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	body := strings.Repeat("text-", 2048)
	raw := []byte(strings.Repeat("blob-", 2048))
	meta := `{"payload":"` + strings.Repeat("json", 1024) + `"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(int64(i+1), body, raw, meta); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomConcurrentMixed(b *testing.B) {
	ctx := context.Background()
	d := seedCustomBenchWithOptions(b, benchRows, db.Options{SyncPolicy: db.SyncOff})
	var seq uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddUint64(&seq, 1)
			id := int64(n%benchRows + 1)
			if n%20 == 0 {
				if _, err := d.Exec(ctx, `UPDATE items SET score = score + 1 WHERE id = ?`, id); err != nil {
					b.Fatal(err)
				}
				continue
			}
			rows, err := d.Query(ctx, `SELECT score FROM items WHERE id = ?`, id)
			if err != nil {
				b.Fatal(err)
			}
			if _, ok := rows.Next(); !ok {
				b.Fatal("missing custom row")
			}
		}
	})
}

func benchMattnConcurrentMixed(b *testing.B) {
	d := seedMattnBenchWithPragmas(b, benchRows, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`, `PRAGMA busy_timeout = 5000`)
	d.SetMaxOpenConns(4)
	readStmt, err := d.Prepare(`SELECT score FROM items WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer readStmt.Close()
	writeStmt, err := d.Prepare(`UPDATE items SET score = score + 1 WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer writeStmt.Close()
	var seq uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddUint64(&seq, 1)
			id := int64(n%benchRows + 1)
			if n%20 == 0 {
				if _, err := writeStmt.Exec(id); err != nil {
					b.Fatal(err)
				}
				continue
			}
			var score int64
			if err := readStmt.QueryRow(id).Scan(&score); err != nil {
				b.Fatal(err)
			}
		}
	})
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
	benchCustomUpdateByPKWithOptions(b, db.Options{SyncPolicy: db.SyncFull})
}

func benchCustomUpdateByPKWithOptions(b *testing.B, opts db.Options) {
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

func benchCustomUpdateByIndex(b *testing.B) {
	ctx := context.Background()
	d := seedCustomBenchWithOptions(b, benchRows, db.Options{SyncPolicy: db.SyncOff})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := fmt.Sprintf("name-%03d", i%100)
		if _, err := d.Exec(ctx, `UPDATE items SET score = score + 1 WHERE name = ?`, name); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnUpdateByIndex(b *testing.B) {
	d := seedMattnBenchWithPragmas(b, benchRows, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	stmt, err := d.Prepare(`UPDATE items SET score = score + 1 WHERE name = ?`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := fmt.Sprintf("name-%03d", i%100)
		if _, err := stmt.Exec(name); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomDeleteByPK(b *testing.B) {
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, db.Options{SyncPolicy: db.SyncOff})
	mustCustomExec(b, d, `CREATE TABLE delete_pk_items (id INTEGER PRIMARY KEY, name TEXT)`)
	tx, err := d.Begin(ctx, db.TxOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO delete_pk_items VALUES (?, ?)`, int64(i+1), fmt.Sprintf("name-%d", i)); err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(ctx, `DELETE FROM delete_pk_items WHERE id = ?`, int64(i+1)); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnDeleteByPK(b *testing.B) {
	d := openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	mustSQLExec(b, d, `CREATE TABLE delete_pk_items (id INTEGER PRIMARY KEY, name TEXT)`)
	tx, err := d.Begin()
	if err != nil {
		b.Fatal(err)
	}
	insert, err := tx.Prepare(`INSERT INTO delete_pk_items VALUES (?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := insert.Exec(int64(i+1), fmt.Sprintf("name-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
	_ = insert.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	stmt, err := d.Prepare(`DELETE FROM delete_pk_items WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(int64(i + 1)); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCustomDeleteByIndex(b *testing.B) {
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, db.Options{SyncPolicy: db.SyncOff})
	mustCustomExec(b, d, `CREATE TABLE delete_idx_items (id INTEGER PRIMARY KEY, name TEXT)`)
	mustCustomExec(b, d, `CREATE INDEX delete_idx_items_name_idx ON delete_idx_items (name)`)
	tx, err := d.Begin(ctx, db.TxOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO delete_idx_items VALUES (?, ?)`, int64(i+1), fmt.Sprintf("name-%d", i)); err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(ctx, `DELETE FROM delete_idx_items WHERE name = ?`, fmt.Sprintf("name-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMattnDeleteByIndex(b *testing.B) {
	d := openBenchMattnWithPragmas(b, `PRAGMA journal_mode = WAL`, `PRAGMA synchronous = OFF`)
	mustSQLExec(b, d, `CREATE TABLE delete_idx_items (id INTEGER PRIMARY KEY, name TEXT)`)
	mustSQLExec(b, d, `CREATE INDEX delete_idx_items_name_idx ON delete_idx_items (name)`)
	tx, err := d.Begin()
	if err != nil {
		b.Fatal(err)
	}
	insert, err := tx.Prepare(`INSERT INTO delete_idx_items VALUES (?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := insert.Exec(int64(i+1), fmt.Sprintf("name-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
	_ = insert.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	stmt, err := d.Prepare(`DELETE FROM delete_idx_items WHERE name = ?`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("name-%d", i)); err != nil {
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

func seedCustomBench(b *testing.B, n int) *db.DB {
	return seedCustomBenchWithOptions(b, n, db.Options{SyncPolicy: db.SyncFull})
}

func seedCustomBenchWithOptions(b *testing.B, n int, opts db.Options) *db.DB {
	b.Helper()
	ctx := context.Background()
	d := openBenchCustomWithOptions(b, opts)
	mustCustomExec(b, d, `CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT)`)
	mustCustomExec(b, d, `CREATE TABLE items (id INTEGER PRIMARY KEY, group_id INTEGER, name TEXT, score INTEGER, payload JSON)`)
	mustCustomExec(b, d, `CREATE INDEX items_name_idx ON items (name)`)
	tx, err := d.Begin(ctx, db.TxOptions{})
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

func openBenchCustom(b *testing.B) *db.DB {
	return openBenchCustomWithOptions(b, db.Options{SyncPolicy: db.SyncFull})
}

func openBenchCustomWithOptions(b *testing.B, opts db.Options) *db.DB {
	b.Helper()
	d, err := db.Open(filepath.Join(b.TempDir(), "custom"), opts)
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

func mustCustomExec(b *testing.B, d *db.DB, query string, args ...any) {
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
