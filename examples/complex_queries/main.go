package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	sqlite "github.com/sujit/sqlite"
	_ "github.com/sujit/sqlite/driver"
)

type orderRow struct {
	ID        int64   `db:"id"`
	AccountID int64   `db:"account_id"`
	Status    string  `json:"status"`
	Total     float64 `sql:"total"`
	CreatedAt string  `db:"created_at"`
	Metadata  string  `db:"metadata"`
}

func main() {
	ctx := context.Background()
	dir := filepath.Join("examples", "complex_queries", "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	embeddedPath := filepath.Join(dir, "complex_embedded.db")
	driverPath := filepath.Join(dir, "complex_driver.db")
	fmt.Println("embedded database:", embeddedPath)
	fmt.Println("database/sql database:", driverPath)

	if err := embeddedExample(ctx, embeddedPath); err != nil {
		log.Fatal(err)
	}
	if err := databaseSQLExample(ctx, driverPath); err != nil {
		log.Fatal(err)
	}
}

func embeddedExample(ctx context.Context, path string) error {
	db, err := sqlite.Open(path, sqlite.Options{SyncPolicy: sqlite.SyncOff})
	if err != nil {
		return err
	}
	defer db.Close()
	if err := createSchema(ctx, db); err != nil {
		return err
	}
	if err := resetEmbeddedSeed(ctx, db); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `INSERT INTO accounts VALUES (?, ?)`, int64(101), "Acme"); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `INSERT INTO system_settings VALUES (?, ?, ?)`, int64(7), int64(3), int64(12)); err != nil {
		return err
	}

	firstOrder := orderRow{
		ID:        1,
		AccountID: 101,
		Status:    "open",
		CreatedAt: "2026-06-04T09:00:00Z",
		Metadata:  `{"priority":"normal"}`,
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO orders (id, account_id, status, total, created_at, metadata)
		 VALUES :row`,
		sqlite.Named("row", firstOrder),
	); err != nil {
		return err
	}
	mapOrder := map[string]any{
		"id":         int64(2),
		"account_id": int64(101),
		"status":     "review",
		"total":      64.25,
		"created_at": "2026-06-04T09:30:00Z",
		"metadata":   `{"priority":"map"}`,
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO orders (id, account_id, status, total, created_at, metadata)
		 VALUES :row`,
		map[string]any{"row": mapOrder},
	); err != nil {
		return err
	}
	if err := insertRows(ctx, db, []orderRow{
		{ID: 3, AccountID: 101, Status: "open", Total: 125.75, CreatedAt: "2026-06-04T10:00:00Z", Metadata: `{"priority":"high"}`},
		{ID: 4, AccountID: 101, Status: "archived", Total: 9.25, CreatedAt: "2026-05-01T00:00:00Z", Metadata: `{"deleted":true}`},
	}); err != nil {
		return err
	}

	filter := map[string]any{
		"account_id": int64(101),
		"status":     "open",
		"since":      "2026-06-01T00:00:00Z",
	}
	rows, err := db.Query(ctx,
		`SELECT id, account_id, status, total
		 FROM orders
		 WHERE account_id = :account_id
		   AND status = @status
		   AND created_at >= $since
		   AND total >= ?
		 ORDER BY id`,
		filter,
		10.0,
	)
	if err != nil {
		return err
	}
	printRows("embedded mixed-parameter select", rows)

	updateParams := struct {
		NextStatus string  `db:"next_status"`
		Adjustment float64 `db:"adjustment"`
		AccountID  int64   `db:"account_id"`
		ID         int64   `db:"id"`
	}{
		NextStatus: "review",
		Adjustment: 7.25,
		AccountID:  101,
		ID:         3,
	}
	if _, err := db.Exec(ctx,
		`UPDATE orders
		 SET status = :next_status,
		     total = total + :adjustment,
		     metadata = ?
		 WHERE account_id = :account_id
		   AND id = $id`,
		updateParams,
		`{"priority":"review"}`,
	); err != nil {
		return err
	}
	rows, err = db.Query(ctx, `SELECT id, status, total FROM orders WHERE id = $id`, sqlite.Named("id", int64(3)))
	if err != nil {
		return err
	}
	printRows("embedded update follow-up select", rows)

	if err := upsertSettings(ctx, db, int64(101), int64(30), "system"); err != nil {
		return err
	}
	rows, err = db.Query(ctx, `SELECT account_id, retention_days, updated_by FROM account_settings WHERE account_id = ?`, int64(101))
	if err != nil {
		return err
	}
	printRows("portable settings upsert", rows)

	rows, err = db.Query(ctx,
		`SELECT accounts.name, count(*), sum(orders.total)
		 FROM orders
		 JOIN accounts ON orders.account_id = accounts.id
		 WHERE orders.account_id = :account_id
		   AND orders.id IN (SELECT id FROM orders WHERE total >= ?)
		 GROUP BY accounts.name
		 HAVING sum(orders.total) >= ?
		 ORDER BY accounts.name`,
		sqlite.Named("account_id", int64(101)),
		10.0,
		20.0,
	)
	if err != nil {
		return err
	}
	printRows("join aggregate with subquery", rows)

	rows, err = db.Query(ctx,
		`EXPLAIN QUERY PLAN SELECT id FROM orders WHERE account_id = :account_id AND status = @status`,
		sqlite.Named("account_id", int64(101)),
		sqlite.Named("status", "review"),
	)
	if err != nil {
		return err
	}
	printRows("explain query plan", rows)

	return nil
}

