package db

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// jsonObject preserves SQL-visible key order. Go maps randomize iteration
// order, but SQLite's json() and json_object() emit keys in insertion order,
// so the object representation has to be ordered.
type jsonObject struct {
	keys []string
	m    map[string]any
}

func newJSONObject() *jsonObject {
	return &jsonObject{m: map[string]any{}}
}

// set writes a key, appending it to the order only on first insert. Replacing
// an existing key keeps its original position, matching SQLite.
func (o *jsonObject) set(key string, v any) {
	if _, exists := o.m[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.m[key] = v
}

func (o *jsonObject) get(key string) (any, bool) {
	v, ok := o.m[key]
	return v, ok
}

func (o *jsonObject) delete(key string) {
	if _, ok := o.m[key]; !ok {
		return
	}
	delete(o.m, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// parseJSON5 decodes JSON plus the JSON5 extensions SQLite accepts: unquoted
// keys, single-quoted strings, trailing commas, comments, hexadecimal numbers,
// leading or trailing decimal points and Infinity/NaN. It returns the byte
// offset of the first syntax error so json_error_position can report it.
func parseJSON5(s string) (any, int, error) {
	return parseJSONWith(s, false)
}

// parseStrictJSON rejects the JSON5 extensions. json_valid() is documented to
// accept only well-formed RFC 8259 JSON even though json() normalizes JSON5.
func parseStrictJSON(s string) (any, int, error) {
	return parseJSONWith(s, true)
}

func parseJSONWith(s string, strict bool) (any, int, error) {
	p := &jsonParser{src: s, strict: strict}
	p.skipWSAndComments()
	v, err := p.parseValue()
	if err != nil {
		return nil, p.errAt, err
	}
	p.skipWSAndComments()
	if p.pos != len(p.src) {
		return nil, p.pos, errors.New("trailing data")
	}
	return v, 0, nil
}

type jsonParser struct {
	src    string
	pos    int
	errAt  int
	strict bool // reject JSON5 extensions (json_valid)
}

func (p *jsonParser) fail(at int, msg string) error {
	p.errAt = at
	return errors.New(msg)
}

func (p *jsonParser) eof() bool {
	return p.pos >= len(p.src)
}

func (p *jsonParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *jsonParser) skipWSAndComments() {
	for !p.eof() {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
			continue
		}
		if c == '/' && p.pos+1 < len(p.src) {
			if p.strict {
				return
			}
			switch p.src[p.pos+1] {
			case '/':
				for !p.eof() && p.src[p.pos] != '\n' {
					p.pos++
				}
				continue
			case '*':
				p.pos += 2
				for p.pos+1 < len(p.src) && !(p.src[p.pos] == '*' && p.src[p.pos+1] == '/') {
					p.pos++
				}
				if p.pos+1 < len(p.src) {
					p.pos += 2
				} else {
					p.pos = len(p.src)
				}
				continue
			}
		}
		return
	}
}

func (p *jsonParser) parseValue() (any, error) {
	p.skipWSAndComments()
	if p.eof() {
		return nil, p.fail(p.pos, "unexpected end of JSON")
	}
	switch c := p.peek(); {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"' || c == '\'':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return s, nil
	case c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	default:
		if p.hasWord("true") {
			p.pos += 4
			return true, nil
		}
		if p.hasWord("false") {
			p.pos += 5
			return false, nil
		}
		if p.hasWord("null") {
			p.pos += 4
			return nil, nil
		}
		if !p.strict && p.hasWord("Infinity") {
			p.pos += 8
			return math.Inf(1), nil
		}
		if !p.strict && p.hasWord("NaN") {
			p.pos += 3
			return math.NaN(), nil
		}
		return nil, p.fail(p.pos, "unexpected character in JSON")
	}
}

func (p *jsonParser) hasWord(w string) bool {
	if !strings.HasPrefix(p.src[p.pos:], w) {
		return false
	}
	end := p.pos + len(w)
	if end < len(p.src) {
		c := p.src[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			return false
		}
	}
	return true
}

func (p *jsonParser) parseObject() (any, error) {
	p.pos++ // {
	obj := newJSONObject()
	afterComma := false
	p.skipWSAndComments()
	if p.peek() == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipWSAndComments()
		if p.eof() {
			return nil, p.fail(p.pos, "unterminated JSON object")
		}
		if p.peek() == '}' {
			// A '}' right after a comma is a trailing comma.
			if p.strict && afterComma {
				return nil, p.fail(p.pos, "trailing comma is not strict JSON")
			}
			p.pos++
			return obj, nil
		}
		key, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		p.skipWSAndComments()
		if p.peek() != ':' {
			return nil, p.fail(p.pos, "expected ':' in JSON object")
		}
		p.pos++
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		obj.set(key, v)
		p.skipWSAndComments()
		if p.eof() {
			return nil, p.fail(p.pos, "unterminated JSON object")
		}
		switch p.peek() {
		case ',':
			p.pos++
			afterComma = true
		case '}':
			p.pos++
			return obj, nil
		default:
			return nil, p.fail(p.pos, "expected ',' or '}' in JSON object")
		}
	}
}

// parseKey reads an object key: a quoted string or a bare identifier.
func (p *jsonParser) parseKey() (string, error) {
	c := p.peek()
	if c == '"' {
		return p.parseString()
	}
	if c == '\'' {
		if p.strict {
			return "", p.fail(p.pos, "single-quoted JSON keys are not strict JSON")
		}
		return p.parseString()
	}
	if p.strict {
		return "", p.fail(p.pos, "unquoted JSON keys are not strict JSON")
	}
	start := p.pos
	for !p.eof() {
		c := p.src[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return "", p.fail(p.pos, "expected JSON object key")
	}
	return p.src[start:p.pos], nil
}

func (p *jsonParser) parseArray() (any, error) {
	p.pos++ // [
	out := []any{}
	afterComma := false
	p.skipWSAndComments()
	if p.peek() == ']' {
		p.pos++
		return out, nil
	}
	for {
		p.skipWSAndComments()
		if p.eof() {
			return nil, p.fail(p.pos, "unterminated JSON array")
		}
		if p.peek() == ']' {
			if p.strict && afterComma {
				return nil, p.fail(p.pos, "trailing comma is not strict JSON")
			}
			p.pos++
			return out, nil
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipWSAndComments()
		if p.eof() {
			return nil, p.fail(p.pos, "unterminated JSON array")
		}
		switch p.peek() {
		case ',':
			p.pos++
			afterComma = true
		case ']':
			p.pos++
			return out, nil
		default:
			return nil, p.fail(p.pos, "expected ',' or ']' in JSON array")
		}
	}
}

func (p *jsonParser) parseString() (string, error) {
	quote := p.src[p.pos]
	if quote == '\'' && p.strict {
		return "", p.fail(p.pos, "single-quoted strings are not strict JSON")
	}
	p.pos++
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.fail(p.pos, "unterminated JSON string")
		}
		c := p.src[p.pos]
		if c == quote {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' {
			p.pos++
			if p.eof() {
				return "", p.fail(p.pos, "unterminated JSON escape")
			}
			e := p.src[p.pos]
			p.pos++
			switch e {
			case '"', '\'', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if p.pos+4 > len(p.src) {
					return "", p.fail(p.pos, "truncated JSON unicode escape")
				}
				n, err := strconv.ParseUint(p.src[p.pos:p.pos+4], 16, 32)
				if err != nil {
					return "", p.fail(p.pos, "invalid JSON unicode escape")
				}
				p.pos += 4
				// Combine surrogate pairs.
				r := rune(n)
				if r >= 0xD800 && r <= 0xDBFF && p.pos+6 <= len(p.src) &&
					p.src[p.pos] == '\\' && p.src[p.pos+1] == 'u' {
					n2, err := strconv.ParseUint(p.src[p.pos+2:p.pos+6], 16, 32)
					if err == nil && n2 >= 0xDC00 && n2 <= 0xDFFF {
						p.pos += 6
						r = 0x10000 + (r-0xD800)*0x400 + (rune(n2) - 0xDC00)
					}
				}
				b.WriteRune(r)
			default:
				return "", p.fail(p.pos-1, "invalid JSON escape")
			}
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
}

// parseNumber accepts JSON numbers plus JSON5 hexadecimal and leading/
// trailing decimal points.
func (p *jsonParser) parseNumber() (any, error) {
	start := p.pos
	if p.peek() == '+' || p.peek() == '-' {
		p.pos++
	}
	// Hexadecimal
	if p.pos+1 < len(p.src) && p.src[p.pos] == '0' && (p.src[p.pos+1] == 'x' || p.src[p.pos+1] == 'X') {
		if p.strict {
			return nil, p.fail(p.pos, "hexadecimal numbers are not strict JSON")
		}
		p.pos += 2
		hexStart := p.pos
		for !p.eof() && isHexDigit(p.src[p.pos]) {
			p.pos++
		}
		if p.pos == hexStart {
			return nil, p.fail(p.pos, "truncated JSON hex number")
		}
		n, err := strconv.ParseInt(p.src[hexStart:p.pos], 16, 64)
		if err != nil {
			return nil, p.fail(start, "invalid JSON hex number")
		}
		if p.src[start] == '-' {
			return -n, nil
		}
		return n, nil
	}
	digits := false
	for !p.eof() && isDigit(p.src[p.pos]) {
		p.pos++
		digits = true
	}
	if !p.eof() && p.src[p.pos] == '.' {
		p.pos++
		for !p.eof() && isDigit(p.src[p.pos]) {
			p.pos++
			digits = true
		}
	}
	if !digits {
		return nil, p.fail(p.pos, "invalid JSON number")
	}
	// Strict JSON requires at least one digit before the decimal point.
	if p.strict {
		if start < len(p.src) && (p.src[start] == '.' || (p.src[start] == '-' && start+1 < len(p.src) && p.src[start+1] == '.')) {
			return nil, p.fail(start, "leading decimal point is not strict JSON")
		}
	}
	if !p.eof() && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		p.pos++
		if !p.eof() && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		expDigits := false
		for !p.eof() && isDigit(p.src[p.pos]) {
			p.pos++
			expDigits = true
		}
		if !expDigits {
			return nil, p.fail(p.pos, "truncated JSON exponent")
		}
	}
	raw := p.src[start:p.pos]
	// A number without a decimal point or exponent that fits in int64 is an
	// INTEGER in SQLite's json_type()/typeof() terms.
	if !strings.ContainsAny(raw, ".eE") {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, p.fail(start, "invalid JSON number")
	}
	return f, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// normalizeJSON converts a decoded value into the shape used for rendering:
// ordered objects, int64 for whole numbers.
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case *jsonObject:
		for _, k := range x.keys {
			x.m[k] = normalizeJSON(x.m[k])
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalizeJSON(x[i])
		}
		return x
	}
	return v
}
