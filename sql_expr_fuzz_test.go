package db

import (
	"context"
	"strings"
	"testing"
)

func FuzzSQLExpressionEvaluation(f *testing.F) {
	for _, seed := range []string{
		`1 + 2`,
		`'abc' || 'def'`,
		`CASE WHEN 1 THEN 'yes' ELSE 'no' END`,
		`3 BETWEEN 1 AND 5`,
		`'hello' LIKE 'h%'`,
		`CAST('42' AS INTEGER)`,
		`json_valid('{"ok":true}')`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		if len(expr) > 256 || strings.Contains(expr, ";") || strings.Contains(expr, "\x00") {
			return
		}
		d, err := Open(t.TempDir(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		rows, err := d.Query(context.Background(), "SELECT "+expr)
		if err != nil {
			return
		}
		_, _ = rows.Next()
	})
}
