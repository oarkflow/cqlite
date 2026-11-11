package lexer

import "testing"

// FuzzLexer tests the lexer with random input to ensure it never panics
// and always terminates with EOF.
func FuzzLexer(f *testing.F) {
	// Seed corpus with representative SQL fragments
	seeds := []string{
		"SELECT * FROM users",
		"INSERT INTO t (a, b) VALUES (1, 'hello')",
		"UPDATE t SET x = 1 WHERE id = 2",
		"DELETE FROM t WHERE id = 1",
		"CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(255))",
		"'string with ''escape''", // unterminated
		"-- comment\nSELECT 1",
		"/* block */ SELECT 1",
		"0xFF",
		"1.5e-3",
		":param @var $1",
		"->  ->>  #>  #>>  @>  <@  ?|  ?&",
		"SELECT 'unclosed",
		"",
		"\x00\xff\xfe",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		l := New(data)
		for i := 0; i < 100000; i++ {
			tok := l.Next()
			if tok.Type == EOF {
				return
			}
		}
		// If we hit 100k tokens without EOF, something is wrong
		t.Fatal("lexer did not terminate within 100k tokens")
	})
}
