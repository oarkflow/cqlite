package db

import (
	"testing"

	"github.com/oarkflow/sqlite/internal/sqlparser"
)

func TestParserAcceptsV1InsertShapes(t *testing.T) {
	cases := []string{
		`INSERT INTO tasks (id, title, is_done) VALUES (1, 'ship', ?)`,
		`INSERT INTO users VALUES (?, ?)`,
	}
	for _, tc := range cases {
		if _, err := sqlparser.ParseStatement(tc); err != nil {
			t.Fatalf("%s: %v", tc, err)
		}
	}
}
