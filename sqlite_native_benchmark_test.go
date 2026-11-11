//go:build sqlite_native_benchmark && cgo

package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	custom "github.com/sujit/sqlite/driver"
	_ "github.com/sujit/sqlite/internal/native/sqlite3"
)

func BenchmarkSQLiteNativeComparison(b *testing.B) {
	for _, engine := range []struct{ name, driver string }{
		{"custom", custom.Name},
		{"native", "sqlite3-native-benchmark"},
	} {
		b.Run(engine.name+"/point_lookup", func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "bench.db")
			db, err := sql.Open(engine.driver, path)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			ctx := context.Background()
			if _, err := db.ExecContext(ctx, "CREATE TABLE items (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
				b.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "CREATE INDEX items_value ON items(value)"); err != nil {
				b.Fatal(err)
			}
			for i := 0; i < 1000; i++ {
				if _, err := db.ExecContext(ctx, "INSERT INTO items(id, value) VALUES (?, ?)", i, fmt.Sprintf("value-%04d", i)); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var value string
				if err := db.QueryRowContext(ctx, "SELECT value FROM items WHERE id = ?", i%1000).Scan(&value); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
