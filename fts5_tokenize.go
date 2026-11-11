package db

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token is one indexed term occurrence. Start and End are byte offsets into
// the original column text so highlight() and snippet() can locate it.
type Token struct {
	Term  string
	Start int
	End   int
}

// Tokenizer turns column text into indexable terms.
type Tokenizer interface {
	Name() string
	Tokenize(text string) []Token
	// QueryToken normalizes a term written in an FTS5 query the same way the
	// tokenizer normalizes indexed text, so they compare equal.
	QueryToken(term string) string
}

// tokenizerName resolves a tokenizer spec such as "unicode61",
// "porter unicode61" or "trigram" to a Tokenizer.
func tokenizerName(spec string) (Tokenizer, error) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "" {
		return unicode61Tokenizer{}, nil
	}
	// A leading "porter" wraps whatever follows (default unicode61).
	if spec == "porter" || strings.HasPrefix(spec, "porter ") {
		inner := strings.TrimSpace(strings.TrimPrefix(spec, "porter"))
		base := unicode61Tokenizer{}
		if inner != "" && inner != "unicode61" {
			b, err := tokenizerName(inner)
			if err != nil {
				return nil, err
			}
			base.name = inner
			base.delegate = b
		}
		return porterTokenizer{inner: unicode61Tokenizer{}}, nil
	}
	switch spec {
	case "unicode61", "unicode61 remove_diacritics 1", "unicode61 remove_diacritics 2":
		return unicode61Tokenizer{removeDiacritics: true}, nil
	case "unicode61 remove_diacritics 0":
		return unicode61Tokenizer{}, nil
	case "ascii":
		return asciiTokenizer{}, nil
	case "simple":
		return asciiTokenizer{}, nil
	case "trigram":
		return trigramTokenizer{}, nil
	}
	// "unicode61 ..." with extra options: accept and use the defaults.
	if strings.HasPrefix(spec, "unicode61") {
		return unicode61Tokenizer{removeDiacritics: !strings.Contains(spec, "remove_diacritics 0")}, nil
	}
	return nil, errUnsupportedTokenizer(spec)
}

type tokenizerError struct{ spec string }

func (e *tokenizerError) Error() string { return "unknown tokenizer: " + e.spec }

func errUnsupportedTokenizer(spec string) error { return &tokenizerError{spec: spec} }

// unicode61Tokenizer splits on non-alphanumeric characters using Unicode
// classes and lowercases the result.
type unicode61Tokenizer struct {
	removeDiacritics bool
	name             string
	delegate         Tokenizer
}

func (unicode61Tokenizer) Name() string { return "unicode61" }

func (u unicode61Tokenizer) Tokenize(text string) []Token {
	return splitTokens(text, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
	}, u.removeDiacritics)
}

func (u unicode61Tokenizer) QueryToken(term string) string {
	return normalizeTerm(term, u.removeDiacritics)
}

// asciiTokenizer keeps only ASCII letters and digits, like SQLite's "simple"
// and "ascii" tokenizers.
type asciiTokenizer struct{}

func (asciiTokenizer) Name() string      { return "ascii" }
func (asciiTokenizer) QueryToken(t string) string {
	return strings.ToLower(t)
}

func (asciiTokenizer) Tokenize(text string) []Token {
	return splitTokens(text, func(r rune) bool {
		return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}, false)
}

// trigramTokenizer emits every three-character run of the text, which makes
// substring LIKE/GLOB-style matching possible through FTS5.
type trigramTokenizer struct{}

func (trigramTokenizer) Name() string                 { return "trigram" }
func (trigramTokenizer) QueryToken(t string) string    { return strings.ToLower(t) }

func (trigramTokenizer) Tokenize(text string) []Token {
	lower := strings.ToLower(text)
	var out []Token
	// Trigrams are indexed over bytes in SQLite; use runes for correctness on
	// non-ASCII input and record byte offsets for highlighting.
	b := []byte(lower)
	for i := 0; i+2 < len(lower); {
		_, size := utf8.DecodeRuneInString(lower[i:])
		_, size2 := utf8.DecodeRuneInString(lower[i+size:])
		_, size3 := utf8.DecodeRuneInString(lower[i+size+size2:])
		end := i + size + size2 + size3
		if end <= len(b) {
			out = append(out, Token{Term: lower[i:end], Start: i, End: end})
		}
		i += size
	}
	return out
}

