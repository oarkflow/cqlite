package main

import (
	"context"
	"path/filepath"
	"testing"

	sqlite "github.com/sujit/sqlite"
)

func TestSchemaReturnsTablesAndViews(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	database, err := sqlite.Open(dbPath, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `CREATE TABLE bridge_items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `CREATE VIEW bridge_item_names AS SELECT name FROM bridge_items`); err != nil {
		t.Fatal(err)
	}
	res, err := schema(ctx, database, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tables) != 1 || res.Tables[0].Name != "bridge_items" {
		t.Fatalf("tables = %#v", res.Tables)
	}
	foundView := false
	for _, obj := range res.Objects {
		if obj.Type == "view" && obj.Name == "bridge_item_names" {
			foundView = true
		}
	}
	if !foundView {
		t.Fatalf("objects missing view: %#v", res.Objects)
	}
}
