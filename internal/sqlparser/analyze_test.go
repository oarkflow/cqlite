package sqlparser_test

import (
	"testing"

	sqlparser "github.com/sujit/sqlite/internal/sqlparser"
)

func TestAnalyzeSQLParseError(t *testing.T) {
	report := sqlparser.AnalyzeSQL("SELECT FROM")
	if report.Valid {
		t.Fatalf("expected invalid SQL")
	}
	if len(report.Findings) == 0 || report.Findings[0].Code != "PARSE_ERROR" {
		t.Fatalf("expected PARSE_ERROR finding, got %#v", report.Findings)
	}
}

func TestAnalyzeSQLRiskyPatterns(t *testing.T) {
	sql := `SELECT * FROM users WHERE name LIKE '%abc'; UPDATE users SET active = 1; DELETE FROM logs;`
	report := sqlparser.AnalyzeSQL(sql)
	if !report.Valid {
		t.Fatalf("expected valid SQL, got parse error: %#v", report.Findings)
	}
	codes := map[string]bool{}
	for _, f := range report.Findings {
		codes[f.Code] = true
	}
	for _, code := range []string{"SELECT_STAR", "LIKE_LEADING_WILDCARD", "UPDATE_WITHOUT_WHERE", "DELETE_WITHOUT_WHERE"} {
		if !codes[code] {
			t.Fatalf("expected finding %s, findings=%#v", code, report.Findings)
		}
	}
}

func TestAnalyzeSQLJSONBHint(t *testing.T) {
	report := sqlparser.AnalyzeSQLWithOptions(`CREATE TABLE events (payload JSONB)`, sqlparser.AnalysisOptions{Dialect: sqlparser.DialectMySQL})
	if !report.Valid {
		t.Fatalf("expected valid SQL")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == "JSONB_DIALECT_NOTE" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected JSONB_DIALECT_NOTE finding")
	}
}

func TestAnalyzeSQLDialectFunctionMismatch(t *testing.T) {
	report := sqlparser.AnalyzeSQLWithOptions(`SELECT IFNULL(name, 'x') FROM users`, sqlparser.AnalysisOptions{Dialect: sqlparser.DialectPostgres})
	if !report.Valid {
		t.Fatalf("expected valid SQL")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == "FUNCTION_DIALECT_REWRITE" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected FUNCTION_DIALECT_REWRITE finding")
	}
}

func TestOptimizeSQLForDialect(t *testing.T) {
	in := `INSERT INTO users (id, name) VALUES (1, IFNULL(:name, 'x')) ON DUPLICATE KEY UPDATE name = IFNULL(:name, name)`
	out, err := sqlparser.OptimizeSQLForDialect(in, sqlparser.DialectPostgres)
	if err != nil {
		t.Fatalf("optimize failed: %v", err)
	}
	if !out.Converted {
		t.Fatalf("expected converted SQL")
	}
	if len(out.Actions) == 0 {
		t.Fatalf("expected optimization actions")
	}
}
