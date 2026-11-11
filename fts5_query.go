package db

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ftsNode is one node of a parsed FTS5 query expression.
type ftsNode interface{}

// ftsTerm matches a single indexed term, optionally as a prefix or as the
// first token of a column.
type ftsTerm struct {
	term   string
	prefix bool
	first  bool
	// cols restricts the match to these column indexes; nil means any column.
	cols []int
}

// ftsPhrase matches terms appearing in consecutive positions. The last term
// may be a prefix.
type ftsPhrase struct {
	terms  []string
	prefix bool
	first  bool
	cols   []int
}

type ftsAnd struct{ l, r ftsNode }
type ftsOr struct{ l, r ftsNode }
type ftsNot struct{ e ftsNode }

// ftsNear matches a phrase whose terms are within a token distance.
type ftsNear struct {
	terms []string
	dist  int
	cols  []int
}

type ftsQueryParser struct {
	src  string
	pos  int
	toks []ftsToken
	idx  int
	// cols is the schema used to resolve column filters in the query.
	cols []string
}

type ftsToken struct {
	kind byte // 'w' word, '"' phrase-start marker, 'o' operator, '(' ')' ':' ',' '*'
	text string
}

// parseFTSQuery compiles an FTS5 MATCH expression. The zero query matches
// every row, which is what an unconstrained scan of the virtual table does.
func parseFTSQuery(q string, cols []string) (ftsNode, error) {
	p := &ftsQueryParser{src: q, cols: cols}
	if err := p.lex(); err != nil {
		return nil, err
	}
	if len(p.toks) == 0 {
		return nil, nil
	}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.idx < len(p.toks) {
		return nil, fmt.Errorf("malformed MATCH expression near %q", p.toks[p.idx].text)
	}
	return node, nil
}

func (p *ftsQueryParser) lex() error {
	s := p.src
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == ':' || c == ',' || c == '*' || c == '^':
			p.toks = append(p.toks, ftsToken{kind: c, text: string(c)})
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			if j >= len(s) {
				return errors.New("unterminated phrase in MATCH expression")
			}
			p.toks = append(p.toks, ftsToken{kind: 'p', text: s[i+1 : j]})
			i = j + 1
		case c == '-' || c == '+':
			// Shorthand for NOT / required-term.
			p.toks = append(p.toks, ftsToken{kind: 'o', text: string(c)})
			i++
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n\r():, *\"^", rune(s[j])) {
				j++
			}
			word := s[i:j]
			switch strings.ToUpper(word) {
			case "AND", "OR", "NOT", "NEAR":
				p.toks = append(p.toks, ftsToken{kind: 'o', text: strings.ToUpper(word)})
			default:
				p.toks = append(p.toks, ftsToken{kind: 'w', text: word})
			}
			i = j
		}
	}
	return nil
}

func (p *ftsQueryParser) peek() (ftsToken, bool) {
	if p.idx >= len(p.toks) {
		return ftsToken{}, false
	}
	return p.toks[p.idx], true
}

func (p *ftsQueryParser) next() (ftsToken, bool) {
	t, ok := p.peek()
	if ok {
		p.idx++
	}
	return t, ok
}

func (p *ftsQueryParser) accept(text string) bool {
	if t, ok := p.peek(); ok && (t.text == text || t.text == strings.ToUpper(text)) {
		p.idx++
		return true
	}
	return false
}

func (p *ftsQueryParser) parseOr() (ftsNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		if t, ok := p.peek(); !ok || t.text != "OR" {
			return left, nil
		}
		p.idx++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = ftsOr{l: left, r: right}
	}
}

func (p *ftsQueryParser) parseAnd() (ftsNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok {
			return left, nil
		}
		switch t.text {
		case "OR":
			return left, nil
		case "AND":
			p.idx++
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = ftsAnd{l: left, r: right}
		case ")", ",", ":":
			return left, nil
		default:
			// Implicit AND between adjacent terms.
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = ftsAnd{l: left, r: right}
		}
	}
}

