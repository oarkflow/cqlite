package db

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	catalog "github.com/oarkflow/sqlite/internal/catalog"
	"github.com/oarkflow/sqlite/internal/sqlparser"
	"github.com/oarkflow/sqlite/internal/sqlparser/ast"
)

// triggerSignal is a control-flow outcome raised by RAISE() inside a trigger.
type triggerSignal struct {
	action  string // ignore, rollback, abort, fail
	message string
}

func (t *triggerSignal) Error() string {
	if t.message == "" {
		return "trigger raised " + strings.ToUpper(t.action)
	}
	return t.message
}

func raiseSignal(action, message string) error {
	return &triggerSignal{action: action, message: message}
}

// activeTriggerRows holds the OLD/NEW bindings for the trigger currently
// firing. triggerScopeMu excludes unrelated statement evaluation while these
// bindings are installed.
var activeTriggerRows map[string]map[string]any

// triggerScopeMu protects activeTriggerRows. Ordinary statement evaluation
// takes a shared lock; statements that may fire triggers take the exclusive
// lock for the whole execution so OLD/NEW bindings cannot leak across DBs.
var triggerScopeMu sync.RWMutex

func lockTriggerScopeForStatement(s *persistedState, stmt ast.Statement) func() {
	if s != nil && len(s.Catalog.Triggers) > 0 {
		switch stmt.(type) {
		case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt:
			triggerScopeMu.Lock()
			return triggerScopeMu.Unlock
		}
	}
	triggerScopeMu.RLock()
	return triggerScopeMu.RUnlock
}

// triggerLookup resolves an identifier against the active trigger's OLD/NEW
// pseudo-rows. A missing binding reports not-ok so ordinary row resolution
// still applies.
func triggerLookup(qualifier, name string) (any, bool) {
	if activeTriggerRows == nil {
		return nil, false
	}
	q := catalog.Normalize(qualifier)
	if q != "old" && q != "new" {
		return nil, false
	}
	row, ok := activeTriggerRows[q]
	if !ok {
		return nil, false
	}
	v, ok := row[catalog.Normalize(name)]
	return v, ok
}

// withTriggerRows installs OLD/NEW bindings for the duration of one trigger
// program and returns a restore function.
func withTriggerRows(oldRow, newRow map[string]any) func() {
	prev := activeTriggerRows
	scope := make(map[string]map[string]any, 2)
	if oldRow != nil {
		scope["old"] = oldRow
	}
	if newRow != nil {
		scope["new"] = newRow
	}
	activeTriggerRows = scope
	return func() { activeTriggerRows = prev }
}

// execCreateTrigger persists a trigger definition.
func execCreateTrigger(s *persistedState, st *ast.CreateTriggerStmt, rawSQL string) (Result, error) {
	if st == nil || st.Name == nil || st.Table == nil {
		return Result{}, errors.New("CREATE TRIGGER requires a name and a table")
	}
	name := catalog.Normalize(st.Name.Unquoted)
	if _, exists := s.Catalog.TriggerByName(name); exists {
		if !st.OrReplace && !st.IfNotExists {
			return Result{}, fmt.Errorf("trigger already exists: %s", name)
		}
		if st.IfNotExists {
			return Result{}, nil
		}
	} else if st.IfNotExists {
		if s.Catalog.Triggers == nil {
			s.Catalog.Triggers = map[string]*catalog.Trigger{}
		}
	}
	tableName := catalog.Normalize(st.Table.Unquoted)
	if _, ok := s.Catalog.Table(tableName); !ok {
		if _, ok := s.Catalog.View(tableName); !ok {
			return Result{}, fmt.Errorf("no such table: %s", tableName)
		}
	}
	trig := &catalog.Trigger{
		Name:       name,
		Table:      tableName,
		Timing:     triggerTimingName(st.Timing),
		Event:      triggerEventName(st.Event),
		ForEachRow: st.ForEachRow,
		Temporary:  st.Temporary,
		SQL:        rawSQL,
	}
	for _, col := range st.UpdateOf {
		trig.UpdateOf = append(trig.UpdateOf, catalog.Normalize(col.Unquoted))
	}
	if st.When != nil {
		trig.WhenSQL = strings.TrimSpace(string(st.WhenRaw))
	}
	trig.BodySQL = strings.TrimSpace(string(st.BodyRaw))
	// Validate the body now so a bad trigger fails at creation time.
	if _, err := parseTriggerBody(trig.BodySQL); err != nil {
		return Result{}, err
	}
	if s.Catalog.Triggers == nil {
		s.Catalog.Triggers = map[string]*catalog.Trigger{}
	}
	s.Catalog.Triggers[name] = trig
	s.Pragmas.SchemaVer++
	return Result{}, nil
}

func triggerTimingName(t ast.TriggerTiming) string {
	switch t {
	case ast.TriggerBefore:
		return "before"
	case ast.TriggerAfter:
		return "after"
	case ast.TriggerInsteadOf:
		return "instead_of"
	}
	return ""
}

