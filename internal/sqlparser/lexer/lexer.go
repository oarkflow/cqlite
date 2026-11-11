package lexer

import "unsafe"

// Token represents a single SQL token. It holds a slice into the original
// input to avoid copying bytes. All string data is borrowed from the source.
type Token struct {
	// Raw is the exact bytes from the source (not unescaped).
	Raw []byte
	// Type is the token classification.
	Type TokenType
	// Pos is the byte offset of the first character.
	Pos int32
}

// Lexer tokenizes SQL input with zero heap allocations per token.
// It processes bytes directly and uses unsafe string conversion
// to avoid allocations in keyword lookups.
//
// Line/column tracking is intentionally omitted from the hot path.
// Use ComputeLineCol(pos) when needed (e.g. error reporting).
type Lexer struct {
	src []byte
	pos int

	// scratch is reused to build lowercased keyword candidates.
	scratch [64]byte
}

// New creates a Lexer for the given SQL source.
func New(src []byte) *Lexer {
	return &Lexer{src: src}
}

// NewString creates a Lexer for a string input, avoiding a copy via unsafe.
func NewString(src string) *Lexer {
	b := unsafe.Slice(unsafe.StringData(src), len(src))
	return &Lexer{src: b}
}

// Init initialises a Lexer in-place (for embedded use, avoids heap alloc).
func (l *Lexer) Init(src []byte) {
	l.src = src
	l.pos = 0
}

// InitString initialises a Lexer in-place from a string.
func (l *Lexer) InitString(src string) {
	l.src = unsafe.Slice(unsafe.StringData(src), len(src))
	l.pos = 0
}

// Reset reuses the lexer with new source, avoiding allocating a new lexer.
func (l *Lexer) Reset(src []byte) {
	l.src = src
	l.pos = 0
}

// Source returns the underlying source bytes.
func (l *Lexer) Source() []byte { return l.src }

// ComputeLineCol calculates 1-based line and column for a given byte offset.
// This is intentionally off the hot path; call only for error reporting.
func ComputeLineCol(src []byte, pos int) (line, col uint32) {
	line = 1
	col = 1
	if pos > len(src) {
		pos = len(src)
	}
	for i := 0; i < pos; i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return
}

// Byte dispatch categories for the lexer's hot loop.
// Using a table avoids branch-heavy switch and improves branch prediction.
const (
	cOther byte = iota // default: punctuation, ILLEGAL
	cSpace             // ' ', '\t', '\v', '\f'
	cNewL              // '\n'
	cCR                // '\r'
	cAlpha             // a-z, A-Z, _
	cDigit             // 0-9
	cDot               // '.'
	cSQ                // '\''
	cDQ                // '"'
	cBT                // '`'
	cDash              // '-'
	cSlash             // '/'
	cHash              // '#'
)

var charClass [256]byte

func init() {
	charClass[' '] = cSpace
	charClass['\t'] = cSpace
	charClass['\v'] = cSpace
	charClass['\f'] = cSpace
	charClass['\n'] = cNewL
	charClass['\r'] = cCR
	for c := byte('a'); c <= 'z'; c++ {
		charClass[c] = cAlpha
	}
	for c := byte('A'); c <= 'Z'; c++ {
		charClass[c] = cAlpha
	}
	charClass['_'] = cAlpha
	for c := byte('0'); c <= '9'; c++ {
		charClass[c] = cDigit
	}
	charClass['.'] = cDot
	charClass['\''] = cSQ
	charClass['"'] = cDQ
	charClass['`'] = cBT
	charClass['-'] = cDash
	charClass['/'] = cSlash
	charClass['#'] = cHash
}

