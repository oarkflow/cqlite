package parser

import (
	"unsafe"

	"github.com/oarkflow/cqlite/internal/sqlparser/ast"
	"github.com/oarkflow/cqlite/internal/sqlparser/lexer"
)

// Document is a lossless parse result. Source and Tokens preserve the exact
// input bytes, including whitespace and comments.
type Document struct {
	Source     []byte
	Tokens     []lexer.Token
	Statements []StatementSpan
}

// SQL returns the original SQL bytes as a string.
func (d *Document) SQL() string {
	if d == nil || len(d.Source) == 0 {
		return ""
	}
	return unsafe.String(&d.Source[0], len(d.Source))
}

// StatementSpan records one parsed statement and the exact source byte range.
type StatementSpan struct {
	Statement ast.Statement
	Start     int32
	End       int32
	Source    []byte
}

// SQL returns the exact source slice for this statement span.
func (s StatementSpan) SQL() string {
	if len(s.Source) == 0 || s.Start < 0 || s.End < s.Start || int(s.End) > len(s.Source) {
		return ""
	}
	src := s.Source[s.Start:s.End]
	if len(src) == 0 {
		return ""
	}
	return unsafe.String(&src[0], len(src))
}

// ParseDocument parses SQL while preserving all tokens and exact statement spans.
func ParseDocument(src []byte, opts ...ParseOptions) (*Document, error) {
	cfg := defaultParseOptions()
	if len(opts) > 0 {
		cfg = opts[0]
	}
	if cfg.MaxBytes > 0 && len(src) > cfg.MaxBytes {
		return nil, &ParseError{Code: "MAX_BYTES", Msg: "input exceeds MaxBytes", Line: 1, Col: 1}
	}
	toks := lexer.TokenizeAll(src, nil)
	if cfg.MaxTokens > 0 && len(toks) > cfg.MaxTokens {
		line, col := lexer.ComputeLineCol(src, int(toks[cfg.MaxTokens].Pos))
		return nil, &ParseError{Code: "MAX_TOKENS", Msg: "token count exceeds MaxTokens", Pos: toks[cfg.MaxTokens].Pos, Line: line, Col: col}
	}
	doc := &Document{Source: src, Tokens: toks}
	start := int32(-1)
	lastContentEnd := int32(0)
	for _, tok := range toks {
		switch tok.Type {
		case lexer.WHITESPACE, lexer.COMMENT:
			continue
		case lexer.EOF:
			if start >= 0 {
				if err := parseDocumentStatement(doc, start, lastContentEnd, cfg); err != nil {
					return nil, err
				}
			}
		case lexer.SEMICOLON:
			if start >= 0 {
				end := tokenEnd(tok)
				if err := parseDocumentStatement(doc, start, end, cfg); err != nil {
					return nil, err
				}
				if cfg.MaxStatements > 0 && len(doc.Statements) > cfg.MaxStatements {
					return nil, &ParseError{Code: "MAX_STATEMENTS", Msg: "statement count exceeds MaxStatements", Pos: tok.Pos, Line: 1, Col: 1}
				}
				start = -1
			}
		default:
			if start < 0 {
				start = tok.Pos
			}
			lastContentEnd = tokenEnd(tok)
		}
	}
	return doc, nil
}

// ParseDocumentString parses SQL text while preserving source spans.
func ParseDocumentString(src string, opts ...ParseOptions) (*Document, error) {
	if len(src) == 0 {
		return ParseDocument(nil, opts...)
	}
	b := unsafe.Slice(unsafe.StringData(src), len(src))
	return ParseDocument(b, opts...)
}

func parseDocumentStatement(doc *Document, start, end int32, opts ParseOptions) error {
	if end < start {
		return nil
	}
	segment := doc.Source[start:end]
	p := New(segment)
	p.opts = opts
	p.tokenCount = 0
	p.depth = 0
	p.lex.Init(segment)
	p.tok = p.nextToken()
	stmt, err := p.ParseOne()
	if err != nil {
		return err
	}
	if stmt == nil {
		return nil
	}
	doc.Statements = append(doc.Statements, StatementSpan{
		Statement: stmt,
		Start:     start,
		End:       end,
		Source:    doc.Source,
	})
	return nil
}

func tokenEnd(tok lexer.Token) int32 {
	return tok.Pos + int32(len(tok.Raw))
}