// splitTokens runs a word-break predicate over the text.
func splitTokens(text string, keep func(rune) bool, foldDiacritics bool) []Token {
	var out []Token
	runes := []rune(text)
	byteOf := make([]int, len(runes)+1)
	off := 0
	for i, r := range runes {
		byteOf[i] = off
		off += utf8.RuneLen(r)
	}
	byteOf[len(runes)] = off

	start := -1
	for i, r := range runes {
		if keep(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, Token{
				Term:  normalizeTerm(string(runes[start:i]), foldDiacritics),
				Start: byteOf[start],
				End:   byteOf[i],
			})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, Token{
			Term:  normalizeTerm(string(runes[start:]), foldDiacritics),
			Start: byteOf[start],
			End:   byteOf[len(runes)],
		})
	}
	return out
}

func normalizeTerm(s string, foldDiacritics bool) string {
	if foldDiacritics {
		s = stripDiacritics(s)
	}
	return strings.ToLower(s)
}

// stripDiacritics removes combining marks and maps a few common Latin-1
// letters, matching unicode61's remove_diacritics behaviour closely enough
// for indexing and lookup to agree.
func stripDiacritics(s string) string {
	if !strings.ContainsFunc(s, unicode.IsMark) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsMark(r) {
			continue
		}
		b.WriteRune(foldLatinRune(r))
	}
	return b.String()
}

func foldLatinRune(r rune) rune {
	switch r {
	case 'ø', 'Ø':
		return 'o'
	case 'æ', 'Æ':
		return 'a'
	case 'ß':
		return 's'
	case 'ð', 'Ð':
		return 'd'
	case 'þ', 'Þ':
		return 't'
	case 'ł', 'Ł':
		return 'l'
	}
	return r
}

// porterTokenizer applies the Porter stemmer over unicode61 tokens.
type porterTokenizer struct {
	inner Tokenizer
}

func (porterTokenizer) Name() string { return "porter" }

func (p porterTokenizer) Tokenize(text string) []Token {
	toks := p.inner.Tokenize(text)
	for i := range toks {
		toks[i].Term = porterStem(toks[i].Term)
	}
	return toks
}

func (p porterTokenizer) QueryToken(term string) string {
	return porterStem(p.inner.QueryToken(term))
}

// porterStem implements the Porter stemming algorithm (1980).
func porterStem(s string) string {
	if len(s) < 3 {
		return s
	}
	b := []byte(strings.ToLower(s))
	b = step1a(b)
	b = step1b(b)
	b = step1c(b)
	b = step2(b)
	b = step3(b)
	b = step4(b)
	b = step5(b)
	return string(b)
}

func cons(b []byte, i int) bool {
	switch b[i] {
	case 'a', 'e', 'i', 'o', 'u':
		return false
	case 'y':
		return i == 0 || !cons(b, i-1)
	}
	return true
}

// m measures the number of VC sequences in the stem.
func m(b []byte) int {
	n := 0
	i := 0
	for i < len(b) {
		if !cons(b, i) {
			break
		}
		i++
	}
	i++
	for {
		for i < len(b) {
			if cons(b, i) {
				break
			}
			i++
		}
		if i >= len(b) {
			return n
		}
		i++
		n++
		for i < len(b) {
			if !cons(b, i) {
				break
			}
			i++
		}
		if i >= len(b) {
			return n
		}
		i++
	}
}

func vowelInStem(b []byte) bool {
	for i := range b {
		if !cons(b, i) {
			return true
		}
	}
	return false
}

func doubleC(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	return b[len(b)-1] == b[len(b)-2] && cons(b, len(b)-1)
}