// Next returns the next token from the input. Returns EOF when exhausted.
// This function never allocates on the heap; all returned Token.Raw slices
// are sub-slices of the original source.
func (l *Lexer) Next() Token {
	src := l.src
	pos := l.pos
	n := len(src)

	for pos < n {
		start := pos
		b := src[pos]

		switch charClass[b] {
		case cNewL:
			pos++
			continue

		case cCR:
			pos++
			if pos < n && src[pos] == '\n' {
				pos++
			}
			continue

		case cSpace:
			pos++
			for pos < n && isSpaceTab[src[pos]] {
				pos++
			}
			continue

		case cDash:
			if pos+1 < n && src[pos+1] == '-' {
				// Single-line comment --
				pos += 2
				for pos < n && src[pos] != '\n' {
					pos++
				}
				continue
			}
			// Might be -> or ->> or just -
			l.pos = pos
			return l.lexPunct(start)

		case cHash:
			if pos+1 < n && src[pos+1] == '>' {
				l.pos = pos
				return l.lexPunct(start)
			}
			// MySQL hash comment
			pos++
			for pos < n && src[pos] != '\n' {
				pos++
			}
			continue

		case cSlash:
			if pos+1 < n && src[pos+1] == '*' {
				// Block comment /* ... */
				pos += 2
				for pos+1 < n {
					if src[pos] == '*' && src[pos+1] == '/' {
						pos += 2
						break
					}
					pos++
				}
				continue
			}
			l.pos = pos
			return l.lexPunct(start)

		case cDigit:
			// Check for 0x hex literal
			if b == '0' && pos+1 < n && (src[pos+1] == 'x' || src[pos+1] == 'X') {
				l.pos = pos
				return l.lexHex0x(start)
			}
			l.pos = pos
			return l.lexNumber(start)

		case cDot:
			if pos+1 < n && src[pos+1] >= '0' && src[pos+1] <= '9' {
				l.pos = pos
				return l.lexNumber(start)
			}
			l.pos = pos
			return l.lexPunct(start)

		case cSQ:
			l.pos = pos
			return l.lexQuoted(start, '\'', STRING)

		case cDQ:
			l.pos = pos
			return l.lexQuoted(start, '"', DQUOTE)

		case cBT:
			l.pos = pos
			return l.lexQuoted(start, '`', BACKTICK)

		case cAlpha:
			// Check for hex/bit string literals: x'...' X'...' b'...' B'...'
			if pos+1 < n && src[pos+1] == '\'' {
				if b == 'x' || b == 'X' {
					l.pos = pos
					return l.lexHexLit(start)
				}
				if b == 'b' || b == 'B' {
					l.pos = pos
					return l.lexBitLit(start)
				}
			}
			l.pos = pos
			return l.lexIdent(start)

		default:
			l.pos = pos
			return l.lexPunct(start)
		}
	}
	l.pos = pos
	return Token{Type: EOF, Pos: int32(pos)}
}

// NextAll returns the next token including whitespace and comments. It is used
// by lossless/document parsing; Next remains the hot trivia-skipping path.
func (l *Lexer) NextAll() Token {
	src := l.src
	pos := l.pos
	n := len(src)
	if pos >= n {
		l.pos = pos
		return Token{Type: EOF, Pos: int32(pos)}
	}
	start := pos
	b := src[pos]
	switch charClass[b] {
	case cNewL:
		pos++
		l.pos = pos
		return Token{Type: WHITESPACE, Raw: src[start:pos], Pos: int32(start)}
	case cCR:
		pos++
		if pos < n && src[pos] == '\n' {
			pos++
		}
		l.pos = pos
		return Token{Type: WHITESPACE, Raw: src[start:pos], Pos: int32(start)}
	case cSpace:
		pos++
		for pos < n && isSpaceTab[src[pos]] {
			pos++
		}
		l.pos = pos
		return Token{Type: WHITESPACE, Raw: src[start:pos], Pos: int32(start)}
	case cDash:
		if pos+1 < n && src[pos+1] == '-' {
			pos += 2
			for pos < n && src[pos] != '\n' {
				pos++
			}
			l.pos = pos
			return Token{Type: COMMENT, Raw: src[start:pos], Pos: int32(start)}
		}
	case cHash:
		if pos+1 >= n || src[pos+1] != '>' {
			pos++
			for pos < n && src[pos] != '\n' {
				pos++
			}
			l.pos = pos
			return Token{Type: COMMENT, Raw: src[start:pos], Pos: int32(start)}
		}
	case cSlash:
		if pos+1 < n && src[pos+1] == '*' {
			pos += 2
			for pos+1 < n {
				if src[pos] == '*' && src[pos+1] == '/' {
					pos += 2
					break
				}
				pos++
			}
			if pos >= n {
				pos = n
			}
			l.pos = pos
			return Token{Type: COMMENT, Raw: src[start:pos], Pos: int32(start)}
		}
	}
	return l.Next()
}

