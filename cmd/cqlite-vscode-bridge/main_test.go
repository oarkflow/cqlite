package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	cqlite "github.com/oarkflow/cqlite"
)

func TestSchemaReturnsTablesAndViews(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	database, err := cqlite.Open(dbPath, cqlite.Options{})
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

func TestSchemaDetailIncludesIndexesForeignKeysAndTriggers(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	database, err := cqlite.Open(dbPath, cqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, stmt := range []string{
		`CREATE TABLE parent_items (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE child_items (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent_items(id), name TEXT)`,
		`CREATE INDEX idx_child_parent ON child_items(parent_id)`,
	} {
		if _, err := database.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	res, err := schema(ctx, database, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var child tableInfo
	for _, obj := range res.Objects {
		if obj.Name == "child_items" {
			child = obj
			break
		}
	}
	if child.Name == "" {
		t.Fatalf("missing child_items in objects: %#v", res.Objects)
	}
	if len(child.Columns) != 3 {
		t.Fatalf("columns = %#v", child.Columns)
	}
	if len(child.Indexes) != 1 || child.Indexes[0].Name != "idx_child_parent" {
		t.Fatalf("indexes = %#v", child.Indexes)
	}
	if len(child.ForeignKeys) != 1 || child.ForeignKeys[0].Table != "parent_items" {
		t.Fatalf("foreign keys = %#v", child.ForeignKeys)
	}
}

func TestRunBatchReturnsSeparateResults(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	database, err := cqlite.Open(dbPath, cqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(ctx, `CREATE TABLE batch_items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	res, err := runBatch(ctx, database, dbPath, []string{
		`INSERT INTO batch_items (name) VALUES ('one')`,
		`SELECT id, name FROM batch_items`,
	}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results = %#v", res.Results)
	}
	if res.Results[0].RowsAffected != 1 {
		t.Fatalf("insert result = %#v", res.Results[0])
	}
	if len(res.Results[1].Rows) != 1 || res.Results[1].Rows[0][1] != "one" {
		t.Fatalf("select result = %#v", res.Results[1])
	}
}

func TestBrowseRowsAppliesLimitOffsetAndSort(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	database, err := cqlite.Open(dbPath, cqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(ctx, `CREATE TABLE browse_items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if _, err := database.Exec(ctx, `INSERT INTO browse_items (name) VALUES (`+sqlString(name)+`)`); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := json.Marshal(statementPayload{Table: "browse_items", Sort: "name DESC", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	res, err := browseRows(ctx, database, dbPath, "", string(payload), 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][1] != "bravo" {
		t.Fatalf("rows = %#v", res.Rows)
	}
}

func TestApplyStatementsCommitsAtomicallyAndRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	database, err := cqlite.Open(dbPath, cqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(ctx, `CREATE TABLE edit_items (id INTEGER PRIMARY KEY, name TEXT UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(statementPayload{Statements: []string{
		`INSERT INTO edit_items (name) VALUES ('one')`,
		`INSERT INTO edit_items (name) VALUES ('two')`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := applyStatements(ctx, database, dbPath, string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsAffected != 2 {
		t.Fatalf("apply result = %#v", res)
	}
	badPayload, err := json.Marshal(statementPayload{Statements: []string{
		`INSERT INTO edit_items (name) VALUES ('three')`,
		`INSERT INTO edit_items (name) VALUES ('one')`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyStatements(ctx, database, dbPath, string(badPayload)); err == nil {
		t.Fatal("expected duplicate insert to fail")
	}
	rows, err := runSQL(ctx, database, dbPath, `SELECT name FROM edit_items ORDER BY name`, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 2 || rows.Rows[0][0] != "one" || rows.Rows[1][0] != "two" {
		t.Fatalf("rollback failed, rows = %#v", rows.Rows)
	}
}
