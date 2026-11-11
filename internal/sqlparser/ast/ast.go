// Package ast defines the SQL Abstract Syntax Tree.
// All nodes are value types where possible to minimize pointer chasing
// and improve cache locality.
package ast

import "github.com/oarkflow/cqlite/internal/sqlparser/lexer"

// Node is implemented by every AST node.
type Node interface {
	node()
	// Pos returns the byte offset of the first token.
	Pos() int32
}

// Statement is a top-level SQL statement.
type Statement interface {
	Node
	stmtNode()
}

// Expr is a SQL expression.
type Expr interface {
	Node
	exprNode()
}

// ---- Expressions ----

// Ident is a (possibly quoted) identifier.
type Ident struct {
	Raw      []byte // original bytes including quotes
	Unquoted string // resolved name
	TokPos   int32
}

func (n *Ident) node()      {}
func (n *Ident) exprNode()  {}
func (n *Ident) Pos() int32 { return n.TokPos }

// QualifiedIdent is a dotted name, e.g. schema.table.column.
type QualifiedIdent struct {
	Parts []*Ident
}

func (n *QualifiedIdent) node()     {}
func (n *QualifiedIdent) exprNode() {}
func (n *QualifiedIdent) Pos() int32 {
	if len(n.Parts) > 0 {
		return n.Parts[0].TokPos
	}
	return -1
}

// StarExpr represents *.
type StarExpr struct {
	TokPos int32
}

func (n *StarExpr) node()      {}
func (n *StarExpr) exprNode()  {}
func (n *StarExpr) Pos() int32 { return n.TokPos }

// Literal is a numeric, string, bool, hex, or bit literal.
type Literal struct {
	Raw    []byte
	Kind   lexer.TokenType
	TokPos int32
}

func (n *Literal) node()      {}
func (n *Literal) exprNode()  {}
func (n *Literal) Pos() int32 { return n.TokPos }

// NullLit is NULL.
type NullLit struct{ TokPos int32 }

func (n *NullLit) node()      {}
func (n *NullLit) exprNode()  {}
func (n *NullLit) Pos() int32 { return n.TokPos }

// Param is a query parameter: ?, :name, @name, $N.
type Param struct {
	Raw    []byte
	TokPos int32
}

func (n *Param) node()      {}
func (n *Param) exprNode()  {}
func (n *Param) Pos() int32 { return n.TokPos }

// NamedArg is a named function/procedure argument: name => expr or name := expr.
type NamedArg struct {
	Name   *QualifiedIdent
	Value  Expr
	Op     lexer.TokenType
	TokPos int32
}

func (n *NamedArg) node()      {}
func (n *NamedArg) exprNode()  {}
func (n *NamedArg) Pos() int32 { return n.TokPos }

// BinaryExpr is a binary operation: expr op expr.
type BinaryExpr struct {
	Left, Right Expr
	Op          lexer.TokenType
	TokPos      int32
}

func (n *BinaryExpr) node()      {}
func (n *BinaryExpr) exprNode()  {}
func (n *BinaryExpr) Pos() int32 { return n.TokPos }

// UnaryExpr is a prefix unary operation.
type UnaryExpr struct {
	Expr   Expr
	Op     lexer.TokenType
	TokPos int32
}

func (n *UnaryExpr) node()      {}
func (n *UnaryExpr) exprNode()  {}
func (n *UnaryExpr) Pos() int32 { return n.TokPos }

// FuncCall is a function invocation.
type FuncCall struct {
	Name     *QualifiedIdent
	Args     []Expr
	Distinct bool
	Star     bool // COUNT(*)
	Filter   Expr
	Over     *WindowSpec
	TokPos   int32
}

func (n *FuncCall) node()      {}
func (n *FuncCall) exprNode()  {}
func (n *FuncCall) Pos() int32 { return n.TokPos }

// CaseExpr is CASE ... END.
type CaseExpr struct {
	Operand Expr // nil for searched case
	Whens   []WhenClause
	Else    Expr
	TokPos  int32
}
type WhenClause struct {
	Cond, Result Expr
}