func cvc(b []byte) bool {
	if len(b) < 3 {
		return false
	}
	i := len(b) - 1
	if !cons(b, i) || cons(b, i-1) || !cons(b, i-2) {
		return false
	}
	switch b[i] {
	case 'w', 'x', 'y':
		return false
	}
	return true
}

func endsWith(b []byte, s string) bool {
	return len(b) >= len(s) && string(b[len(b)-len(s):]) == s
}

func stem(b []byte, s string) []byte {
	if !endsWith(b, s) {
		return b
	}
	return b[:len(b)-len(s)]
}

func setTo(b []byte, s string) []byte {
	b = stem(b, s)
	return append(b, s...)
}

func step1a(b []byte) []byte {
	switch {
	case endsWith(b, "sses"):
		return b[:len(b)-2]
	case endsWith(b, "ies"):
		return b[:len(b)-2]
	case endsWith(b, "ss"):
		return b
	case endsWith(b, "s"):
		return b[:len(b)-1]
	}
	return b
}

func step1b(b []byte) []byte {
	if endsWith(b, "eed") {
		if m(b[:len(b)-3]) > 0 {
			return b[:len(b)-1]
		}
		return b
	}
	cut := -1
	switch {
	case endsWith(b, "ed"):
		cut = len(b) - 2
	case endsWith(b, "ing"):
		cut = len(b) - 3
	}
	if cut < 0 {
		return b
	}
	st := b[:cut]
	if !vowelInStem(st) {
		return b
	}
	b = st
	switch {
	case endsWith(b, "at"), endsWith(b, "bl"), endsWith(b, "iz"):
		return append(b, 'e')
	case doubleC(b) && !endsWith(b, "l") && !endsWith(b, "s") && !endsWith(b, "z"):
		return b[:len(b)-1]
	case m(b) == 1 && cvc(b):
		return append(b, 'e')
	}
	return b
}

func step1c(b []byte) []byte {
	if endsWith(b, "y") && vowelInStem(b[:len(b)-1]) {
		return append(b[:len(b)-1], 'i')
	}
	return b
}

var step2Rules = [][2]string{
	{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"}, {"anci", "ance"},
	{"izer", "ize"}, {"abli", "able"}, {"alli", "al"}, {"entli", "ent"},
	{"eli", "e"}, {"ousli", "ous"}, {"ization", "ize"}, {"ation", "ate"},
	{"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"}, {"fulness", "ful"},
	{"ousness", "ous"}, {"aliti", "al"}, {"iviti", "ive"}, {"biliti", "ble"},
}

var step3Rules = [][2]string{
	{"icate", "ic"}, {"ative", ""}, {"alize", "al"}, {"iciti", "ic"},
	{"ical", "ic"}, {"ful", ""}, {"ness", ""},
}

var step4Rules = []string{
	"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement",
	"ment", "ent", "ion", "ou", "ism", "ate", "iti", "ous", "ive", "ize",
}

func step2(b []byte) []byte {
	for _, r := range step2Rules {
		if endsWith(b, r[0]) && m(b[:len(b)-len(r[0])]) > 0 {
			return setTo(b[:len(b)-len(r[0])], r[1])
		}
	}
	return b
}

func step3(b []byte) []byte {
	for _, r := range step3Rules {
		if endsWith(b, r[0]) && m(b[:len(b)-len(r[0])]) > 0 {
			return setTo(b[:len(b)-len(r[0])], r[1])
		}
	}
	return b
}

func step4(b []byte) []byte {
	for _, suf := range step4Rules {
		if !endsWith(b, suf) {
			continue
		}
		st := b[:len(b)-len(suf)]
		if suf == "ion" {
			if m(st) > 1 && len(st) > 0 && (st[len(st)-1] == 's' || st[len(st)-1] == 't') {
				return st
			}
			continue
		}
		if m(st) > 1 {
			return st
		}
	}
	return b
}

func step5(b []byte) []byte {
	if endsWith(b, "e") {
		st := b[:len(b)-1]
		if m(st) > 1 || (m(st) == 1 && !cvc(st)) {
			b = st
		}
	}
	if endsWith(b, "ll") && m(b) > 1 {
		b = b[:len(b)-1]
	}
	return b
}
