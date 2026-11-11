package parser

import (
	"github.com/oarkflow/sqlite/internal/sqlparser/ast"
	"github.com/oarkflow/sqlite/internal/sqlparser/lexer"
)

// parseCreateTrigger parses SQLite's CREATE TRIGGER statement, including the
// BEGIN ... END body as a real statement list.
//
//	CREATE [TEMP] TRIGGER [IF NOT EXISTS] name
//	  [BEFORE|AFTER|INSTEAD OF] {INSERT|DELETE|UPDATE [OF cols]} ON table
//	  [FOR EACH ROW] [WHEN expr]
//	BEGIN step [; step]... END
func (p *Parser) parseCreateTrigger(orReplace, temporary bool, pos int32) (ast.Statement, error) {
	p.advance() // TRIGGER
	stmt := arenaNode(&p.arena, ast.CreateTriggerStmt{OrReplace: orReplace, Temporary: temporary, TokPos: pos})
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
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	stmt.Name = name

	switch {
	case p.eatIdentWord("before"):
		stmt.Timing = ast.TriggerBefore
	case p.tryEatKeyword(lexer.AFTER):
		stmt.Timing = ast.TriggerAfter
	case p.eatIdentWord("instead"):
		if !p.tryEatKeyword(lexer.OF) {
			return nil, p.errorf("expected OF after INSTEAD")
		}
		stmt.Timing = ast.TriggerInsteadOf
	}

	switch {
	case p.tryEatKeyword(lexer.INSERT):
		stmt.Event = ast.TriggerInsert
	case p.tryEatKeyword(lexer.DELETE):
		stmt.Event = ast.TriggerDelete
	case p.tryEatKeyword(lexer.UPDATE):
		stmt.Event = ast.TriggerUpdate
		if p.tryEatKeyword(lexer.OF) {
			for {
				col, err := p.parseIdent()
				if err != nil {
					return nil, err
				}
				stmt.UpdateOf = append(stmt.UpdateOf, col)
				if !p.tryEat(lexer.COMMA) {
					break
				}
			}
		}
	default:
		return nil, p.errorf("expected INSERT, DELETE or UPDATE in CREATE TRIGGER")
	}

	if err := p.eatKeyword(lexer.ON); err != nil {
		return nil, err
	}
	table, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	stmt.Table = table

	// Optional FOR EACH ROW
	if p.tryEatKeyword(lexer.FOR) {
		p.eatIdentWord("each")
		p.eatIdentWord("row")
		stmt.ForEachRow = true
	}

	if p.tryEatKeyword(lexer.WHEN) {
		whenStart := p.tok.Pos
		when, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		stmt.When = when
		whenEnd := int(p.tok.Pos)
		src := p.lex.Source()
		if int(whenStart) < whenEnd && whenEnd <= len(src) {
			stmt.WhenRaw = src[whenStart:whenEnd]
		}
	}

	// AS is optional in SQLite but accepted.
	p.tryEatKeyword(lexer.AS)

	if !p.eatIdentWord("begin") {
		return nil, p.errorf("expected BEGIN to open the trigger body")
	}
	bodyStart := p.tok.Pos
	for {
		p.skipStmtSeparators()
		if p.tok.Type == lexer.EOF {
			return nil, p.errorf("unterminated trigger body: expected END")
		}
		if p.is(lexer.END) {
			break
		}
		step, err := p.parseTriggerStep()
		if err != nil {
			return nil, err
		}
		stmt.Body = append(stmt.Body, step)
		p.skipStmtSeparators()
	}
	bodyEnd := int(p.tok.Pos)
	p.advance() // END
	src := p.lex.Source()
	if int(bodyStart) < bodyEnd && bodyEnd <= len(src) {
		stmt.BodyRaw = src[bodyStart:bodyEnd]
	}
	return stmt, nil
}

// eatIdentWord consumes a non-reserved word written as a plain identifier.
func (p *Parser) eatIdentWord(word string) bool {
	if p.tok.Type == lexer.IDENT && equalASCIIFold(p.tok.Raw, word) {
		p.advance()
		return true
	}
	return false
}

// skipStmtSeparators consumes semicolons between trigger body steps.
func (p *Parser) skipStmtSeparators() {
	for p.tok.Type == lexer.SEMICOLON {
		p.advance()
	}
}

// parseTriggerStep parses one statement of a trigger body: a DML statement or
// a RAISE() command.
func (p *Parser) parseTriggerStep() (ast.Statement, error) {
	if p.tok.Type == lexer.IDENT && equalASCIIFold(p.tok.Raw, "raise") {
		return p.parseRaiseStmt()
	}
	return p.parseStatement()
}

// parseRaiseStmt parses RAISE(IGNORE) and RAISE(level, message).
func (p *Parser) parseRaiseStmt() (ast.Statement, error) {
	pos := p.tok.Pos
	p.advance() // RAISE
	if _, err := p.eat(lexer.LPAREN); err != nil {
		return nil, err
	}
	stmt := arenaNode(&p.arena, ast.RaiseTriggerStmt{TokPos: pos})
	// The action word (ignore/rollback/abort/fail) may lex as a keyword.
	if p.tok.Type == lexer.EOF || p.tok.Type == lexer.RPAREN {
		return nil, p.errorf("expected RAISE action")
	}
	stmt.Action = lowerASCIIBytes(p.tok.Raw)
	p.advance()
	if p.tryEat(lexer.COMMA) {
		if p.tok.Type != lexer.STRING {
			return nil, p.errorf("expected RAISE message string")
		}
		stmt.Message = p.tok.Raw
		p.advance()
	}
	if _, err := p.eat(lexer.RPAREN); err != nil {
		return nil, err
	}
	return stmt, nil
}

// parseDropTrigger parses DROP TRIGGER [IF EXISTS] name.
func (p *Parser) parseDropTrigger(pos int32) (ast.Statement, error) {
	p.advance() // TRIGGER
	stmt := arenaNode(&p.arena, ast.DropTriggerStmt{TokPos: pos})
	if p.is(lexer.IF) {
		p.advance()
		if !p.tryEatKeyword(lexer.EXISTS) {
			return nil, p.errorf("expected EXISTS in IF EXISTS")
		}
		stmt.IfExists = true
	}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	return stmt, nil
}

func lowerASCIIBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return out
}
