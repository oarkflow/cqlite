package ast

// TriggerTiming selects when a trigger fires relative to its statement.
type TriggerTiming uint8

const (
	TriggerTimingDefault TriggerTiming = iota
	TriggerBefore
	TriggerAfter
	TriggerInsteadOf
)

// TriggerEvent selects which DML statement fires a trigger.
type TriggerEvent uint8

const (
	TriggerEventNone TriggerEvent = iota
	TriggerInsert
	TriggerDelete
	TriggerUpdate
)

// CreateTriggerStmt is a CREATE TRIGGER statement with a structured body.
// The body is a real statement list rather than raw text, so multi-statement
// bodies survive the trip through the parser.
type CreateTriggerStmt struct {
	OrReplace   bool
	Temporary   bool
	IfNotExists bool
	Name        *Ident
	Table       *Ident
	Timing      TriggerTiming
	Event       TriggerEvent
	// UpdateOf lists the columns for UPDATE OF col, ... triggers. It is empty
	// for a plain UPDATE trigger.
	UpdateOf []*Ident
	// ForEachRow is always true for SQLite row triggers; kept for fidelity.
	ForEachRow bool
	When       Expr
	WhenRaw    []byte
	Body       []Statement
	BodyRaw    []byte
	TokPos     int32
}

func (n *CreateTriggerStmt) node()         {}
func (n *CreateTriggerStmt) stmtNode()     {}
func (n *CreateTriggerStmt) Pos() int32    { return n.TokPos }
func (n *CreateTriggerStmt) stmt()         {}

// DropTriggerStmt is DROP TRIGGER [IF EXISTS] name.
type DropTriggerStmt struct {
	IfExists bool
	Name     *Ident
	TokPos   int32
}

func (n *DropTriggerStmt) node()      {}
func (n *DropTriggerStmt) stmtNode()  {}
func (n *DropTriggerStmt) Pos() int32 { return n.TokPos }
func (n *DropTriggerStmt) stmt()      {}

// RaiseTriggerStmt is RAISE(IGNORE) or RAISE(level, message), valid only in a
// trigger body.
type RaiseTriggerStmt struct {
	// Action is one of "ignore", "rollback", "abort", "fail".
	Action  []byte
	Message []byte
	TokPos  int32
}

func (n *RaiseTriggerStmt) node()      {}
func (n *RaiseTriggerStmt) stmtNode()  {}
func (n *RaiseTriggerStmt) Pos() int32 { return n.TokPos }
func (n *RaiseTriggerStmt) stmt()      {}
