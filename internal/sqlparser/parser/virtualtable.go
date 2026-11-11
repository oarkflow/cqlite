package parser

import (
	"github.com/sujit/sqlite/internal/sqlparser/ast"
	"github.com/sujit/sqlite/internal/sqlparser/lexer"
)

// parseCreateVirtualTable parses CREATE VIRTUAL TABLE ... USING module(args).
// The module arguments are kept as raw text because each module defines its
// own argument grammar.
func (p *Parser) parseCreateVirtualTable(orReplace, temporary bool, pos int32) (ast.Statement, error) {
	p.advance() // VIRTUAL
	if err := p.eatKeyword(lexer.TABLE); err != nil {
		return nil, err
	}
	stmt := arenaNode(&p.arena, ast.CreateVirtualTableStmt{OrReplace: orReplace, Temporary: temporary, TokPos: pos})
	if p.is(lexer.IF) {
		p.advance()
		if !p.tryEatKeyword(lexer.NOT) {
			return nil, p.errorf("expected NOT in IF NOT EXISTS")
		}
		if !p.tryEatKeyword(lexer.EXISTS) {
			return nil, p.errorf("expected EXISTS in IF NOT EXISTS")
		}
		stmt.IfNotExists = true
	}
	name, err := p.parseQualifiedIdent()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	if !p.tryEatKeyword(lexer.USING) {
		return nil, p.errorf("expected USING in CREATE VIRTUAL TABLE")
	}
	mod, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	stmt.Module = mod
	if _, err := p.eat(lexer.LPAREN); err != nil {
		return nil, err
	}
	start := p.tok.Pos
	depth := 1
	for depth > 0 {
		if p.tok.Type == lexer.EOF {
			return nil, p.errorf("unterminated virtual table argument list")
		}
		switch p.tok.Type {
		case lexer.LPAREN:
			depth++
		case lexer.RPAREN:
			depth--
			if depth == 0 {
				end := int(p.tok.Pos)
				src := p.lex.Source()
				if int(start) < end && end <= len(src) {
					stmt.Args = src[start:end]
				}
				p.advance()
				return stmt, nil
			}
		}
		p.advance()
	}
	return stmt, nil
}
