package lexer

import (
	"testing"
)

func TestLexerBasicTokens(t *testing.T) {
	input := "SELECT id, name FROM users WHERE id = 1"
	l := New([]byte(input))
	expected := []TokenType{SELECT, IDENT, COMMA, IDENT, FROM, IDENT, WHERE, IDENT, EQ, INT, EOF}
	for i, exp := range expected {
		tok := l.Next()
		if tok.Type != exp {
			t.Fatalf("token %d: expected %s, got %s (%q)", i, exp, tok.Type, tok.Raw)
		}
	}
}

func TestLexerStrings(t *testing.T) {
	tests := []struct {
		input string
		typ   TokenType
		raw   string
	}{
		{"'hello'", STRING, "'hello'"},
		{"'it''s'", STRING, "'it''s'"},
		{"'escape\\n'", STRING, "'escape\\n'"},
		{`"column"`, DQUOTE, `"column"`},
		{"`table`", BACKTICK, "`table`"},
	}
	for _, tt := range tests {
		l := New([]byte(tt.input))
		tok := l.Next()
		if tok.Type != tt.typ {
			t.Errorf("input %q: expected type %s, got %s", tt.input, tt.typ, tok.Type)
		}
		if string(tok.Raw) != tt.raw {
			t.Errorf("input %q: expected raw %q, got %q", tt.input, tt.raw, tok.Raw)
		}
	}
}

func TestLexerNumbers(t *testing.T) {
	tests := []struct {
		input string
		typ   TokenType
	}{
		{"42", INT},
		{"3.14", FLOAT},
		{".5", FLOAT},
		{"1e10", FLOAT},
		{"1.5e-3", FLOAT},
		{"0xFF", HEXLIT},
		{"x'1A'", HEXLIT},
		{"b'1010'", BITLIT},
	}
	for _, tt := range tests {
		l := New([]byte(tt.input))
		tok := l.Next()
		if tok.Type != tt.typ {
			t.Errorf("input %q: expected type %s, got %s", tt.input, tt.typ, tok.Type)
		}
	}
}

func TestLexerOperators(t *testing.T) {
	tests := []struct {
		input string
		typ   TokenType
	}{
		{"(", LPAREN},
		{")", RPAREN},
		{",", COMMA},
		{";", SEMICOLON},
		{"+", PLUS},
		{"-", MINUS},
		{"*", STAR},
		{"/", SLASH},
		{"=", EQ},
		{"!=", NEQ},
		{"<>", NEQ},
		{"<", LT},
		{">", GT},
		{"<=", LTE},
		{">=", GTE},
		{"||", DBAR},
		{"&&", DAMP},
		{"->", ARROW},
		{"->>", DARROW2},
		{"=>", DARROW},
		{":=", ASSIGN},
		{"#>", HASHARROW},
		{"#>>", HASHDARROW},
		{"@>", ATGT},
		{"<@", LTAT},
		{"?|", QMARKPIPE},
		{"?&", QMARKAMP},
		{"<<", LSHIFT},
		{">>", RSHIFT},
	}
	for _, tt := range tests {
		l := New([]byte(tt.input))
		tok := l.Next()
		if tok.Type != tt.typ {
			t.Errorf("input %q: expected type %s, got %s", tt.input, tt.typ, tok.Type)
		}
	}
}

func TestLexerNamedParams(t *testing.T) {
	tests := []struct {
		input string
		raw   string
	}{
		{":name", ":name"},
		{"@variable", "@variable"},
		{"$1", "$1"},
		{"$name", "$name"},
	}
	for _, tt := range tests {
		l := New([]byte(tt.input))
		tok := l.Next()
		if tok.Type != NAMEDPARAM {
			t.Errorf("input %q: expected NAMEDPARAM, got %s", tt.input, tok.Type)
		}
		if string(tok.Raw) != tt.raw {
			t.Errorf("input %q: expected raw %q, got %q", tt.input, tt.raw, tok.Raw)
		}
	}
}

func TestLexerComments(t *testing.T) {
	// Single-line comment
	l := New([]byte("SELECT -- comment\nid"))
	tok := l.Next()
	if tok.Type != SELECT {
		t.Fatalf("expected SELECT, got %s", tok.Type)
	}
	tok = l.Next()
	if tok.Type != IDENT || string(tok.Raw) != "id" {
		t.Fatalf("expected IDENT 'id', got %s %q", tok.Type, tok.Raw)
	}

	// Block comment
	l = New([]byte("SELECT /* block */ id"))
	tok = l.Next()
	if tok.Type != SELECT {
		t.Fatalf("expected SELECT, got %s", tok.Type)
	}
	tok = l.Next()
	if tok.Type != IDENT || string(tok.Raw) != "id" {
		t.Fatalf("expected IDENT 'id' after block comment, got %s %q", tok.Type, tok.Raw)
	}

	// MySQL hash comment
	l = New([]byte("SELECT # hash comment\nid"))
	tok = l.Next()
	if tok.Type != SELECT {
		t.Fatalf("expected SELECT, got %s", tok.Type)
	}
	tok = l.Next()
	if tok.Type != IDENT || string(tok.Raw) != "id" {
		t.Fatalf("expected IDENT 'id' after hash comment, got %s %q", tok.Type, tok.Raw)
	}
}