func (n *CaseExpr) node()      {}
func (n *CaseExpr) exprNode()  {}
func (n *CaseExpr) Pos() int32 { return n.TokPos }

// BetweenExpr is expr [NOT] BETWEEN lo AND hi.
type BetweenExpr struct {
	Expr   Expr
	Lo, Hi Expr
	Not    bool
	TokPos int32
}

func (n *BetweenExpr) node()      {}
func (n *BetweenExpr) exprNode()  {}
func (n *BetweenExpr) Pos() int32 { return n.TokPos }

// InExpr is expr [NOT] IN (list) or expr [NOT] IN (subquery).
type InExpr struct {
	Expr   Expr
	List   []Expr
	Subq   *SelectStmt
	Not    bool
	TokPos int32
}

func (n *InExpr) node()      {}
func (n *InExpr) exprNode()  {}
func (n *InExpr) Pos() int32 { return n.TokPos }

// LikeExpr is expr [NOT] LIKE pattern [ESCAPE e].
type LikeExpr struct {
	Expr, Pattern, Escape Expr
	Not                   bool
	TokPos                int32
}

func (n *LikeExpr) node()      {}
func (n *LikeExpr) exprNode()  {}
func (n *LikeExpr) Pos() int32 { return n.TokPos }

// IsNullExpr is expr IS [NOT] NULL.
type IsNullExpr struct {
	Expr   Expr
	Not    bool
	TokPos int32
}

func (n *IsNullExpr) node()      {}
func (n *IsNullExpr) exprNode()  {}
func (n *IsNullExpr) Pos() int32 { return n.TokPos }

// ExistsExpr is EXISTS (subquery).
type ExistsExpr struct {
	Subq   *SelectStmt
	Not    bool
	TokPos int32
}

func (n *ExistsExpr) node()      {}
func (n *ExistsExpr) exprNode()  {}
func (n *ExistsExpr) Pos() int32 { return n.TokPos }

// SubqueryExpr is a scalar subquery.
type SubqueryExpr struct {
	Subq   *SelectStmt
	TokPos int32
}

func (n *SubqueryExpr) node()      {}
func (n *SubqueryExpr) exprNode()  {}
func (n *SubqueryExpr) Pos() int32 { return n.TokPos }

// CastExpr is CAST(expr AS type).
type CastExpr struct {
	Expr   Expr
	Type   *DataType
	TokPos int32
}

func (n *CastExpr) node()      {}
func (n *CastExpr) exprNode()  {}
func (n *CastExpr) Pos() int32 { return n.TokPos }

// IntervalExpr is INTERVAL expr unit.
type IntervalExpr struct {
	Expr   Expr
	Unit   []byte
	TokPos int32
}

func (n *IntervalExpr) node()      {}
func (n *IntervalExpr) exprNode()  {}
func (n *IntervalExpr) Pos() int32 { return n.TokPos }

// ---- Data types ----

// DataType represents a SQL column type.
type DataType struct {
	Name      []byte
	Precision int
	Scale     int
	Unsigned  bool
	Zerofill  bool
	Charset   []byte
	Collation []byte
	EnumVals  [][]byte // for ENUM/SET
	TokPos    int32
}

// ---- Table references ----

// TableRef is a table reference (FROM clause).
type TableRef interface {
	Node
	tableRefNode()
}

// SimpleTable is a named table with optional alias.
type SimpleTable struct {
	Name    *QualifiedIdent
	Alias   *Ident
	Columns []*Ident
	Lateral bool
}

func (n *SimpleTable) node()         {}
func (n *SimpleTable) tableRefNode() {}
func (n *SimpleTable) Pos() int32    { return n.Name.Pos() }

// SubqueryTable is (SELECT ...) [AS alias].
type SubqueryTable struct {
	Subq    *SelectStmt
	Alias   *Ident
	Columns []*Ident
	Lateral bool
	TokPos  int32
}

func (n *SubqueryTable) node()         {}
func (n *SubqueryTable) tableRefNode() {}
func (n *SubqueryTable) Pos() int32    { return n.TokPos }