func triggerEventName(e ast.TriggerEvent) string {
	switch e {
	case ast.TriggerInsert:
		return "insert"
	case ast.TriggerDelete:
		return "delete"
	case ast.TriggerUpdate:
		return "update"
	}
	return ""
}

// execDropTrigger removes a trigger definition.
func execDropTrigger(s *persistedState, st *ast.DropTriggerStmt) (Result, error) {
	if st == nil || st.Name == nil {
		return Result{}, errors.New("DROP TRIGGER requires a name")
	}
	name := catalog.Normalize(st.Name.Unquoted)
	if _, ok := s.Catalog.TriggerByName(name); !ok {
		if st.IfExists {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("no such trigger: %s", name)
	}
	delete(s.Catalog.Triggers, name)
	s.Pragmas.SchemaVer++
	return Result{}, nil
}

// parseTriggerBody splits a trigger body into statements. The returned list
// owns its parse arena: trigger steps execute further SQL, and those nested
// parses must not clobber the body's AST.
func parseTriggerBody(body string) (*sqlparser.OwnedStatements, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("trigger body is empty")
	}
	body = strings.TrimSuffix(body, ";")
	owned, err := sqlparser.ParseStatementsOwned(body + ";")
	if err != nil {
		return nil, fmt.Errorf("invalid trigger body: %w", err)
	}
	return owned, nil
}

// triggerContext carries a row change into trigger evaluation.
type triggerContext struct {
	oldRow map[string]any
	newRow map[string]any
}

// fireTriggers runs the triggers attached to a table for one row change.
// before=true runs BEFORE triggers, before=false runs AFTER/INSTEAD OF ones.
// The returned bool reports whether the row change should proceed.
func fireTriggers(s *persistedState, table, event string, before bool, updateOf []string, ctx triggerContext) (proceed bool, err error) {
	proceed = true
	trigs := s.Catalog.TriggersFor(table, event)
	if len(trigs) == 0 {
		return true, nil
	}
	for _, tr := range trigs {
		isBefore := tr.Timing == "before"
		if before != isBefore {
			continue
		}
		if event == "update" && len(tr.UpdateOf) > 0 && !updateOfTouches(tr.UpdateOf, updateOf) {
			continue
		}
		ok, err := runOneTrigger(s, tr, ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			// RAISE(IGNORE) in a BEFORE trigger cancels the row change.
			return false, nil
		}
	}
	return true, nil
}

func updateOfTouches(triggerCols, changedCols []string) bool {
	if len(changedCols) == 0 {
		// The caller could not determine the changed columns; fire conservatively.
		return true
	}
	changed := make(map[string]struct{}, len(changedCols))
	for _, c := range changedCols {
		changed[catalog.Normalize(c)] = struct{}{}
	}
	for _, c := range triggerCols {
		if _, ok := changed[catalog.Normalize(c)]; ok {
			return true
		}
	}
	return false
}

// runOneTrigger evaluates a WHEN clause and, if it passes, runs the body.
// It returns false when the body raised RAISE(IGNORE).
func runOneTrigger(s *persistedState, tr *catalog.Trigger, ctx triggerContext) (bool, error) {
	if tr.WhenSQL != "" {
		restore := withTriggerRows(ctx.oldRow, ctx.newRow)
		pass, err := evalTriggerWhen(s, tr.WhenSQL, ctx)
		restore()
		if err != nil {
			var sig *triggerSignal
			if errors.As(err, &sig) {
				return false, err
			}
			return false, err
		}
		if !pass {
			return true, nil
		}
	}
	owned, err := parseTriggerBody(tr.BodySQL)
	if err != nil {
		return false, err
	}
	restore := withTriggerRows(ctx.oldRow, ctx.newRow)
	defer restore()
	for _, stmt := range owned.Stmts() {
		if err := execTriggerStep(s, stmt); err != nil {
			var sig *triggerSignal
			if errors.As(err, &sig) {
				if sig.action == "ignore" {
					return false, nil
				}
				return false, err
			}
			return false, err
		}
	}
	return true, nil
}

