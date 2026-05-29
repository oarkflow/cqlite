package driver

import (
	"database/sql"
	"testing"
)

func TestDatabaseSQLDriver(t *testing.T) {
	database, err := sql.Open(Name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE users (id INT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO users VALUES (?, ?)`, int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := database.QueryRow(`SELECT name FROM users WHERE id = ?`, int64(1)).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Ada" {
		t.Fatalf("name = %q", name)
	}
}