// ValuesTable is a VALUES row source in FROM.
type ValuesTable struct {
	Rows    [][]Expr
	Alias   *Ident
	Columns []*Ident
	Lateral bool
	TokPos  int32
}

func (n *ValuesTable) node()         {}
func (n *ValuesTable) tableRefNode() {}
func (n *ValuesTable) Pos() int32    { return n.TokPos }

// JoinTable represents a JOIN expression.
type JoinTable struct {
	Left, Right TableRef
	Kind        JoinKind
	On          Expr
	Using       []*Ident
	TokPos      int32
}
type JoinKind uint8

const (
	InnerJoin JoinKind = iota
	LeftJoin
	RightJoin
	FullJoin
	CrossJoin
	NaturalJoin
)

func (n *JoinTable) node()         {}
func (n *JoinTable) tableRefNode() {}
func (n *JoinTable) Pos() int32    { return n.TokPos }

// ---- DML Statements ----

// SelectStmt represents a SELECT statement.
type SelectStmt struct {
	With       *WithClause
	Distinct   bool
	DistinctOn []Expr
	Columns    []SelectColumn
	From       []TableRef
	Where      Expr
	GroupBy    []Expr
	Having     Expr
	Windows    []WindowDef
	OrderBy    []OrderByItem
	Limit      *LimitClause
	Lock       *LockClause
	SetOp      *SetOperation // UNION/INTERSECT/EXCEPT
	TokPos     int32
}

func (n *SelectStmt) node()      {}
func (n *SelectStmt) stmtNode()  {}
func (n *SelectStmt) exprNode()  {} // SELECT can appear as expr in some dialects
func (n *SelectStmt) Pos() int32 { return n.TokPos }

// WithClause is a Common Table Expression prefix.
type WithClause struct {
	Recursive bool
	CTEs      []CTE
}
type CTE struct {
	Name    *Ident
	Columns []*Ident
	Subq    *SelectStmt
}

// SelectColumn is a single column in a SELECT list.
type SelectColumn struct {
	Expr      Expr
	Alias     *Ident
	Star      bool // * or table.*
	Qualifier *QualifiedIdent
}

// OrderByItem is a single ORDER BY key.
type OrderByItem struct {
	Expr       Expr
	Desc       bool
	NullsFirst *bool
}

// WindowDef is a named WINDOW clause entry.
type WindowDef struct {
	Name *Ident
	Spec *WindowSpec
}

// WindowSpec is the supported shape of OVER (...).
type WindowSpec struct {
	Name        *Ident
	PartitionBy []Expr
	OrderBy     []OrderByItem
	Raw         []byte
	TokPos      int32
}

// LockClause is SELECT ... FOR UPDATE/SHARE.
type LockClause struct {
	Strength []byte
	Tables   []*Ident
	NoWait   bool
	SkipLock bool
	TokPos   int32
}

// LimitClause is LIMIT count [OFFSET skip].
type LimitClause struct {
	Count  Expr
	Offset Expr
}

// SetOperation chains SELECT statements.
type SetOperation struct {
	Op    SetOp
	All   bool
	Right *SelectStmt
}
type SetOp uint8

const (
	Union SetOp = iota
	Intersect
	Except
)

// InsertStmt represents an INSERT statement.
type InsertStmt struct {
	With                *WithClause
	Table               *QualifiedIdent
	Columns             []*Ident
	Values              [][]Expr // rows
	ValuesExpr          Expr     // whole VALUES payload, e.g. :rows or :row
	Set                 []Assignment
	Select              *SelectStmt
	DefaultValues       bool
	OnDupKey            []Assignment
	OnConflictTarget    []*Ident
	OnConflictWhere     Expr
	OnConflictDoNothing bool
	OnConflictUpdate    []Assignment
	Returning           []SelectColumn
	Ignore              bool
	Replace             bool // REPLACE INTO
	TokPos              int32
}

func (n *InsertStmt) node()      {}
func (n *InsertStmt) stmtNode()  {}
func (n *InsertStmt) Pos() int32 { return n.TokPos }

