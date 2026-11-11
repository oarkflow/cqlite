// Package lexer provides a zero-allocation, high-performance SQL lexer.
// It uses a hand-rolled state machine with perfect hash keyword lookup,
// achieving O(1) keyword recognition without any memory allocations.
package lexer

// TokenType identifies the type of a SQL token.
type TokenType uint8

const (
	// Special tokens
	ILLEGAL TokenType = iota
	EOF
	COMMENT
	WHITESPACE

	// Literals
	IDENT
	INT
	FLOAT
	STRING     // 'single quoted'
	BACKTICK   // `backtick quoted`
	DQUOTE     // "double quoted"
	HEXLIT     // 0x...
	BITLIT     // b'...' or B'...'
	NAMEDPARAM // :name or @name or $N

	// Operators & punctuation
	LPAREN    // (
	RPAREN    // )
	LBRACE    // {
	RBRACE    // }
	LBRACKET  // [
	RBRACKET  // ]
	COMMA     // ,
	SEMICOLON // ;
	COLON     // :
	ASSIGN    // :=
	DOT       // .
	DOTDOT    // ..
	STAR      // *
	PLUS      // +
	MINUS     // -
	SLASH     // /
	PERCENT   // %
	AMPERSAND // &
	PIPE      // |
	CARET     // ^
	TILDE     // ~
	BANG      // !
	QUESTION  // ?
	AT        // @
	HASH      // #
	DOLLAR    // $

	// Comparison operators
	EQ         // =
	NEQ        // != or <>
	LT         // <
	GT         // >
	LTE        // <=
	GTE        // >=
	LSHIFT     // <<
	RSHIFT     // >>
	DBAR       // ||
	DAMP       // &&
	DARROW     // =>
	ARROW      // ->
	DARROW2    // ->>
	HASHARROW  // #>
	HASHDARROW // #>>
	ATGT       // @>
	LTAT       // <@
	QMARKPIPE  // ?|
	QMARKAMP   // ?&

	// Keywords (DDL)
	kwSTART // marker
	ADD
	AFTER
	ALL
	ALTER
	ANALYZE
	AND
	AS
	ASC
	AUTO_INCREMENT
	BETWEEN
	BY
	CASCADE
	CASE
	CAST
	CHANGE
	CHARACTER
	CHECK
	COLLATE
	COLUMN
	COMMENT_KW
	CONSTRAINT
	CREATE
	CROSS
	DATABASE
	DEFAULT
	DEFERRABLE
	DEFERRED
	DELETE
	DESC
	DISTINCT
	DROP
	ELSE
	END
	ENGINE
	ESCAPE
	EXCEPT
	EXISTS
	EXPLAIN
	FALSE_KW
	FILTER
	FIRST
	FOR
	FOREIGN
	FROM
	FULL
	FUNCTION
	GROUP
	HAVING
	IF
	IGNORE
	IN
	INDEX
	INNER
	INSERT
	INTERSECT
	INTO
	IS
	JOIN
	KEY
	LAST
	LATERAL
	LEFT
	LIKE
	LIMIT
	MATCH
	MATERIALIZED
	NATURAL
	NO
	NOT
	NULL_KW
	NULLS
	OF
	OFFSET
	ON
	OR
	ORDER
	OUTER
	OVER
	PARTITION
	PRIMARY
	PROCEDURE
	RECURSIVE
	REFERENCES
	RENAME
	REPLACE
	RESTRICT
	RETURNING
	RIGHT
	ROLLBACK
	SELECT
	SET
	SHOW
	TABLE
	TABLES
	THEN
	TO
	TRANSACTION
	TRIGGER
	TRUE_KW
	TRUNCATE
	UNION
	UNIQUE
	UPDATE
	USE
	USING
	VALUES
	VIEW
	WHEN
	WHERE
	WINDOW
	WITH
	WITHOUT
	kwEND // marker

	// Data type keywords
	BIGINT
	BINARY
	BLOB
	BOOLEAN
	CHAR
	DATE
	DATETIME
	DECIMAL
	DOUBLE
	ENUM
	FLOAT_KW
	INT_KW
	INTEGER
	JSON
	JSONB
	LONGBLOB
	LONGTEXT
	MEDIUMBLOB
	MEDIUMINT
	MEDIUMTEXT
	NCHAR
	NUMERIC
	REAL
	SMALLINT
	TEXT
	TIME
	TIMESTAMP
	TINYBLOB
	TINYINT
	TINYTEXT
	VARBINARY
	VARCHAR
	YEAR
)

// String returns a human-readable representation of the token type.
func (t TokenType) String() string {
	if int(t) < len(tokenNames) {
		return tokenNames[t]
	}
	return "UNKNOWN"
}

var tokenNames = [...]string{
	ILLEGAL:    "ILLEGAL",
	EOF:        "EOF",
	COMMENT:    "COMMENT",
	WHITESPACE: "WHITESPACE",
	IDENT:      "IDENT",
	INT:        "INT",
	FLOAT:      "FLOAT",
	STRING:     "STRING",
	BACKTICK:   "BACKTICK",
	DQUOTE:     "DQUOTE",
	HEXLIT:     "HEXLIT",
	BITLIT:     "BITLIT",
	NAMEDPARAM: "NAMEDPARAM",
	LPAREN:     "(",
	RPAREN:     ")",
	LBRACE:     "{",
	RBRACE:     "}",
	LBRACKET:   "[",
	RBRACKET:   "]",
	COMMA:      ",",
	SEMICOLON:  ";",
	COLON:      ":",
	ASSIGN:     ":=",
	DOT:        ".",
	DOTDOT:     "..",
	STAR:       "*",
	PLUS:       "+",
	MINUS:      "-",
	SLASH:      "/",
	PERCENT:    "%",
	AMPERSAND:  "&",
	PIPE:       "|",
	CARET:      "^",
	TILDE:      "~",
	BANG:       "!",
	QUESTION:   "?",
	AT:         "@",
	HASH:       "#",
	DOLLAR:     "$",
	EQ:         "=",
	NEQ:        "!=",
	LT:         "<",
	GT:         ">",
	LTE:        "<=",
	GTE:        ">=",
	LSHIFT:     "<<",
	RSHIFT:     ">>",
	DBAR:       "||",
	DAMP:       "&&",
	DARROW:     "=>",
	ARROW:      "->",
	DARROW2:    "->>",
	HASHARROW:  "#>",
	HASHDARROW: "#>>",
	ATGT:       "@>",
	LTAT:       "<@",
	QMARKPIPE:  "?|",
	QMARKAMP:   "?&",
}