func TestLexerKeywords(t *testing.T) {
	keywords := map[string]TokenType{
		"select": SELECT, "SELECT": SELECT, "Select": SELECT,
		"from": FROM, "where": WHERE, "insert": INSERT,
		"update": UPDATE, "delete": DELETE, "create": CREATE,
		"table": TABLE, "index": INDEX, "join": JOIN,
		"left": LEFT, "right": RIGHT, "inner": INNER,
		"outer": OUTER, "on": ON, "and": AND,
		"or": OR, "not": NOT, "null": NULL_KW,
		"true": TRUE_KW, "false": FALSE_KW,
		"in": IN, "between": BETWEEN, "like": LIKE,
		"is": IS, "as": AS, "by": BY,
		"group": GROUP, "having": HAVING, "order": ORDER,
		"limit": LIMIT, "offset": OFFSET, "union": UNION,
		"distinct": DISTINCT, "all": ALL, "exists": EXISTS,
		"case": CASE, "when": WHEN, "then": THEN,
		"else": ELSE, "end": END, "cast": CAST,
	}
	for kw, expected := range keywords {
		l := New([]byte(kw))
		tok := l.Next()
		if tok.Type != expected {
			t.Errorf("keyword %q: expected %s, got %s", kw, expected, tok.Type)
		}
	}
}

func TestLexerWhitespace(t *testing.T) {
	// All types of whitespace
	l := New([]byte("  \t\n\r\n  SELECT"))
	tok := l.Next()
	if tok.Type != SELECT {
		t.Fatalf("expected SELECT after whitespace, got %s", tok.Type)
	}
}

func TestLexerEmptyInput(t *testing.T) {
	l := New([]byte(""))
	tok := l.Next()
	if tok.Type != EOF {
		t.Fatalf("expected EOF, got %s", tok.Type)
	}
}

func TestLexerPos(t *testing.T) {
	l := New([]byte("SELECT id"))
	tok := l.Next()
	if tok.Pos != 0 {
		t.Fatalf("expected pos 0, got %d", tok.Pos)
	}
	tok = l.Next()
	if tok.Pos != 7 {
		t.Fatalf("expected pos 7, got %d", tok.Pos)
	}
}

func TestComputeLineCol(t *testing.T) {
	src := []byte("SELECT\n  id\n  FROM users")
	line, col := ComputeLineCol(src, 9) // "id" starts at byte 9
	if line != 2 || col != 3 {
		t.Fatalf("expected line=2 col=3, got line=%d col=%d", line, col)
	}
}

func TestTokenizeFunc(t *testing.T) {
	src := []byte("SELECT 1")
	buf := make([]Token, 0, 16)
	toks := Tokenize(src, buf)
	if len(toks) != 3 { // SELECT, 1, EOF
		t.Fatalf("expected 3 tokens, got %d", len(toks))
	}
	if toks[0].Type != SELECT {
		t.Fatalf("expected SELECT, got %s", toks[0].Type)
	}
	if toks[1].Type != INT {
		t.Fatalf("expected INT, got %s", toks[1].Type)
	}
	if toks[2].Type != EOF {
		t.Fatalf("expected EOF, got %s", toks[2].Type)
	}
}

func TestLexerUnicodeInString(t *testing.T) {
	l := New([]byte("'héllo wörld'"))
	tok := l.Next()
	if tok.Type != STRING {
		t.Fatalf("expected STRING, got %s", tok.Type)
	}
}

func TestLexerNestedBlockComment(t *testing.T) {
	// Standard SQL doesn't nest block comments, but we should handle them gracefully
	l := New([]byte("SELECT /* outer /* inner */ still comment */ id"))
	tok := l.Next()
	if tok.Type != SELECT {
		t.Fatalf("expected SELECT, got %s", tok.Type)
	}
	// After "/* outer /* inner */", the lexer stops at first "*/"
	// "still" becomes an identifier, "comment" becomes an identifier
	// then "*/" becomes STAR SLASH
}

func TestLexerDotDot(t *testing.T) {
	// Standalone .. operator
	l := New([]byte("a..b"))
	tok := l.Next()
	if tok.Type != IDENT {
		t.Fatalf("expected IDENT, got %s", tok.Type)
	}
	tok = l.Next()
	if tok.Type != DOTDOT {
		t.Fatalf("expected DOTDOT, got %s", tok.Type)
	}
	tok = l.Next()
	if tok.Type != IDENT {
		t.Fatalf("expected IDENT, got %s", tok.Type)
	}
}

// Benchmarks

func BenchmarkLexerNext(b *testing.B) {
	src := []byte("SELECT u.id, u.name, COUNT(o.id) FROM users u LEFT JOIN orders o ON u.id = o.user_id WHERE u.active = 1 GROUP BY u.id ORDER BY u.id LIMIT 10")
	l := New(src)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Reset(src)
		for {
			tok := l.Next()
			if tok.Type == EOF {
				break
			}
		}
	}
}

func BenchmarkKeywordLookup(b *testing.B) {
	words := [][]byte{
		[]byte("select"), []byte("from"), []byte("where"),
		[]byte("insert"), []byte("update"), []byte("delete"),
		[]byte("username"), []byte("id"), []byte("active"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, w := range words {
			lookupKeyword(w)
		}
	}
}