// evalTriggerWhen evaluates a trigger WHEN clause.
func evalTriggerWhen(s *persistedState, whenSQL string, _ triggerContext) (bool, error) {
	expr, err := sqlparser.ParseExpression(whenSQL)
	if err != nil {
		return false, fmt.Errorf("invalid trigger WHEN clause: %w", err)
	}
	b := binder{}
	v, err := evalExprState(s, expr, map[string]any{}, &b)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

// execTriggerStep runs one statement of a trigger program.
func execTriggerStep(s *persistedState, stmt ast.Statement) error {
	switch st := stmt.(type) {
	case *ast.RaiseTriggerStmt:
		action := strings.ToLower(string(st.Action))
		msg := ""
		if len(st.Message) > 0 {
			msg = unquoteSQL(string(st.Message))
		}
		switch action {
		case "ignore", "rollback", "abort", "fail":
			return raiseSignal(action, msg)
		default:
			return fmt.Errorf("unknown RAISE action: %s", action)
		}
	case *ast.SelectStmt:
		_, err := execSelect(s, st, nil)
		return err
	case *ast.InsertStmt:
		_, err := execInsert(s, st, nil)
		return err
	case *ast.UpdateStmt:
		_, err := execUpdate(s, st, nil)
		return err
	case *ast.DeleteStmt:
		_, err := execDelete(s, st, nil)
		return err
	default:
		return fmt.Errorf("statement not allowed in a trigger body: %T", stmt)
	}
}

// dmlStatementTouchesTriggers reports whether a DML statement targets a table
// that has triggers for its event. Fast paths must bail out for such tables so
// that no change is applied without firing the trigger program.
func dmlStatementTouchesTriggers(s *persistedState, stmt ast.Statement) bool {
	switch st := stmt.(type) {
	case *ast.InsertStmt:
		return tableHasTriggers(s, qname(st.Table), "insert")
	case *ast.UpdateStmt:
		name, err := tableNameFromRefs(st.Tables)
		if err != nil {
			return false
		}
		return tableHasTriggers(s, name, "update")
	case *ast.DeleteStmt:
		name, err := tableNameFromRefs(st.From)
		if err != nil {
			return false
		}
		return tableHasTriggers(s, name, "delete")
	}
	return false
}

// insteadOfTrigger reports whether a name resolves to a view carrying an
// INSTEAD OF trigger for an event. Such views swallow the base mutation and
// run the trigger program instead.
func insteadOfTrigger(s *persistedState, name, event string) bool {
	if s == nil || s.Catalog.Triggers == nil {
		return false
	}
	if _, isView := s.Catalog.View(name); !isView {
		return false
	}
	for _, tr := range s.Catalog.TriggersFor(name, event) {
		if tr.Timing == "instead_of" {
			return true
		}
	}
	return false
}

// fireInsteadOf runs the INSTEAD OF triggers for a view row change and returns
// the number of rows the statement should report as affected.
func fireInsteadOf(s *persistedState, name, event string, oldRow, newRow map[string]any) (int64, error) {
	trigs := s.Catalog.TriggersFor(name, event)
	if len(trigs) == 0 {
		return 0, fmt.Errorf("no INSTEAD OF %s trigger on view %s", event, name)
	}
	_, err := fireTriggers(s, name, event, false, nil, rowTriggerContext(oldRow, newRow))
	if err != nil {
		var sig *triggerSignal
		if errors.As(err, &sig) {
			return 0, errors.New(sig.Error())
		}
		return 0, err
	}
	return 1, nil
}

// tableHasTriggers reports whether any trigger is attached to a table for an
// event. Fast mutation paths consult this so they never bypass trigger firing.
func tableHasTriggers(s *persistedState, table, event string) bool {
	if s == nil || s.Catalog.Triggers == nil {
		return false
	}
	return len(s.Catalog.TriggersFor(table, event)) > 0
}

// changedColumns returns the normalized column names a SET clause assigns.
func changedColumns(assigns []ast.Assignment) []string {
	out := make([]string, 0, len(assigns))
	for _, asn := range assigns {
		if asn.Column == nil {
			continue
		}
		out = append(out, catalog.Normalize(asn.Column.Unquoted))
	}
	return out
}

// beforeRowChange runs BEFORE triggers for one pending row change. It returns
// false when RAISE(IGNORE) cancels the change.
func beforeRowChange(s *persistedState, table, event string, updateOf []string, oldRow, newRow map[string]any) (bool, error) {
	if !tableHasTriggers(s, table, event) {
		return true, nil
	}
	proceed, err := fireTriggers(s, table, event, true, updateOf, rowTriggerContext(oldRow, newRow))
	if err != nil {
		var sig *triggerSignal
		if errors.As(err, &sig) && sig.action == "ignore" {
			return false, nil
		}
		return false, err
	}
	return proceed, nil
}

// afterRowChange runs AFTER/INSTEAD OF triggers for one completed row change.
func afterRowChange(s *persistedState, table, event string, updateOf []string, oldRow, newRow map[string]any) error {
	if !tableHasTriggers(s, table, event) {
		return nil
	}
	_, err := fireTriggers(s, table, event, false, updateOf, rowTriggerContext(oldRow, newRow))
	if err != nil {
		var sig *triggerSignal
		if errors.As(err, &sig) {
			return errors.New(sig.Error())
		}
		return err
	}
	return nil
}

// rowTriggerContext builds trigger bindings from a row change.
func rowTriggerContext(oldRow, newRow map[string]any) triggerContext {
	ctx := triggerContext{}
	if oldRow != nil {
		ctx.oldRow = copyTriggerRow(oldRow)
	}
	if newRow != nil {
		ctx.newRow = copyTriggerRow(newRow)
	}
	return ctx
}

func copyTriggerRow(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}
