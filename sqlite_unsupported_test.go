package db

import (
	"context"
	"strings"
	"testing"
)

func TestSQLiteUnsupportedFeatureAllowlist(t *testing.T) {
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	allowlist := []struct {
		name string
		sql  string
	}{
		{name: "views", sql: `CREATE VIEW v AS SELECT 1`},
		{name: "triggers", sql: `CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`},
		{name: "virtual tables", sql: `CREATE VIRTUAL TABLE ft USING fts5(body)`},
		{name: "recursive CTE execution", sql: `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 3) SELECT x FROM c`},
		{name: "window functions", sql: `SELECT row_number() OVER (ORDER BY 1)`},
		{name: "attach database", sql: `ATTACH DATABASE 'other.db' AS other`},
	}
	for _, item := range allowlist {
		t.Run(item.name, func(t *testing.T) {
			if strings.HasPrefix(strings.ToLower(item.sql), "select") || strings.HasPrefix(strings.ToLower(item.sql), "with") {
				_, err = d.Query(ctx, item.sql)
			} else {
				_, err = d.Exec(ctx, item.sql)
			}
			if err == nil {
				t.Fatalf("unsupported feature %q unexpectedly succeeded", item.name)
			}
		})
	}
}