// Assignment is col = expr.
type Assignment struct {
	Column *Ident
	Value  Expr
}

// UpdateStmt represents an UPDATE statement.
type UpdateStmt struct {
	With      *WithClause
	Tables    []TableRef
	Set       []Assignment
	Where     Expr
	Order     []OrderByItem
	Limit     *LimitClause
	Returning []SelectColumn
	TokPos    int32
}

func (n *UpdateStmt) node()      {}
func (n *UpdateStmt) stmtNode()  {}
func (n *UpdateStmt) Pos() int32 { return n.TokPos }

// DeleteStmt represents a DELETE statement.
type DeleteStmt struct {
	With      *WithClause
	Tables    []*QualifiedIdent
	From      []TableRef
	Using     []TableRef
	Where     Expr
	Order     []OrderByItem
	Limit     *LimitClause
	Returning []SelectColumn
	TokPos    int32
}

func (n *DeleteStmt) node()      {}
func (n *DeleteStmt) stmtNode()  {}
func (n *DeleteStmt) Pos() int32 { return n.TokPos }

// ---- DDL Statements ----

// CreateTableStmt represents CREATE [TEMPORARY] TABLE.
type CreateTableStmt struct {
	Table       *QualifiedIdent
	Temporary   bool
	Unlogged    bool
	OrReplace   bool
	IfNotExists bool
	Columns     []*ColumnDef
	Constraints []*TableConstraint
	Options     []TableOption
	Select      *SelectStmt // CREATE TABLE ... AS SELECT
	Like        *QualifiedIdent
	TokPos      int32
}

func (n *CreateTableStmt) node()      {}
func (n *CreateTableStmt) stmtNode()  {}
func (n *CreateTableStmt) Pos() int32 { return n.TokPos }

// ColumnDef defines a table column.
type ColumnDef struct {
	Name          *Ident
	Type          *DataType
	NotNull       bool
	Default       Expr
	AutoIncrement bool
	PrimaryKey    bool
	Unique        bool
	Comment       *Literal
	References    *ForeignKeyRef
	Check         Expr
	Generated     *GeneratedCol
	Identity      bool
	OnUpdate      Expr
	TokPos        int32
}

type GeneratedCol struct {
	Expr   Expr
	Stored bool // STORED vs VIRTUAL
}

// TableConstraint is a table-level constraint.
type TableConstraint struct {
	Name      *Ident
	Type      ConstraintType
	Columns   []*IndexColDef
	RefTable  *QualifiedIdent
	RefCols   []*Ident
	OnDelete  RefAction
	OnUpdate  RefAction
	Check     Expr
	IndexType []byte // BTREE, HASH
	TokPos    int32
}
type ConstraintType uint8

const (
	PrimaryKeyConstraint ConstraintType = iota
	UniqueConstraint
	IndexConstraint
	ForeignKeyConstraint
	CheckConstraint
	FulltextConstraint
	SpatialConstraint
)

type RefAction uint8

const (
	NoAction RefAction = iota
	Restrict
	Cascade
	SetNull
	SetDefault
)

// ForeignKeyRef is a REFERENCES clause on a column.
type ForeignKeyRef struct {
	Table    *QualifiedIdent
	Columns  []*Ident
	OnDelete RefAction
	OnUpdate RefAction
}

// IndexColDef is a column in an index definition.
type IndexColDef struct {
	Name   *Ident
	Length *int
	Desc   bool
}

// TableOption is a table-level option, e.g. ENGINE=InnoDB.
type TableOption struct {
	Key   []byte
	Value []byte
}

// AlterTableStmt represents ALTER TABLE.
type AlterTableStmt struct {
	Table  *QualifiedIdent
	Cmds   []AlterCmd
	TokPos int32
}

func (n *AlterTableStmt) node()      {}
func (n *AlterTableStmt) stmtNode()  {}
func (n *AlterTableStmt) Pos() int32 { return n.TokPos }

type AlterCmd interface {
	Node
	alterCmdNode()
}

type AddColumnCmd struct {
	Col    *ColumnDef
	First  bool
	After  *Ident
	TokPos int32
}

