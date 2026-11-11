package db

import "testing"

func FuzzFTSQueryParser(f *testing.F) {
	for _, query := range []string{
		"",
		"hello",
		"hello AND world",
		"hello OR (world NOT goodbye)",
		`"quoted phrase"*`,
		"title : hello",
		"NEAR(one two three, 5)",
		"^prefix*",
		"(unclosed",
		`"unterminated`,
		"NOT",
		"NEAR(,)",
		"title:body:term",
	} {
		f.Add(query)
	}
	f.Fuzz(func(t *testing.T, query string) {
		_, _ = parseFTSQuery(query, []string{"body", "title"})
	})
}