// lexIdent scans an identifier or keyword.
func (l *Lexer) lexIdent(start int) Token {
	src := l.src
	pos := l.pos + 1
	n := len(src)
	for pos < n && identContTable[src[pos]] {
		pos++
	}
	l.pos = pos
	raw := src[start:pos]

	// Lowercase into scratch for keyword lookup (no heap alloc for <=64 bytes).
	kwLen := len(raw)
	if kwLen > 14 {
		// All SQL keywords are <= 14 chars
		return Token{Type: IDENT, Raw: raw, Pos: int32(start)}
	}
	scratch := &l.scratch
	for i := 0; i < kwLen; i++ {
		c := raw[i]
		if c >= 'A' && c <= 'Z' {
			scratch[i] = c + 32
		} else {
			scratch[i] = c
		}
	}
	tok := lookupKeyword(scratch[:kwLen])
	return Token{Type: tok, Raw: raw, Pos: int32(start)}
}

// lexNumber scans integer or float literals.
func (l *Lexer) lexNumber(start int) Token {
	src := l.src
	pos := l.pos
	n := len(src)
	typ := INT
	for pos < n && src[pos] >= '0' && src[pos] <= '9' {
		pos++
	}
	if pos < n && src[pos] == '.' {
		typ = FLOAT
		pos++
		for pos < n && src[pos] >= '0' && src[pos] <= '9' {
			pos++
		}
	}
	// optional exponent
	if pos < n && (src[pos] == 'e' || src[pos] == 'E') {
		typ = FLOAT
		pos++
		if pos < n && (src[pos] == '+' || src[pos] == '-') {
			pos++
		}
		for pos < n && src[pos] >= '0' && src[pos] <= '9' {
			pos++
		}
	}
	l.pos = pos
	return Token{Type: TokenType(typ), Raw: src[start:pos], Pos: int32(start)}
}

// lexQuoted scans a single, double, or backtick quoted string.
func (l *Lexer) lexQuoted(start int, delim byte, typ TokenType) Token {
	src := l.src
	pos := l.pos + 1 // skip opening delimiter
	n := len(src)
	for pos < n {
		c := src[pos]
		if c == delim {
			pos++
			// doubled delimiter is an escape (e.g. '' or "")
			if pos < n && src[pos] == delim {
				pos++
				continue
			}
			break
		}
		if c == '\\' && delim != '`' {
			pos++
			if pos < n {
				pos++
			}
			continue
		}
		pos++
	}
	l.pos = pos
	return Token{Type: typ, Raw: src[start:pos], Pos: int32(start)}
}

func (l *Lexer) lexHexLit(start int) Token {
	src := l.src
	pos := l.pos + 2 // skip x'
	n := len(src)
	for pos < n && src[pos] != '\'' {
		pos++
	}
	if pos < n {
		pos++ // closing '
	}
	l.pos = pos
	return Token{Type: HEXLIT, Raw: src[start:pos], Pos: int32(start)}
}

func (l *Lexer) lexHex0x(start int) Token {
	src := l.src
	pos := l.pos + 2 // 0x
	n := len(src)
	for pos < n && isHexDigit(src[pos]) {
		pos++
	}
	l.pos = pos
	return Token{Type: HEXLIT, Raw: src[start:pos], Pos: int32(start)}
}

func (l *Lexer) lexBitLit(start int) Token {
	l.pos++ // skip b/B
	return l.lexQuoted(l.pos-1, '\'', BITLIT)
}

