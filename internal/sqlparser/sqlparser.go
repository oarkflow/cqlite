// Package sqlparser is a high-performance, zero-allocation SQL parser for Go.
//
// Design goals:
//   - Zero heap allocations in the hot path (lexer)
//   - O(1) keyword recognition via length-bucketed tables
//   - Pratt (top-down operator precedence) expression parser
//   - Arena allocator eliminates per-node GC pressure
//   - Supports a documented MySQL/PostgreSQL-oriented SQL subset
//   - Lossless document parsing for exact source preservation
//
// Usage:
//
//	stmt, err := sqlparser.ParseStatement("SELECT id, name FROM users WHERE id = 1")
//	stmts, err := sqlparser.ParseStatements(sql)
//	doc, err := sqlparser.ParseDocumentString(sql)
package sqlparser

import (
	"github.com/oarkflow/cqlite/internal/sqlparser/ast"
	"github.com/oarkflow/cqlite/internal/sqlparser/lexer"
	"github.com/oarkflow/cqlite/internal/sqlparser/parser"
)

// Re-export core types so callers only import this package.
type (
	Statement          = ast.Statement
	Expr               = ast.Expr
	SelectStmt         = ast.SelectStmt
	InsertStmt         = ast.InsertStmt
	UpdateStmt         = ast.UpdateStmt
	DeleteStmt         = ast.DeleteStmt
	CreateTableStmt    = ast.CreateTableStmt
	CreateDatabaseStmt = ast.CreateDatabaseStmt
	AlterDatabaseStmt  = ast.AlterDatabaseStmt
	DropDatabaseStmt   = ast.DropDatabaseStmt
	AlterTableStmt     = ast.AlterTableStmt
	DropTableStmt      = ast.DropTableStmt
	CallStmt           = ast.CallStmt
	TransactionStmt    = ast.TransactionStmt
	GenericDDLStmt     = ast.GenericDDLStmt
	ObjectDDLStmt      = ast.ObjectDDLStmt
	ParseOptions       = parser.ParseOptions
	Document           = parser.Document
	StatementSpan      = parser.StatementSpan
	ParseError         = parser.ParseError
	Token              = lexer.Token
	TokenType          = lexer.TokenType
)

// OwnedStatements is an arena-owned statement list.
type OwnedStatements = parser.OwnedStatements

// ParseStatementsOwned parses statements into a caller-owned arena so the AST
// remains valid across later parses.
func ParseStatementsOwned(src string) (*OwnedStatements, error) {
	return parser.ParseStatementsOwned(src)
}

// ParseExpression parses a standalone expression from a string.
func ParseExpression(src string) (Expr, error) {
	return parser.ParseExpression(src)
}

// ParseStatement parses a single SQL statement from a string.
// It returns the AST node and any parse error.
func ParseStatement(sql string) (Statement, error) {
	return parser.ParseStatement(sql)
}

// ParseStatements parses multiple semicolon-separated SQL statements.
func ParseStatements(sql string) ([]Statement, error) {
	return parser.ParseStatements(sql)
}

// ParseStatementsWithOptions parses semicolon-separated SQL with guardrails.
func ParseStatementsWithOptions(sql string, opts ParseOptions) ([]Statement, error) {
	return parser.ParseStatementsWithOptions(sql, opts)
}

// ParseDocument parses SQL while preserving whitespace, comments, and exact spans.
func ParseDocument(src []byte, opts ...ParseOptions) (*Document, error) {
	return parser.ParseDocument(src, opts...)
}

// ParseDocumentString parses SQL text while preserving whitespace, comments, and exact spans.
func ParseDocumentString(src string, opts ...ParseOptions) (*Document, error) {
	return parser.ParseDocumentString(src, opts...)
}

// Parser is a reusable, stateful SQL parser.
// Reuse a Parser across calls to amortise arena allocations.
type Parser struct {
	p *parser.Parser
}

// New creates a Parser backed by the given SQL bytes.
func New(src []byte) *Parser {
	return &Parser{p: parser.New(src)}
}

// NewString creates a Parser backed by the given SQL string.
func NewString(src string) *Parser {
	return &Parser{p: parser.NewString(src)}
}

// Reset reuses the Parser with new input, reusing internal allocations.
func (p *Parser) Reset(src []byte) {
	p.p.Reset(src)
}

// ResetString resets the parser using a string-backed lexer input.
func (p *Parser) ResetString(src string) {
	p.p.ResetString(src)
}

// Next returns the next statement or (nil, nil) at EOF.
func (p *Parser) Next() (Statement, error) {
	return p.p.ParseOne()
}

// Single parses exactly one statement and rejects trailing SQL.
func (p *Parser) Single() (Statement, error) {
	return p.p.ParseSingle()
}

// All parses all remaining statements.
func (p *Parser) All() ([]Statement, error) {
	return p.p.ParseAll()
}

// Tokenize breaks a SQL string into tokens.
// The returned slice is backed by the original byte slice to avoid copies.
// Provide a pre-allocated buffer to avoid heap allocation:
//
//	buf := make([]lexer.Token, 0, 128)
//	tokens := sqlparser.Tokenize([]byte(sql), buf)
func Tokenize(src []byte, buf []Token) []Token {
	return lexer.Tokenize(src, buf)
}

// TokenizeAll breaks SQL into tokens including whitespace and comments.
func TokenizeAll(src []byte, buf []Token) []Token {
	return lexer.TokenizeAll(src, buf)
}
