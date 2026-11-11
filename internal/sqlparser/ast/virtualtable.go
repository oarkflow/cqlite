package ast

// CreateVirtualTableStmt is CREATE VIRTUAL TABLE ... USING module(args).
type CreateVirtualTableStmt struct {
	OrReplace   bool
	Temporary   bool
	IfNotExists bool
	// Name is the possibly schema-qualified table name.
	Name *QualifiedIdent
	// Module is the virtual table module, e.g. "fts5".
	Module *Ident
	// Args are the raw module arguments as written, without the parentheses.
	Args    []byte
	ArgsRaw []byte
	TokPos  int32
}

func (n *CreateVirtualTableStmt) node()      {}
func (n *CreateVirtualTableStmt) stmtNode()  {}
func (n *CreateVirtualTableStmt) Pos() int32 { return n.TokPos }