func (p *ftsQueryParser) parseUnary() (ftsNode, error) {
	t, ok := p.peek()
	if !ok {
		return nil, errors.New("unexpected end of MATCH expression")
	}
	switch {
	case t.text == "NOT" || t.text == "-":
		p.idx++
		e, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return ftsNot{e: e}, nil
	case t.text == "+":
		p.idx++
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *ftsQueryParser) parsePrimary() (ftsNode, error) {
	t, ok := p.peek()
	if !ok {
		return nil, errors.New("unexpected end of MATCH expression")
	}
	switch {
	case t.kind == '(':
		p.idx++
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.accept(")") {
			return nil, errors.New("unbalanced parentheses in MATCH expression")
		}
		return e, nil
	case t.kind == '^':
		p.idx++
		e, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return markFirst(e), nil
	case t.text == "NEAR":
		return p.parseNear()
	case t.kind == 'p':
		p.idx++
		return p.makePhrase(t.text, nil)
	}

	// Optional column filter: col:term or {col1 col2}:term
	var cols []int
	if t.kind == 'w' {
		if nxt, ok := p.peekAt(1); ok && nxt.kind == ':' {
			p.idx += 2
			cols = []int{}
			if name := p.columnIndex(t.text); name >= 0 {
				cols = append(cols, name)
			}
			return p.parseFilteredPrimary(cols)
		}
	}
	if t.kind == '{' {
		p.idx++
		var names []string
		for {
			n, ok := p.next()
			if !ok {
				return nil, errors.New("unterminated column list in MATCH expression")
			}
			if n.kind == '}' {
				break
			}
			if n.kind == 'w' {
				names = append(names, n.text)
			}
		}
		if !p.accept(":") {
			return nil, errors.New("expected ':' after column list in MATCH expression")
		}
		for _, nm := range names {
			if i := p.columnIndex(nm); i >= 0 {
				cols = append(cols, i)
			}
		}
		return p.parseFilteredPrimary(cols)
	}
	return p.parseFilteredPrimary(nil)
}

func (p *ftsQueryParser) parseFilteredPrimary(cols []int) (ftsNode, error) {
	t, ok := p.next()
	if !ok {
		return nil, errors.New("unexpected end of MATCH expression")
	}
	switch {
	case t.kind == '(':
		return nil, errors.New("column filter must apply to a term in MATCH expression")
	case t.kind == 'p':
		return p.makePhrase(t.text, cols)
	case t.kind == 'w':
		word := t.text
		prefix := false
		if nxt, ok := p.peek(); ok && nxt.kind == '*' {
			p.idx++
			prefix = true
		}
		term := strings.TrimPrefix(word, "^")
		first := strings.HasPrefix(word, "^")
		if prefix {
			return ftsPhrase{terms: []string{term}, prefix: true, first: first, cols: cols}, nil
		}
		return ftsTerm{term: term, first: first, cols: cols}, nil
	case t.kind == '^':
		p.idx--
		return p.parsePrimary()
	}
	return nil, fmt.Errorf("unexpected token %q in MATCH expression", t.text)
}

// makePhrase compiles a quoted phrase into an adjacency constraint.
func (p *ftsQueryParser) makePhrase(text string, cols []int) (ftsNode, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil, errors.New("empty phrase in MATCH expression")
	}
	if len(fields) == 1 {
		prefix := false
		word := fields[0]
		if strings.HasSuffix(word, "*") {
			word = strings.TrimSuffix(word, "*")
			prefix = true
		}
		if prefix {
			return ftsPhrase{terms: []string{word}, prefix: true, cols: cols}, nil
		}
		return ftsTerm{term: word, cols: cols}, nil
	}
	return ftsPhrase{terms: fields, cols: cols}, nil
}

func (p *ftsQueryParser) parseNear() (ftsNode, error) {
	p.idx++ // NEAR
	if !p.accept("(") {
		return nil, errors.New("expected '(' after NEAR")
	}
	var terms []string
	for {
		t, ok := p.peek()
		if !ok {
			return nil, errors.New("unterminated NEAR expression")
		}
		if t.kind == ')' || t.kind == ',' {
			break
		}
		if t.kind == 'p' {
			p.idx++
			terms = append(terms, strings.Fields(t.text)...)
			continue
		}
		if t.kind == 'w' {
			p.idx++
			terms = append(terms, t.text)
			continue
		}
		return nil, fmt.Errorf("unexpected token %q in NEAR expression", t.text)
	}
	dist := 10
	if p.accept(",") {
		n, ok := p.next()
		if !ok {
			return nil, errors.New("missing NEAR distance")
		}
		d, err := strconv.Atoi(n.text)
		if err != nil {
			return nil, errors.New("NEAR distance must be an integer")
		}
		dist = d
	}
	if !p.accept(")") {
		return nil, errors.New("unterminated NEAR expression")
	}
	if len(terms) < 2 {
		return nil, errors.New("NEAR expects at least two terms")
	}
	return ftsNear{terms: terms, dist: dist}, nil
}

// peekAt returns the token at an offset from the cursor.
func (p *ftsQueryParser) peekAt(off int) (ftsToken, bool) {
	if p.idx+off >= len(p.toks) {
		return ftsToken{}, false
	}
	return p.toks[p.idx+off], true
}

// columnIndex resolves a column name against the virtual table's columns. The
// parser does not know the schema, so a name that is not recognised is treated
// as "no such column" and matches nothing.
func (p *ftsQueryParser) columnIndex(name string) int {
	if p.cols == nil {
		return -1
	}
	lower := strings.ToLower(name)
	for i, c := range p.cols {
		if strings.ToLower(c) == lower {
			return i
		}
	}
	return -1
}

// withColumns attaches the virtual table's column list to the parser so
// column filters can be resolved.
func (p *ftsQueryParser) withColumns(cols []string) { p.cols = cols }

func markFirst(e ftsNode) ftsNode {
	switch n := e.(type) {
	case ftsTerm:
		n.first = true
		return n
	case ftsPhrase:
		n.first = true
		return n
	}
	return e
}
