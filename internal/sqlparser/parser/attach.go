package parser

import (
	"github.com/oarkflow/sqlite/internal/sqlparser/ast"
	"github.com/oarkflow/sqlite/internal/sqlparser/lexer"
)

// parseAttach parses ATTACH [DATABASE] expr AS schema-name. ATTACH is not a
// reserved word in the lexer, so it arrives as a plain identifier.
func (p *Parser) parseAttach(pos int32) (ast.Statement, error) {
	p.advance() // ATTACH
	p.tryEatKeyword(lexer.DATABASE)
	file, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if err := p.eatKeyword(lexer.AS); err != nil {
		return nil, err
	}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	return arenaNode(&p.arena, ast.AttachStmt{File: file, Name: name, TokPos: pos}), nil
}

// parseDetach parses DETACH [DATABASE] schema-name.
func (p *Parser) parseDetach(pos int32) (ast.Statement, error) {
	p.advance() // DETACH
	p.tryEatKeyword(lexer.DATABASE)
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	return arenaNode(&p.arena, ast.DetachStmt{Name: name, TokPos: pos}), nil
}
