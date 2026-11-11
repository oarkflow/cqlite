package sqlparser_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlparser "github.com/sujit/sqlite/internal/sqlparser"
)

func TestParseDocumentLosslessRoundTrip(t *testing.T) {
	sql := " -- leading comment\nSELECT  /* mid */  ID, name\nFROM users\nWHERE id = 1;\n\nCREATE OR REPLACE FUNCTION f()\nRETURNS int\nAS 'select 1';\n"
	doc, err := sqlparser.ParseDocumentString(sql)
	if err != nil {
		t.Fatalf("ParseDocumentString failed: %v", err)
	}
	if doc.SQL() != sql {
		t.Fatalf("document SQL mismatch\nwant: %q\n got: %q", sql, doc.SQL())
	}
	if len(doc.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(doc.Statements))
	}
	if got := doc.Statements[0].SQL(); got != "SELECT  /* mid */  ID, name\nFROM users\nWHERE id = 1;" {
		t.Fatalf("statement span mismatch: %q", got)
	}
	if !strings.Contains(doc.Statements[1].SQL(), "CREATE OR REPLACE FUNCTION") {
		t.Fatalf("expected function statement span, got %q", doc.Statements[1].SQL())
	}
}

func TestParseDocumentExamplesLossless(t *testing.T) {
	paths, err := filepath.Glob("examples/sql/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no example SQL files found")
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc, err := sqlparser.ParseDocument(src)
		if err != nil {
			t.Fatalf("parse document %s: %v", path, err)
		}
		if doc.SQL() != string(src) {
			t.Fatalf("lossless mismatch for %s", path)
		}
		if len(doc.Statements) == 0 {
			t.Fatalf("expected statements for %s", path)
		}
		for _, span := range doc.Statements {
			if span.Start < 0 || span.End < span.Start || int(span.End) > len(src) {
				t.Fatalf("invalid span for %s: %#v", path, span)
			}
			if span.SQL() == "" {
				t.Fatalf("empty statement span for %s", path)
			}
		}
	}
}

func TestParseOptionsGuardrails(t *testing.T) {
	if _, err := sqlparser.ParseStatementsWithOptions("SELECT 1", sqlparser.ParseOptions{MaxBytes: 3, AllowGenericDDL: true}); err == nil {
		t.Fatalf("expected MaxBytes error")
	}
	if _, err := sqlparser.ParseStatementsWithOptions("SELECT 1, 2, 3", sqlparser.ParseOptions{MaxTokens: 3, AllowGenericDDL: true}); err == nil {
		t.Fatalf("expected MaxTokens error")
	}
	if _, err := sqlparser.ParseStatementsWithOptions("SELECT (((((1)))))", sqlparser.ParseOptions{MaxDepth: 3, AllowGenericDDL: true}); err == nil {
		t.Fatalf("expected MaxDepth error")
	}
	if _, err := sqlparser.ParseStatementsWithOptions("SELECT 1; SELECT 2", sqlparser.ParseOptions{MaxStatements: 1, AllowGenericDDL: true}); err == nil {
		t.Fatalf("expected MaxStatements error")
	}
	if _, err := sqlparser.ParseStatementsWithOptions("CREATE TYPE mood", sqlparser.ParseOptions{}); err == nil {
		t.Fatalf("expected generic DDL disabled error")
	}
}

func TestTokenizeAllPreservesTrivia(t *testing.T) {
	toks := sqlparser.TokenizeAll([]byte("SELECT -- hi\n 1"), nil)
	seenComment := false
	seenWhitespace := false
	for _, tok := range toks {
		if tok.Type.String() == "COMMENT" {
			seenComment = true
		}
		if tok.Type.String() == "WHITESPACE" {
			seenWhitespace = true
		}
	}
	if !seenComment || !seenWhitespace {
		t.Fatalf("expected comment and whitespace tokens, got %#v", toks)
	}
}

func TestStrictConvertRejectsFallbackDDL(t *testing.T) {
	_, err := sqlparser.ConvertDialectWithOptions("CREATE FUNCTION f", sqlparser.ConvertOptions{
		Target: sqlparser.DialectPostgres,
		Strict: true,
	})
	if err == nil {
		t.Fatalf("expected strict conversion error")
	}
}

func TestConvertDialectIdentityPreservesOriginal(t *testing.T) {
	sql := "SELECT  /* keep */  id FROM users WHERE id = ?"
	out, err := sqlparser.ConvertDialectWithOptions(sql, sqlparser.ConvertOptions{})
	if err != nil {
		t.Fatalf("identity conversion failed: %v", err)
	}
	if out != sql {
		t.Fatalf("expected identity conversion to preserve SQL\nwant: %q\n got: %q", sql, out)
	}
}

func TestAnalyzeObjectDDLRawBodyFinding(t *testing.T) {
	report := sqlparser.AnalyzeSQL("CREATE OR REPLACE FUNCTION f() RETURNS int AS 'select 1'")
	if !report.Valid {
		t.Fatalf("expected valid SQL: %#v", report.Findings)
	}
	for _, f := range report.Findings {
		if f.Code == "OBJECT_DDL_RAW_BODY" {
			return
		}
	}
	t.Fatalf("expected OBJECT_DDL_RAW_BODY finding, got %#v", report.Findings)
}