// lexPunct handles single and multi-character punctuation/operators.
func (l *Lexer) lexPunct(start int) Token {
	src := l.src
	b := src[l.pos]
	l.pos++

	peek := func() byte {
		if l.pos < len(src) {
			return src[l.pos]
		}
		return 0
	}
	advance := func() {
		l.pos++
	}

	var typ TokenType
	switch b {
	case '(':
		typ = LPAREN
	case ')':
		typ = RPAREN
	case '{':
		typ = LBRACE
	case '}':
		typ = RBRACE
	case '[':
		typ = LBRACKET
	case ']':
		typ = RBRACKET
	case ',':
		typ = COMMA
	case ';':
		typ = SEMICOLON
	case '*':
		typ = STAR
	case '%':
		typ = PERCENT
	case '^':
		typ = CARET
	case '~':
		typ = TILDE
	case '?':
		switch peek() {
		case '|':
			advance()
			typ = QMARKPIPE
		case '&':
			advance()
			typ = QMARKAMP
		default:
			typ = QUESTION
		}
	case '+':
		typ = PLUS
	case '=':
		if peek() == '>' {
			advance()
			typ = DARROW
		} else {
			typ = EQ
		}
	case '!':
		if peek() == '=' {
			advance()
			typ = NEQ
		} else {
			typ = BANG
		}
	case '<':
		switch peek() {
		case '=':
			advance()
			typ = LTE
		case '>':
			advance()
			typ = NEQ
		case '@':
			advance()
			typ = LTAT
		case '<':
			advance()
			typ = LSHIFT
		default:
			typ = LT
		}
	case '>':
		switch peek() {
		case '=':
			advance()
			typ = GTE
		case '>':
			advance()
			typ = RSHIFT
		default:
			typ = GT
		}
	case '|':
		if peek() == '|' {
			advance()
			typ = DBAR
		} else {
			typ = PIPE
		}
	case '&':
		if peek() == '&' {
			advance()
			typ = DAMP
		} else {
			typ = AMPERSAND
		}
	case '-':
		if peek() == '>' {
			advance()
			if peek() == '>' {
				advance()
				typ = DARROW2
			} else {
				typ = ARROW
			}
		} else {
			typ = MINUS
		}
	case '/':
		typ = SLASH
	case '.':
		if peek() == '.' {
			advance()
			typ = DOTDOT
		} else {
			typ = DOT
		}
	case ':':
		// named parameter :name
		if p := peek(); p == '=' {
			advance()
			typ = ASSIGN
		} else if isAlphaB(p) || p == '_' {
			for l.pos < len(src) && identContTable[src[l.pos]] {
				advance()
			}
			typ = NAMEDPARAM
		} else {
			typ = COLON
		}
	case '@':
		p := peek()
		if p == '>' {
			advance()
			typ = ATGT
		} else if isAlphaB(p) || p == '_' || p == '@' {
			for l.pos < len(src) && identContTable[src[l.pos]] {
				advance()
			}
			typ = NAMEDPARAM
		} else {
			typ = AT
		}
	case '$':
		p := peek()
		if p >= '0' && p <= '9' {
			for l.pos < len(src) && src[l.pos] >= '0' && src[l.pos] <= '9' {
				advance()
			}
			typ = NAMEDPARAM
		} else if isAlphaB(p) || p == '_' {
			for l.pos < len(src) && identContTable[src[l.pos]] {
				advance()
			}
			typ = NAMEDPARAM
		} else {
			typ = DOLLAR
		}
	case '#':
		if peek() == '>' {
			advance()
			if peek() == '>' {
				advance()
				typ = HASHDARROW
			} else {
				typ = HASHARROW
			}
		} else {
			typ = HASH
		}
	default:
		typ = ILLEGAL
	}
	return Token{Type: typ, Raw: src[start:l.pos], Pos: int32(start)}
}

// ---- character classification tables (no function call overhead) ----

var isSpaceTab = [256]bool{' ': true, '\t': true, '\v': true, '\f': true}

var identContTable [256]bool

func init() {
	for c := 'a'; c <= 'z'; c++ {
		identContTable[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		identContTable[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		identContTable[c] = true
	}
	identContTable['_'] = true
	identContTable['$'] = true
}

func isAlphaB(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' }
func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Tokenize breaks SQL source into tokens. Provide a pre-allocated buf to avoid allocation.
func Tokenize(src []byte, buf []Token) []Token {
	buf = buf[:0]
	l := Lexer{src: src}
	for {
		t := l.Next()
		buf = append(buf, t)
		if t.Type == EOF {
			break
		}
	}
	return buf
}

// TokenizeAll breaks SQL source into tokens including whitespace and comments.
// Provide a pre-allocated buf to control allocations.
func TokenizeAll(src []byte, buf []Token) []Token {
	buf = buf[:0]
	l := Lexer{src: src}
	for {
		t := l.NextAll()
		buf = append(buf, t)
		if t.Type == EOF {
			break
		}
	}
	return buf
}