func (c *AddColumnCmd) node()         {}
func (c *AddColumnCmd) alterCmdNode() {}
func (c *AddColumnCmd) Pos() int32    { return c.TokPos }

type DropColumnCmd struct {
	Name   *Ident
	TokPos int32
}

func (c *DropColumnCmd) node()         {}
func (c *DropColumnCmd) alterCmdNode() {}
func (c *DropColumnCmd) Pos() int32    { return c.TokPos }

type ModifyColumnCmd struct {
	Col    *ColumnDef
	First  bool
	After  *Ident
	TokPos int32
}

func (c *ModifyColumnCmd) node()         {}
func (c *ModifyColumnCmd) alterCmdNode() {}
func (c *ModifyColumnCmd) Pos() int32    { return c.TokPos }

type AddConstraintCmd struct {
	Constraint *TableConstraint
	TokPos     int32
}

func (c *AddConstraintCmd) node()         {}
func (c *AddConstraintCmd) alterCmdNode() {}
func (c *AddConstraintCmd) Pos() int32    { return c.TokPos }

type DropIndexCmd struct {
	Name   *Ident
	TokPos int32
}

func (c *DropIndexCmd) node()         {}
func (c *DropIndexCmd) alterCmdNode() {}
func (c *DropIndexCmd) Pos() int32    { return c.TokPos }

type DropConstraintCmd struct {
	Name     *Ident
	IfExists bool
	TokPos   int32
}

func (c *DropConstraintCmd) node()         {}
func (c *DropConstraintCmd) alterCmdNode() {}
func (c *DropConstraintCmd) Pos() int32    { return c.TokPos }

type AlterColumnCmd struct {
	Name   *Ident
	Action []byte
	Expr   Expr
	TokPos int32
}

func (c *AlterColumnCmd) node()         {}
func (c *AlterColumnCmd) alterCmdNode() {}
func (c *AlterColumnCmd) Pos() int32    { return c.TokPos }

type RenameTableCmd struct {
	NewName *QualifiedIdent
	TokPos  int32
}

func (c *RenameTableCmd) node()         {}
func (c *RenameTableCmd) alterCmdNode() {}
func (c *RenameTableCmd) Pos() int32    { return c.TokPos }

// CreateIndexStmt represents CREATE [UNIQUE|FULLTEXT|SPATIAL] INDEX.
type CreateIndexStmt struct {
	Name     *Ident
	Table    *QualifiedIdent
	Columns  []*IndexColDef
	Type     ConstraintType
	IndexAlg []byte
	TokPos   int32
}

func (n *CreateIndexStmt) node()      {}
func (n *CreateIndexStmt) stmtNode()  {}
func (n *CreateIndexStmt) Pos() int32 { return n.TokPos }

// DropTableStmt represents DROP TABLE.
type DropTableStmt struct {
	Tables   []*QualifiedIdent
	IfExists bool
	Cascade  bool
	TokPos   int32
}

func (n *DropTableStmt) node()      {}
func (n *DropTableStmt) stmtNode()  {}
func (n *DropTableStmt) Pos() int32 { return n.TokPos }

// DropIndexStmt represents DROP INDEX.
type DropIndexStmt struct {
	Name     *Ident
	Table    *QualifiedIdent
	IfExists bool
	TokPos   int32
}

func (n *DropIndexStmt) node()      {}
func (n *DropIndexStmt) stmtNode()  {}
func (n *DropIndexStmt) Pos() int32 { return n.TokPos }

// CreateViewStmt represents CREATE VIEW.
type CreateViewStmt struct {
	Name         *QualifiedIdent
	Columns      []*Ident
	Select       *SelectStmt
	OrReplace    bool
	Materialized bool
	TokPos       int32
}

func (n *CreateViewStmt) node()      {}
func (n *CreateViewStmt) stmtNode()  {}
func (n *CreateViewStmt) Pos() int32 { return n.TokPos }

// CreateDatabaseStmt represents CREATE DATABASE / SCHEMA.
type CreateDatabaseStmt struct {
	Name        *Ident
	IfNotExists bool
	Options     []TableOption
	TokPos      int32
}

