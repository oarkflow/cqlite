package ast

// AttachStmt is ATTACH [DATABASE] expr AS schema-name.
type AttachStmt struct {
	// File is the expression naming the database file to open.
	File Expr
	Name *Ident
	TokPos int32
}

func (n *AttachStmt) node()      {}
func (n *AttachStmt) stmtNode()  {}
func (n *AttachStmt) Pos() int32 { return n.TokPos }

// DetachStmt is DETACH [DATABASE] schema-name.
type DetachStmt struct {
	Name   *Ident
	TokPos int32
}

func (n *DetachStmt) node()      {}
func (n *DetachStmt) stmtNode()  {}
func (n *DetachStmt) Pos() int32 { return n.TokPos }
