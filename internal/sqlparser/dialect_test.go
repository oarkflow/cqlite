package sqlparser_test

import (
	"strings"
	"testing"

	sqlparser "github.com/oarkflow/cqlite/internal/sqlparser"
)

func TestConvertDialectSelectPostgres(t *testing.T) {
	out, err := sqlparser.ConvertDialect("SELECT id, name FROM users WHERE id = ?", sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, `"users"`) {
		t.Fatalf("expected postgres quoted identifier, got: %s", out)
	}
	if !strings.Contains(out, "$1") {
		t.Fatalf("expected postgres placeholder conversion, got: %s", out)
	}
}

func TestConvertDialectMySQLQuotes(t *testing.T) {
	out, err := sqlparser.ConvertDialect(`SELECT "Name" FROM "Users"`, sqlparser.DialectMySQL)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, "`Name`") || !strings.Contains(out, "`Users`") {
		t.Fatalf("expected mysql backtick quoting, got: %s", out)
	}
}

func TestConvertDialectJSONBToMySQL(t *testing.T) {
	out, err := sqlparser.ConvertDialect(`CREATE TABLE events (payload JSONB)`, sqlparser.DialectMySQL)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(strings.ToUpper(out), "JSON)") {
		t.Fatalf("expected JSONB to map to JSON for mysql, got: %s", out)
	}
}

func TestConvertDialectJSONBToSQLite(t *testing.T) {
	out, err := sqlparser.ConvertDialect(`CREATE TABLE events (payload JSONB)`, sqlparser.DialectSQLite)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(strings.ToUpper(out), "TEXT)") {
		t.Fatalf("expected JSONB to map to TEXT for sqlite, got: %s", out)
	}
}

func TestConvertDialectJSONBOperators(t *testing.T) {
	in := `SELECT payload->>'user', payload#>>'{a,b}' FROM events WHERE payload @> '{"x":1}' AND payload ? 'user'`
	out, err := sqlparser.ConvertDialect(in, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	for _, op := range []string{"->>", "#>>", "@>", "?"} {
		if !strings.Contains(out, op) {
			t.Fatalf("expected operator %s in output: %s", op, out)
		}
	}
}

func TestConvertDialectWithInsert(t *testing.T) {
	in := `WITH src AS (SELECT id FROM users WHERE id = ?) INSERT INTO logs (id) SELECT id FROM src`
	out, err := sqlparser.ConvertDialect(in, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, "WITH") || !strings.Contains(out, "INSERT INTO") {
		t.Fatalf("expected CTE + insert, got: %s", out)
	}
}

func TestConvertFunctionRewrite(t *testing.T) {
	out, err := sqlparser.ConvertDialect(`SELECT IFNULL(name, 'x') FROM users`, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, "COALESCE(") {
		t.Fatalf("expected IFNULL->COALESCE rewrite, got: %s", out)
	}
	out, err = sqlparser.ConvertDialect(`SELECT COALESCE(name, 'x') FROM users`, sqlparser.DialectMySQL)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, "IFNULL(") {
		t.Fatalf("expected COALESCE->IFNULL rewrite, got: %s", out)
	}
}

func TestConvertDialectNamedArguments(t *testing.T) {
	out, err := sqlparser.ConvertDialect(`SELECT make_interval(days => :days, hours := ?)`, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, `"days" => $1`) {
		t.Fatalf("expected named => argument with converted placeholder, got: %s", out)
	}
	if !strings.Contains(out, `"hours" := $2`) {
		t.Fatalf("expected named := argument with converted placeholder, got: %s", out)
	}
}

func TestConvertDialectInsertValuesPayloadParam(t *testing.T) {
	out, err := sqlparser.ConvertDialect(`INSERT INTO orders (account_id, status) VALUES :rows RETURNING id`, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, `VALUES $1`) {
		t.Fatalf("expected values payload placeholder conversion, got: %s", out)
	}
	if !strings.Contains(out, `RETURNING "id"`) {
		t.Fatalf("expected RETURNING clause, got: %s", out)
	}
}

func TestConvertDialectDDLNamedParams(t *testing.T) {
	in := `
		CREATE TABLE param_defaults (
			account_id BIGINT DEFAULT :default_account_id CHECK (account_id <> :blocked_account_id),
			status VARCHAR(32) DEFAULT @default_status
		);
		ALTER TABLE param_defaults ALTER COLUMN status SET DEFAULT :next_status`
	out, err := sqlparser.ConvertDialect(in, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	for _, want := range []string{"DEFAULT $1", `CHECK (("account_id" != $2))`, "DEFAULT $3", "DEFAULT $4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in converted DDL, got: %s", want, out)
		}
	}
}

func TestConvertOnDupKeyToOnConflict(t *testing.T) {
	in := `INSERT INTO users (id, name) VALUES (1, 'a') ON DUPLICATE KEY UPDATE name = 'b'`
	out, err := sqlparser.ConvertDialect(in, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, "ON CONFLICT") || !strings.Contains(out, "DO UPDATE SET") {
		t.Fatalf("expected ON DUPLICATE KEY -> ON CONFLICT rewrite, got: %s", out)
	}
}

func TestConvertOnConflictToOnDupKey(t *testing.T) {
	in := `INSERT INTO users (id, name) VALUES (1, 'a') ON CONFLICT (id) DO UPDATE SET name = 'b'`
	out, err := sqlparser.ConvertDialect(in, sqlparser.DialectMySQL)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("expected ON CONFLICT -> ON DUPLICATE KEY rewrite, got: %s", out)
	}
}

func TestConvertAutoIncrementToIdentity(t *testing.T) {
	in := `CREATE TABLE users (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(32))`
	out, err := sqlparser.ConvertDialect(in, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out, "GENERATED BY DEFAULT AS IDENTITY") {
		t.Fatalf("expected AUTO_INCREMENT->IDENTITY rewrite, got: %s", out)
	}
}