func (n *CreateDatabaseStmt) node()      {}
func (n *CreateDatabaseStmt) stmtNode()  {}
func (n *CreateDatabaseStmt) Pos() int32 { return n.TokPos }

// AlterDatabaseStmt represents ALTER DATABASE / SCHEMA.
type AlterDatabaseStmt struct {
	Name    *Ident
	Options []TableOption
	TokPos  int32
}

func (n *AlterDatabaseStmt) node()      {}
func (n *AlterDatabaseStmt) stmtNode()  {}
func (n *AlterDatabaseStmt) Pos() int32 { return n.TokPos }

// DropDatabaseStmt represents DROP DATABASE / SCHEMA.
type DropDatabaseStmt struct {
	Name     *Ident
	IfExists bool
	TokPos   int32
}

func (n *DropDatabaseStmt) node()      {}
func (n *DropDatabaseStmt) stmtNode()  {}
func (n *DropDatabaseStmt) Pos() int32 { return n.TokPos }

// TruncateStmt represents TRUNCATE TABLE.
type TruncateStmt struct {
	Table  *QualifiedIdent
	TokPos int32
}

func (n *TruncateStmt) node()      {}
func (n *TruncateStmt) stmtNode()  {}
func (n *TruncateStmt) Pos() int32 { return n.TokPos }

// UseStmt represents USE database.
type UseStmt struct {
	Database *Ident
	TokPos   int32
}

func (n *UseStmt) node()      {}
func (n *UseStmt) stmtNode()  {}
func (n *UseStmt) Pos() int32 { return n.TokPos }

// ShowStmt represents SHOW TABLES / SHOW DATABASES / etc.
type ShowStmt struct {
	What   []byte
	Like   *Literal
	Where  Expr
	TokPos int32
}

func (n *ShowStmt) node()      {}
func (n *ShowStmt) stmtNode()  {}
func (n *ShowStmt) Pos() int32 { return n.TokPos }

// ExplainStmt represents EXPLAIN / DESCRIBE.
type ExplainStmt struct {
	Stmt   Statement
	TokPos int32
}

func (n *ExplainStmt) node()      {}
func (n *ExplainStmt) stmtNode()  {}
func (n *ExplainStmt) Pos() int32 { return n.TokPos }

// CallStmt represents CALL proc(args...).
type CallStmt struct {
	Name   *QualifiedIdent
	Args   []Expr
	TokPos int32
}

func (n *CallStmt) node()      {}
func (n *CallStmt) stmtNode()  {}
func (n *CallStmt) Pos() int32 { return n.TokPos }

// TransactionStmt represents BEGIN/COMMIT/ROLLBACK/SAVEPOINT/RELEASE/START/SET TRANSACTION.
type TransactionStmt struct {
	Action    []byte
	Savepoint *Ident
	Options   [][]byte
	TokPos    int32
}

func (n *TransactionStmt) node()      {}
func (n *TransactionStmt) stmtNode()  {}
func (n *TransactionStmt) Pos() int32 { return n.TokPos }

// GenericDDLStmt is a permissive DDL representation for statements not yet fully modeled.
type GenericDDLStmt struct {
	Verb   []byte
	Object []byte
	Name   *Ident
	Body   []byte
	TokPos int32
}

func (n *GenericDDLStmt) node()      {}
func (n *GenericDDLStmt) stmtNode()  {}
func (n *GenericDDLStmt) Pos() int32 { return n.TokPos }

// ObjectDDLStmt represents common CREATE/DROP object statements whose bodies
// are intentionally preserved rather than fully decomposed.
type ObjectDDLStmt struct {
	Verb        []byte
	Object      []byte
	Name        *Ident
	OrReplace   bool
	IfExists    bool
	IfNotExists bool
	Body        []byte
	TokPos      int32
}

func (n *ObjectDDLStmt) node()      {}
func (n *ObjectDDLStmt) stmtNode()  {}
func (n *ObjectDDLStmt) Pos() int32 { return n.TokPos }
