package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	db "github.com/oarkflow/cqlite"
)

func TestCLIDotCommandsAndMultilineStatements(t *testing.T) {
	database, err := db.Open(t.TempDir(), db.Options{SyncPolicy: db.SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	input := strings.NewReader(strings.Join([]string{
		"CREATE TABLE users (",
		"id INTEGER PRIMARY KEY,",
		"name TEXT",
		");",
		"INSERT INTO users VALUES (1, 'Ada');",
		".tables",
		".schema users",
		".headers off",
		".mode list",
		"SELECT id, name FROM users;",
		".quit",
	}, "\n"))
	var stdout, stderr bytes.Buffer
	if err := runCLI(context.Background(), database, input, &stdout, &stderr, false); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"users\n",
		"CREATE TABLE users",
		"ok rows=1 last_insert_id=1\n",
		"1|Ada\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("CLI output missing %q\nfull output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "id|name") {
		t.Fatalf(".headers off did not suppress headers:\n%s", got)
	}
}

func TestCLIHeadersAndCSVMode(t *testing.T) {
	database, err := db.Open(t.TempDir(), db.Options{SyncPolicy: db.SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(context.Background(), `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(context.Background(), `INSERT INTO users VALUES (?, ?)`, int64(1), "Ada, Lovelace"); err != nil {
		t.Fatal(err)
	}

	input := strings.NewReader(".headers on\n.mode csv\nSELECT id, name FROM users;\n")
	var stdout, stderr bytes.Buffer
	if err := runCLI(context.Background(), database, input, &stdout, &stderr, false); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "id,name\n") || !strings.Contains(got, "1,\"Ada, Lovelace\"\n") {
		t.Fatalf("unexpected csv output:\n%s", got)
	}
}