func databaseSQLExample(ctx context.Context, path string) error {
	db, err := sql.Open("cqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, stmt := range schemaStatements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := resetDriverSeed(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts VALUES (?, ?)`, int64(201), "Driver Co"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO orders (id, account_id, status, total, created_at, metadata)
		 VALUES (:id, :account_id, @status, ?, :created_at, ?)`,
		sql.Named("id", int64(10)),
		sql.Named("account_id", int64(201)),
		sql.Named("status", "open"),
		88.25,
		sql.Named("created_at", "2026-06-04T11:00:00Z"),
		`{"source":"database/sql"}`,
	); err != nil {
		return err
	}
	var status string
	var total float64
	if err := db.QueryRowContext(ctx,
		`SELECT status, total FROM orders WHERE account_id = :account_id AND status = @status`,
		sql.Named("account_id", int64(201)),
		sql.Named("status", "open"),
	).Scan(&status, &total); err != nil {
		return err
	}
	fmt.Printf("database/sql named query: status=%s total=%.2f\n", status, total)
	return nil
}

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS accounts (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS orders (id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, status TEXT NOT NULL, total REAL NOT NULL, created_at TEXT NOT NULL, metadata TEXT)`,
	`CREATE TABLE IF NOT EXISTS account_settings (account_id INTEGER PRIMARY KEY, retention_days INTEGER NOT NULL, updated_by TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS system_settings (tenant_id INTEGER PRIMARY KEY, days INTEGER NOT NULL, hours INTEGER NOT NULL)`,
	`DROP VIEW IF EXISTS active_orders`,
	`CREATE VIEW active_orders AS SELECT id, account_id, status, total FROM orders WHERE status = 'open'`,
	`DROP INDEX IF EXISTS idx_orders_account_status_param`,
	`CREATE INDEX idx_orders_account_status_param ON orders (account_id, status)`,
}

func createSchema(ctx context.Context, db *sqlite.DB) error {
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func resetEmbeddedSeed(ctx context.Context, db *sqlite.DB) error {
	for _, stmt := range []string{
		`DELETE FROM orders`,
		`DELETE FROM account_settings`,
		`DELETE FROM system_settings`,
		`DELETE FROM accounts`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func resetDriverSeed(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`DELETE FROM orders`,
		`DELETE FROM account_settings`,
		`DELETE FROM system_settings`,
		`DELETE FROM accounts`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func insertRows(ctx context.Context, db *sqlite.DB, rows []orderRow) error {
	_, err := db.Exec(ctx,
		`INSERT INTO orders (id, account_id, status, total, created_at, metadata) VALUES :rows`,
		sqlite.Named("rows", rows),
	)
	return err
}

func upsertSettings(ctx context.Context, db *sqlite.DB, accountID, retentionDays int64, actor string) error {
	res, err := db.Exec(ctx,
		`UPDATE account_settings
		 SET retention_days = :retention_days,
		     updated_by = @actor
		 WHERE account_id = :account_id`,
		sqlite.Named("retention_days", retentionDays),
		sqlite.Named("actor", actor),
		sqlite.Named("account_id", accountID),
	)
	if err != nil {
		return err
	}
	if res.RowsAffected > 0 {
		return nil
	}
	_, err = db.Exec(ctx,
		`INSERT INTO account_settings (account_id, retention_days, updated_by)
		 VALUES (:account_id, :retention_days, @actor)`,
		sqlite.Named("account_id", accountID),
		sqlite.Named("retention_days", retentionDays),
		sqlite.Named("actor", actor),
	)
	return err
}

func printRows(title string, rows *sqlite.Rows) {
	fmt.Println(title)
	fmt.Println("  columns:", strings.Join(rows.Columns, ", "))
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		fmt.Println(" ", row)
	}
}
