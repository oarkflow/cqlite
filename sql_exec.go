package cqlite

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/oarkflow/cqlite/internal/catalog"
	isql "github.com/oarkflow/cqlite/internal/sql"
	"github.com/oarkflow/cqlite/internal/sqlparser"
	"github.com/oarkflow/cqlite/internal/sqlparser/ast"
	"github.com/oarkflow/cqlite/internal/sqlparser/lexer"
)

const primaryKeyNamePlaceholder = "__primary_key__"

var simpleDMLCache sync.Map
var simpleSelectCache sync.Map
var queryParserPool = sync.Pool{New: func() any { return sqlparser.NewString("") }}

// parseMu protects legacy callers that use the shared convenience parser.
// New execution paths lease parser arenas from queryParserPool.
var parseMu sync.Mutex

type simpleDMLPlan struct {
	kind   byte
	table  string
	col    string
	where  string
	inc    int64
	params int
}

type simpleSelectPlan struct {
	table  string
	cols   []string
	conds  []simpleSelectCond
	order  simpleSelectOrder
	limit  int
	offset int
}

type simpleSelectCond struct {
	col      string
	op       string
	param    int
	literal  any
	hasParam bool
}

type simpleSelectOrder struct {
	col  string
	desc bool
}

func parseStatementCached(query string) (ast.Statement, error) {
	query = normalizeInsertOrReplace(query)
	return sqlparser.ParseStatement(query)
}

// withQueryStatement gives this execution exclusive use of a parser arena and
// keeps the AST valid until run returns. Separate readers can parse and execute
// concurrently without sharing the convenience parser's global arena.
func withQueryStatement[T any](query string, run func(ast.Statement) (T, error)) (T, error) {
	p := queryParserPool.Get().(*sqlparser.Parser)
	p.ResetString(normalizeInsertOrReplace(query))
	defer queryParserPool.Put(p)
	stmt, err := p.Next()
	if err != nil {
		var zero T
		return zero, err
	}
	return run(stmt)
}

func execute(ctx context.Context, s *persistedState, query string, args ...any) (Result, error) {
	if res, ok, err := execPragma(s, query); ok || err != nil {
		return res, err
	}
	if res, ok, err := execMaintenance(s, query); ok || err != nil {
		return res, err
	}
	parseQuery, strictTable := stripSQLiteStrict(query)
	type execution struct {
		result Result
		err    error
	}
	parsed, parseErr := withQueryStatement(parseQuery, func(stmt ast.Statement) (execution, error) {
		unlockTriggerScope := lockTriggerScopeForStatement(s, stmt)
		defer unlockTriggerScope()
		select {
		case <-ctx.Done():
			return execution{err: ctx.Err()}, nil
		default:
		}
		var res Result
		var err error
		switch st := stmt.(type) {
		case *ast.CreateTableStmt:
			res, err = execCreateTable(s, st, strictTable, query)
		case *ast.CreateViewStmt:
			res, err = execCreateView(s, st, query)
		case *ast.DropTableStmt:
			res, err = execDropTable(s, st, query)
		case *ast.CreateIndexStmt:
			res, err = execCreateIndex(s, st, query)
		case *ast.DropIndexStmt:
			res, err = execDropIndex(s, st)
		case *ast.AlterTableStmt:
			res, err = execAlterTable(s, st)
		case *ast.CreateVirtualTableStmt:
			res, err = execCreateVirtualTableStmt(s, st, query)
		case *ast.CreateTriggerStmt:
			res, err = execCreateTrigger(s, st, query)
		case *ast.DropTriggerStmt:
			res, err = execDropTrigger(s, st)
		case *ast.InsertStmt:
			res, err = execInsert(s, st, args)
		case *ast.UpdateStmt:
			res, err = execUpdate(s, st, args)
		case *ast.DeleteStmt:
			res, err = execDelete(s, st, args)
		case *ast.ExplainStmt, *ast.TransactionStmt:
		default:
			err = fmt.Errorf("statement is not executable in v1: %T", stmt)
		}
		return execution{res, err}, nil
	})
	if parseErr != nil {
		if res, ok, manualErr := execManualCreateIndex(s, query); ok {
			return res, manualErr
		}
		if res, ok, manualErr := execManualDropView(s, query); ok {
			return res, manualErr
		}
		return Result{}, parseErr
	}
	return parsed.result, parsed.err
}

func fastDirectDML(s *persistedState, stmt ast.Statement, args []any) (Result, bool, error) {
	if hasNamedArgs(args) {
		return Result{}, false, nil
	}
	// A trigger can run through a foreign-key action on a different table, so
	// keep every DML statement on the trigger-aware executor when any trigger
	// is present in the schema.
	if s != nil && len(s.Catalog.Triggers) > 0 {
		return Result{}, false, nil
	}
	// Trigger-armed tables must go through the row-by-row path so BEFORE and
	// AFTER triggers fire for every change. Virtual tables are handled by
	// their module, never by the text-driven fast paths.
	if dmlStatementTouchesTriggers(s, stmt) {
		return Result{}, false, nil
	}
	if dmlStatementTouchesVirtualTable(s, stmt) {
		return Result{}, false, nil
	}
	if ins, ok := stmt.(*ast.InsertStmt); ok {
		return fastInsertDML(s, ins, args)
	}
	s.materializePending()
	upd, ok := stmt.(*ast.UpdateStmt)
	if !ok || len(upd.Set) != 1 {
		return Result{}, false, nil
	}
	name, err := tableNameFromRefs(upd.Tables)
	if err != nil {
		return Result{}, false, nil
	}
	t, ok := s.Catalog.Table(name)
	if !ok {
		return Result{}, true, fmt.Errorf("table does not exist: %s", name)
	}
	pk := integerPrimaryKeyColumn(t)
	if pk == "" {
		return Result{}, false, nil
	}
	where, ok := upd.Where.(*ast.BinaryExpr)
	if !ok || where.Op != lexer.EQ || exprColumnName(where.Left) != pk {
		return Result{}, false, nil
	}
	asn := upd.Set[0]
	setParams := countParams(asn.Value)
	idv, err := evalExpr(where.Right, nil, &binder{args: args, pos: setParams})
	if err != nil {
		return Result{}, true, err
	}
	id, ok := asInt(idv)
	if !ok {
		return Result{RowsAffected: 0}, true, nil
	}
	data := s.Tables[t.Name]
	if data.runtimeDeferred() {
		return Result{}, false, nil
	}
	pos, ok := data.lookupRowID(id)
	if !ok || data.Rows[pos].Deleted != 0 {
		return Result{RowsAffected: 0}, true, nil
	}
	col, _, ok := t.Column(asn.Column.Unquoted)
	if !ok {
		return Result{}, true, fmt.Errorf("unknown column: %s", asn.Column.Unquoted)
	}
	if !canFastUpdateColumn(s, t, col.Name) {
		return Result{}, false, nil
	}
	if countParams(asn.Value) > 0 {
		return Result{}, false, nil
	}
	rowValues := data.Rows[pos].Values
	if rowValues == nil {
		rowValues = positionedToValues(t, data.Rows[pos].positionedValues())
	}
	nextValue, ok, err := fastUpdateValue(asn.Value, rowValues)
	if err != nil || !ok {
		return Result{}, ok, err
	}
	cv, err := coerce(col, nextValue, t.Strict)
	if err != nil {
		return Result{}, true, err
	}
	data.copyRow(pos)
	if data.Rows[pos].Values == nil {
		data.Rows[pos].Values = positionedToValues(t, data.Rows[pos].positionedValues())
	}
	data.Rows[pos].Values[col.Name] = cv
	data.Rows[pos].syncPositioned(t)
	return Result{RowsAffected: 1}, true, nil
}

type rowChangeRecorder func(table string, data *tableData, pos int, columnIndex int, columnName string, oldValue any)

func fastSimpleDML(s *persistedState, query string, args []any) (Result, bool, error) {
	return fastSimpleDMLWithRecorder(s, query, args, nil)
}

func fastSimpleDMLWithRecorder(s *persistedState, query string, args []any, record rowChangeRecorder) (Result, bool, error) {
	plan, planOK := simplePlan(query)
	return fastSimpleDMLWithPlan(s, query, args, record, plan, planOK)
}

func fastSimpleDMLWithPlan(s *persistedState, query string, args []any, record rowChangeRecorder, plan simpleDMLPlan, planOK bool) (Result, bool, error) {
	// Virtual tables are routed to their module rather than the fast paths.
	if s != nil && planOK {
		if _, isVT := hasVirtualTables(s, plan.table); isVT {
			return Result{}, false, nil
		}
	}
	// Triggers can also fire indirectly through foreign-key actions on a
	// different table, so DML uses the general executor whenever triggers exist.
	if s != nil && len(s.Catalog.Triggers) > 0 {
		return Result{}, false, nil
	}
	if hasNamedArgs(args) {
		return Result{}, false, nil
	}
	if !planOK || plan.params != len(args) {
		return Result{}, false, nil
	}
	switch plan.kind {
	case 'i':
		return fastInsertValues(s, plan.table, args)
	case 'u':
		s.materializePending()
		return fastIncrementUpdate(s, simpleIncrementUpdate{
			table: plan.table,
			col:   plan.col,
			where: plan.where,
			inc:   plan.inc,
		}, args, record)
	case 'd':
		s.materializePending()
		return fastDeleteWhere(s, plan.table, plan.where, args, record)
	default:
		return Result{}, false, nil
	}
}

func simplePlan(query string) (simpleDMLPlan, bool) {
	if cached, ok := simpleDMLCache.Load(query); ok {
		plan := cached.(simpleDMLPlan)
		return plan, plan.kind != 0
	}
	var plan simpleDMLPlan
	if table, n, ok := parseSimpleInsertValues(query); ok {
		plan = simpleDMLPlan{kind: 'i', table: table, params: n}
	} else if upd, ok := parseSimpleIncrementUpdate(query); ok {
		plan = simpleDMLPlan{kind: 'u', table: upd.table, col: upd.col, where: upd.where, inc: upd.inc, params: 1}
	} else if table, where, ok := parseSimpleDeleteWhere(query); ok {
		plan = simpleDMLPlan{kind: 'd', table: table, where: where, params: 1}
	}
	actual, _ := simpleDMLCache.LoadOrStore(query, plan)
	plan = actual.(simpleDMLPlan)
	return plan, plan.kind != 0
}

func fastInsertValues(s *persistedState, table string, args []any) (Result, bool, error) {
	t, data, err := tableForWrite(s, table)
	if err != nil {
		return Result{}, true, err
	}
	if len(args) != len(t.Columns) {
		return Result{}, false, nil
	}
	if canFastPositionedInsert(s, t) {
		return fastInsertPositioned(s, t, data, args)
	}
	row := make(map[string]any, len(t.Columns))
	for i, col := range t.Columns {
		cv, err := coerce(col, args[i], t.Strict)
		if err != nil {
			return Result{}, true, err
		}
		row[col.Name] = cv
	}
	rowid := t.NextRowID
	if pk := integerPrimaryKeyColumn(t); pk != "" {
		if row[pk] == nil {
			row[pk] = rowid
		} else if id, ok := asInt(row[pk]); ok {
			rowid = id
		}
	}
	if err := validateInsertFast(s, t, data, row, rowid); err != nil {
		return Result{}, true, err
	}
	data.stageRow(t, newRowVersion(t, rowid, row, s.NextTx))
	if rowid >= t.NextRowID {
		t.NextRowID = rowid + 1
	}
	return Result{RowsAffected: 1, LastInsertID: rowid}, true, nil
}

func canFastPositionedInsert(s *persistedState, t *catalog.Table) bool {
	if s.Pragmas.ForeignKeys || len(t.ForeignKeys) > 0 || len(t.Checks) > 0 || len(t.Indexes) > 0 {
		return false
	}
	ipk := integerPrimaryKeyColumn(t)
	for _, col := range t.Columns {
		if col.Check != "" || col.References != nil {
			return false
		}
		if (col.PrimaryKey || col.Unique) && col.Name != ipk {
			return false
		}
	}
	return ipk != ""
}

func fastInsertPositioned(s *persistedState, t *catalog.Table, data *tableData, args []any) (Result, bool, error) {
	ncols := len(t.Columns)
	// The row retains Pos after this function returns. Carve it from a table-local
	// arena so small rows do not allocate a separate slice for every insert.
	pos := data.positionArena.alloc(ncols)
	rowid := t.NextRowID
	pkName := integerPrimaryKeyColumn(t)
	pkIndex := -1
	for i, col := range t.Columns {
		cv, err := coerce(col, args[i], t.Strict)
		if err != nil {
			clear(pos)
			return Result{}, true, err
		}
		if col.NotNull && cv == nil {
			clear(pos)
			return Result{}, true, fmt.Errorf("column %s cannot be null", col.Name)
		}
		pos[i] = cv
		if col.Name == pkName {
			pkIndex = i
		}
	}
	if pkIndex >= 0 {
		if pos[pkIndex] == nil {
			pos[pkIndex] = rowid
		} else if id, ok := asInt(pos[pkIndex]); ok {
			rowid = id
		}
	}
	if err := validateIntegerPrimaryKeyFast(t, data, rowid, pkName); err != nil {
		clear(pos)
		return Result{}, true, err
	}
	data.stageRow(t, newPositionedRowVersion(rowid, pos, s.NextTx))
	if rowid >= t.NextRowID {
		t.NextRowID = rowid + 1
	}
	return Result{RowsAffected: 1, LastInsertID: rowid}, true, nil
}

func validateIntegerPrimaryKeyFast(t *catalog.Table, data *tableData, rowid int64, pkName string) error {
	if data.rowids == nil {
		data.rebuildRuntime(t)
	}
	if pos, ok := data.lookupRowID(rowid); ok && pos >= 0 && pos < len(data.Rows) && data.Rows[pos].Deleted == 0 {
		return fmt.Errorf("unique constraint failed: %s.%s", t.Name, pkName)
	}
	if data.pendingRowIDConflict(rowid) {
		return fmt.Errorf("unique constraint failed: %s.%s", t.Name, pkName)
	}
	return nil
}

type simpleIncrementUpdate struct {
	table string
	col   string
	where string
	inc   int64
}

func fastIncrementUpdate(s *persistedState, upd simpleIncrementUpdate, args []any, record rowChangeRecorder) (Result, bool, error) {
	if len(args) != 1 {
		return Result{}, false, nil
	}
	t, ok := s.Catalog.Table(upd.table)
	if !ok {
		return Result{}, true, fmt.Errorf("table does not exist: %s", upd.table)
	}
	pk := integerPrimaryKeyColumn(t)
	if pk != upd.where && usableIndexForColumn(s, upd.table, upd.where) == "" {
		return Result{}, false, nil
	}
	col, colIdx, ok := t.Column(upd.col)
	if !ok {
		return Result{}, true, fmt.Errorf("unknown column: %s", upd.col)
	}
	if !canFastUpdateColumn(s, t, col.Name) {
		return Result{}, false, nil
	}
	data := s.Tables[t.Name]
	if data == nil {
		return Result{RowsAffected: 0}, true, nil
	}
	if data.runtimeDeferred() {
		return Result{}, false, nil
	}
	var affected int64
	apply := func(pos int) (bool, error) {
		if pos < 0 || pos >= len(data.Rows) {
			return true, nil
		}
		row := &data.Rows[pos]
		if row.Deleted != 0 {
			return true, nil
		}
		cur, ok := asInt(row.value(t, col.Name))
		if !ok {
			return false, nil
		}
		cv, err := coerce(col, cur+upd.inc, t.Strict)
		if err != nil {
			return false, err
		}
		if record != nil {
			record(t.Name, data, pos, colIdx, col.Name, row.value(t, col.Name))
		}
		data.copyRowForColumnUpdate(pos, colIdx, col.Name, cv)
		affected++
		return true, nil
	}
	if pk != "" && upd.where == pk {
		id, ok := asInt(args[0])
		if !ok {
			return Result{RowsAffected: 0}, true, nil
		}
		pos, ok := data.lookupRowID(id)
		if !ok {
			return Result{RowsAffected: 0}, true, nil
		}
		keepGoing, err := apply(pos)
		if err != nil {
			return Result{}, true, err
		}
		if !keepGoing {
			return Result{}, false, nil
		}
	} else if idxName := usableIndexForColumn(s, upd.table, upd.where); idxName != "" {
		idx := catalog.Index{Name: idxName, Columns: []string{upd.where}}
		var fallback bool
		var applyErr error
		key := runtimeIndexLookupKeyBytes(idx, args[0])
		data.eachIndexPositionBytes(idxName, key, func(pos int) bool {
			var keepGoing bool
			keepGoing, applyErr = apply(pos)
			if applyErr != nil {
				fallback = false
				return false
			}
			if !keepGoing {
				fallback = true
				return false
			}
			return true
		})
		if applyErr != nil {
			return Result{}, true, applyErr
		}
		if fallback {
			return Result{}, false, nil
		}
	} else {
		return Result{}, false, nil
	}
	return Result{RowsAffected: affected}, true, nil
}

func fastDeleteWhere(s *persistedState, table, where string, args []any, record rowChangeRecorder) (Result, bool, error) {
	if len(args) != 1 {
		return Result{}, false, nil
	}
	t, ok := s.Catalog.Table(table)
	if !ok {
		return Result{}, true, fmt.Errorf("table does not exist: %s", table)
	}
	data := s.Tables[t.Name]
	if data == nil {
		return Result{RowsAffected: 0}, true, nil
	}
	if data.runtimeDeferred() {
		return Result{}, false, nil
	}
	var affected int64
	apply := func(pos int) (bool, error) {
		if pos < 0 || pos >= len(data.Rows) {
			return true, nil
		}
		row := &data.Rows[pos]
		if row.Deleted != 0 {
			return true, nil
		}
		if s.Pragmas.ForeignKeys {
			row.ensureValues(t)
			if err := validateDeleteReferences(s, table, row.Values); err != nil {
				return false, err
			}
		}
		if record != nil {
			record(t.Name, data, pos, -1, "", nil)
		}
		data.markRowDeleted(pos, s.NextTx)
		affected++
		return true, nil
	}
	if pk := integerPrimaryKeyColumn(t); pk == where || where == "rowid" || where == "_rowid_" || where == "oid" {
		id, ok := asInt(args[0])
		if !ok {
			return Result{RowsAffected: 0}, true, nil
		}
		pos, ok := data.lookupRowID(id)
		if !ok {
			return Result{RowsAffected: 0}, true, nil
		}
		if _, err := apply(pos); err != nil {
			return Result{}, true, err
		}
	} else if idxName := usableIndexForColumn(s, table, where); idxName != "" {
		idx := catalog.Index{Name: idxName, Columns: []string{where}}
		var applyErr error
		key := runtimeIndexLookupKeyBytes(idx, args[0])
		data.eachIndexPositionBytes(idxName, key, func(pos int) bool {
			_, applyErr = apply(pos)
			return applyErr == nil
		})
		if applyErr != nil {
			return Result{}, true, applyErr
		}
	} else {
		return Result{}, false, nil
	}
	return Result{RowsAffected: affected}, true, nil
}

func fastInsertDML(s *persistedState, st *ast.InsertStmt, args []any) (Result, bool, error) {
	if st.Replace || st.OnConflictDoNothing || st.Ignore || len(st.OnConflictUpdate) > 0 || len(st.Values) != 1 {
		return Result{}, false, nil
	}
	if st.ValuesExpr != nil {
		return Result{}, false, nil
	}
	t, data, err := tableForWrite(s, qname(st.Table))
	if err != nil {
		return Result{}, true, err
	}
	cols := make([]catalog.Column, 0, len(st.Columns))
	if len(st.Columns) == 0 {
		cols = append(cols, t.Columns...)
	} else {
		for _, id := range st.Columns {
			col, _, ok := t.Column(id.Unquoted)
			if !ok {
				return Result{}, true, fmt.Errorf("unknown column: %s", id.Unquoted)
			}
			cols = append(cols, col)
		}
	}
	values := st.Values[0]
	if len(values) != len(cols) {
		return Result{}, true, errors.New("insert column/value count mismatch")
	}
	if len(values) != len(args) {
		return Result{}, false, nil
	}
	for _, expr := range values {
		if _, ok := expr.(*ast.Param); !ok {
			return Result{}, false, nil
		}
	}
	row := make(map[string]any, len(t.Columns))
	for _, col := range t.Columns {
		row[col.Name] = col.Default
	}
	for i, col := range cols {
		cv, err := coerce(col, args[i], t.Strict)
		if err != nil {
			return Result{}, true, err
		}
		row[col.Name] = cv
	}
	rowid := t.NextRowID
	if pk := integerPrimaryKeyColumn(t); pk != "" {
		if row[pk] == nil {
			row[pk] = rowid
		} else if id, ok := asInt(row[pk]); ok {
			rowid = id
		}
	}
	if err := validateInsertFast(s, t, data, row, rowid); err != nil {
		return Result{}, true, err
	}
	data.stageRow(t, newRowVersion(t, rowid, row, s.NextTx))
	if rowid >= t.NextRowID {
		t.NextRowID = rowid + 1
	}
	return Result{RowsAffected: 1, LastInsertID: rowid}, true, nil
}

func validateInsertFast(s *persistedState, t *catalog.Table, data *tableData, values map[string]any, rowid int64) error {
	for _, col := range t.Columns {
		v := values[col.Name]
		if col.NotNull && v == nil {
			return fmt.Errorf("column %s cannot be null", col.Name)
		}
		if col.Unique || isSingleColumnPrimaryKey(t, col.Name) {
			if col.PrimaryKey && col.Name == integerPrimaryKeyColumn(t) {
				if data.rowids == nil {
					data.rebuildRuntime(t)
				}
				if pos, ok := data.lookupRowID(rowid); ok && pos >= 0 && pos < len(data.Rows) && data.Rows[pos].Deleted == 0 {
					return fmt.Errorf("unique constraint failed: %s.%s", t.Name, col.Name)
				}
				for i := data.baseRows; data.deferRuntime && i < len(data.Rows); i++ {
					row := data.Rows[i]
					if row.Deleted == 0 && row.RowID == rowid {
						return fmt.Errorf("unique constraint failed: %s.%s", t.Name, col.Name)
					}
				}
				if data.pendingRowIDConflict(rowid) {
					return fmt.Errorf("unique constraint failed: %s.%s", t.Name, col.Name)
				}
				continue
			}
			return validateRow(s, t, data, values, 0)
		}
	}
	for _, idx := range t.Indexes {
		if !idx.Unique {
			continue
		}
		if idx.Partial || len(idx.Columns) == 0 {
			return validateRow(s, t, data, values, 0)
		}
		if data.indexes == nil {
			data.rebuildRuntime(t)
		}
		key := indexRuntimeKey(idx, values)
		conflict := false
		data.eachIndexPosition(idx.Name, key, func(pos int) bool {
			if pos >= 0 && pos < len(data.Rows) && data.Rows[pos].Deleted == 0 {
				conflict = true
				return false
			}
			return true
		})
		for i := data.baseRows; data.deferRuntime && !conflict && i < len(data.Rows); i++ {
			row := data.Rows[i]
			if row.Deleted != 0 {
				continue
			}
			same := true
			for _, col := range idx.Columns {
				same = same && valuesEqual(row.Values[col], values[col], columnCollation(t, col))
			}
			conflict = same
		}
		for _, row := range data.Pending {
			if conflict || row.Deleted != 0 {
				continue
			}
			same := true
			for _, col := range idx.Columns {
				same = same && valuesEqual(row.Values[col], values[col], columnCollation(t, col))
			}
			conflict = same
		}
		if conflict {
			return fmt.Errorf("unique constraint failed: %s.%s", t.Name, idx.Name)
		}
	}
	if err := validateChecks(t, values); err != nil {
		return err
	}
	if s.Pragmas.ForeignKeys {
		for _, fk := range t.ForeignKeys {
			if err := validateForeignKey(s, fk, values); err != nil {
				return err
			}
		}
	}
	return nil
}

func canFastUpdateColumn(s *persistedState, t *catalog.Table, col string) bool {
	if len(t.Checks) > 0 {
		return false
	}
	c, _, ok := t.Column(col)
	if !ok || c.PrimaryKey || c.Unique || c.NotNull || c.References != nil {
		return false
	}
	for _, idx := range t.Indexes {
		for _, idxCol := range idx.Columns {
			if idxCol == col {
				return false
			}
		}
	}
	if s.Pragmas.ForeignKeys {
		for _, fk := range t.ForeignKeys {
			for _, fkCol := range fk.Columns {
				if fkCol == col {
					return false
				}
			}
		}
		for _, child := range s.Catalog.Tables {
			for _, fk := range child.ForeignKeys {
				if fk.RefTable != t.Name {
					continue
				}
				for _, refCol := range resolveFKColumns(t, fk.RefColumns) {
					if refCol == col {
						return false
					}
				}
			}
		}
	}
	return true
}

func fastUpdateValue(expr ast.Expr, row map[string]any) (any, bool, error) {
	bin, ok := expr.(*ast.BinaryExpr)
	if !ok {
		return nil, false, nil
	}
	col := exprColumnName(bin.Left)
	if col == "" {
		return nil, false, nil
	}
	right, err := evalExpr(bin.Right, row, &binder{})
	if err != nil {
		return nil, true, err
	}
	v, err := evalBinary(row[col], right, bin.Op)
	return v, true, err
}

func parseSimpleInsertValues(query string) (string, int, bool) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	lower := strings.ToLower(q)
	const prefix = "insert into "
	if !strings.HasPrefix(lower, prefix) {
		return "", 0, false
	}
	rest := strings.TrimSpace(q[len(prefix):])
	lowerRest := strings.ToLower(rest)
	idx := strings.Index(lowerRest, " values")
	if idx <= 0 {
		return "", 0, false
	}
	table := catalog.Normalize(strings.TrimSpace(rest[:idx]))
	if !isBareIdentifier(table) {
		return "", 0, false
	}
	valuesPart := strings.TrimSpace(rest[idx+len(" values"):])
	if !strings.HasPrefix(valuesPart, "(") || !strings.HasSuffix(valuesPart, ")") {
		return "", 0, false
	}
	body := strings.TrimSpace(valuesPart[1 : len(valuesPart)-1])
	if body == "" {
		return "", 0, false
	}
	parts := strings.Split(body, ",")
	for _, part := range parts {
		if strings.TrimSpace(part) != "?" {
			return "", 0, false
		}
	}
	return table, len(parts), true
}

func parseSimpleIncrementUpdate(query string) (simpleIncrementUpdate, bool) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	fields := strings.Fields(q)
	if len(fields) != 12 || !strings.EqualFold(fields[0], "update") || !strings.EqualFold(fields[2], "set") || !strings.EqualFold(fields[8], "where") {
		return simpleIncrementUpdate{}, false
	}
	if fields[4] != "=" || fields[6] != "+" || fields[10] != "=" || fields[11] != "?" {
		return simpleIncrementUpdate{}, false
	}
	table := catalog.Normalize(fields[1])
	col := catalog.Normalize(fields[3])
	left := catalog.Normalize(fields[5])
	where := catalog.Normalize(fields[9])
	if !isBareIdentifier(table) || !isBareIdentifier(col) || !isBareIdentifier(left) || !isBareIdentifier(where) || col != left {
		return simpleIncrementUpdate{}, false
	}
	inc, err := strconv.ParseInt(fields[7], 10, 64)
	if err != nil {
		return simpleIncrementUpdate{}, false
	}
	return simpleIncrementUpdate{table: table, col: col, where: where, inc: inc}, true
}

func parseSimpleDeleteWhere(query string) (string, string, bool) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	fields := strings.Fields(q)
	if len(fields) != 7 || !strings.EqualFold(fields[0], "delete") || !strings.EqualFold(fields[1], "from") || !strings.EqualFold(fields[3], "where") {
		return "", "", false
	}
	if fields[5] != "=" || fields[6] != "?" {
		return "", "", false
	}
	table := catalog.Normalize(fields[2])
	where := catalog.Normalize(fields[4])
	if !isBareIdentifier(table) || !isBareIdentifier(where) {
		return "", "", false
	}
	return table, where, true
}

func execSimpleSelectPlan(s *persistedState, query string, args []any) (*Rows, bool, error) {
	plan, ok := parseSimpleSelectPlanCached(query)
	if !ok {
		return nil, false, nil
	}
	if _, isVT := hasVirtualTables(s, plan.table); isVT {
		return nil, false, nil
	}
	t, ok := s.Catalog.Table(plan.table)
	if !ok {
		if _, viewOK := s.Catalog.View(plan.table); viewOK {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("table does not exist: %s", plan.table)
	}
	data := s.Tables[catalog.Normalize(plan.table)]
	if data == nil {
		return isql.NewRows(plan.cols, nil), true, nil
	}
	lazyData := data.lazyPath != "" || len(data.lazyRecords) > 0
	if data.deferRuntime && !lazyData {
		return nil, false, nil
	}
	var projectorStack [8]int
	projectors, ok := simpleSelectProjectors(t, plan.cols, projectorStack[:0])
	if !ok {
		return nil, false, nil
	}
	cols := append([]string(nil), plan.cols...)
	if plan.order.col != "" && columnCollation(t, plan.order.col) != "BINARY" {
		return nil, false, nil
	}
	if plan.order.col != "" && len(plan.conds) > 0 && !simpleSelectOrderCompatible(plan, args) {
		return nil, false, nil
	}
	width := len(projectors)
	if row, used, err := simpleSelectLazyPointRow(t, data, plan, args); used || err != nil {
		if err != nil {
			return nil, true, err
		}
		if row.Deleted != 0 || !simpleSelectMatches(t, plan, row, args) {
			return isql.NewFlatRows(cols, nil), true, nil
		}
		out := make([]any, 0, width)
		out = appendSimpleSelectProjection(out, row, projectors, plan.cols)
		return isql.NewFlatRows(cols, out), true, nil
	}
	// Other lazy reads require the full runtime indexes; the caller falls
	// through to the generic path, which materialises them before execution.
	if lazyData {
		return nil, false, nil
	}
	if len(data.lazyRecords) > 0 {
		data.rebuildRuntime(t)
	}
	out := make([]any, 0, simpleSelectStreamCap(t, plan, data, args)*width)
	emit := func(row rowVersion) {
		out = appendSimpleSelectProjection(out, row, projectors, plan.cols)
	}
	postFilter := !simpleSelectAccessCoversConditions(plan, args)
	if plan.order.col == "" {
		if used := simpleSelectEachCandidatePosition(s, plan, data, args, func(pos int) bool {
			if pos < 0 || pos >= len(data.Rows) {
				return true
			}
			row := data.Rows[pos]
			if row.Deleted != 0 || (postFilter && !simpleSelectMatches(t, plan, row, args)) {
				return true
			}
			emit(row)
			return true
		}); used {
			if plan.limit >= 0 {
				out = applySimpleFlatLimit(out, width, plan.limit, plan.offset)
			}
			return isql.NewFlatRows(cols, out), true, nil
		}
	}
	positions := simpleSelectCandidatePositions(s, plan, data, args)
	if positions == nil && plan.order.col != "" {
		positions = simpleSelectOrderPositions(s, plan, data)
	}
	if positions != nil && plan.order.desc {
		reverseInts(positions)
	}
	capHint := len(data.Rows)
	if positions != nil {
		capHint = len(positions)
	}
	out = make([]any, 0, capHint*width)
	if positions != nil {
		for _, pos := range positions {
			if pos < 0 || pos >= len(data.Rows) {
				continue
			}
			row := data.Rows[pos]
			if row.Deleted != 0 || !simpleSelectMatches(t, plan, row, args) {
				continue
			}
			emit(row)
		}
	} else if plan.order.col != "" {
		return nil, false, nil
	} else {
		for _, row := range data.Rows {
			if row.Deleted != 0 || !simpleSelectMatches(t, plan, row, args) {
				continue
			}
			emit(row)
		}
	}
	if plan.limit >= 0 {
		out = applySimpleFlatLimit(out, width, plan.limit, plan.offset)
	}
	return isql.NewFlatRows(cols, out), true, nil
}

func canQueryDeferredSimplePoint(s *persistedState, query string, args []any) bool {
	if s == nil {
		return false
	}
	plan, ok := parseSimpleSelectPlanCached(query)
	if !ok || plan.order.col != "" || len(plan.conds) != 1 || plan.conds[0].op != "=" {
		return false
	}
	table, ok := s.Catalog.Table(plan.table)
	if !ok {
		return false
	}
	data := s.Tables[catalog.Normalize(plan.table)]
	if data == nil || (len(data.lazyRecords) == 0 && data.lazyPath == "") {
		return false
	}
	pk := integerPrimaryKeyColumn(table)
	cond := plan.conds[0]
	if cond.col != pk && cond.col != "rowid" && cond.col != "_rowid_" && cond.col != "oid" {
		column, _, ok := table.Column(cond.col)
		if !ok || !column.PrimaryKey {
			return false
		}
	}
	if !simpleSelectColumnsValid(table, plan.cols) {
		return false
	}
	if plan.order.col != "" && columnCollation(table, plan.order.col) != "BINARY" {
		return false
	}
	if plan.order.col != "" && !simpleSelectOrderCompatible(plan, args) {
		return false
	}
	return true
}

func simpleSelectLazyPointRow(t *catalog.Table, data *tableData, plan simpleSelectPlan, args []any) (rowVersion, bool, error) {
	if data == nil || (len(data.lazyRecords) == 0 && data.lazyPath == "") || plan.order.col != "" || len(plan.conds) != 1 {
		return rowVersion{}, false, nil
	}
	cond := plan.conds[0]
	if cond.op != "=" {
		return rowVersion{}, false, nil
	}
	pk := integerPrimaryKeyColumn(t)
	if cond.col != pk && cond.col != "rowid" && cond.col != "_rowid_" && cond.col != "oid" {
		col, _, ok := t.Column(cond.col)
		if !ok || !col.PrimaryKey {
			return rowVersion{}, false, nil
		}
	}
	v, ok := simpleSelectCondValue(cond, args)
	if !ok {
		return rowVersion{}, true, nil
	}
	id, ok := asInt(v)
	if !ok {
		return rowVersion{}, true, nil
	}
	row, found, err := data.lazyRowByID(t, id)
	if !found {
		return rowVersion{}, true, nil
	}
	return row, true, err
}

func simpleSelectAccessCoversConditions(plan simpleSelectPlan, args []any) bool {
	if len(plan.conds) == 0 {
		return false
	}
	if len(plan.conds) == 1 && plan.conds[0].op == "=" {
		_, ok := simpleSelectCondValue(plan.conds[0], args)
		return ok
	}
	rangeCol, _, _, ok := simpleSelectRange(plan.conds, args)
	if !ok {
		return false
	}
	for _, cond := range plan.conds {
		switch cond.op {
		case "<", "<=", ">", ">=":
			if cond.col != rangeCol {
				return false
			}
			if _, ok := simpleSelectCondValue(cond, args); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func simpleSelectStreamCap(table *catalog.Table, plan simpleSelectPlan, data *tableData, args []any) int {
	if data == nil || len(data.Rows) == 0 {
		return 0
	}
	if simpleSelectHasUniqueEquality(table, plan) {
		return 1
	}
	if _, _, _, ok := simpleSelectRange(plan.conds, args); ok {
		if len(data.Rows) < 128 {
			return len(data.Rows)
		}
		return 128
	}
	return 16
}

func simpleSelectHasUniqueEquality(table *catalog.Table, plan simpleSelectPlan) bool {
	if table == nil {
		return false
	}
	for _, cond := range plan.conds {
		if cond.op != "=" {
			continue
		}
		if cond.col == "rowid" || cond.col == "_rowid_" || cond.col == "oid" {
			if _, _, shadowed := table.Column(cond.col); !shadowed {
				return true
			}
		}
		column, _, ok := table.Column(cond.col)
		if ok && (column.Unique || column.PrimaryKey && !isCompositeUniqueColumn(table, cond.col)) {
			return true
		}
		for _, index := range table.Indexes {
			if index.Unique && len(index.Columns) == 1 && index.Columns[0] == cond.col && index.Where == "" {
				return true
			}
		}
	}
	return false
}

func isCompositeUniqueColumn(table *catalog.Table, column string) bool {
	for _, index := range table.Indexes {
		if !index.Unique || len(index.Columns) < 2 {
			continue
		}
		for _, indexColumn := range index.Columns {
			if indexColumn == column {
				return true
			}
		}
	}
	return false
}

func parseSimpleSelectPlanCached(query string) (simpleSelectPlan, bool) {
	if cached, ok := simpleSelectCache.Load(query); ok {
		return cached.(simpleSelectPlan), true
	}
	plan, ok := parseSimpleSelectPlan(query)
	if !ok {
		return simpleSelectPlan{}, false
	}
	simpleSelectCache.Store(query, plan)
	return plan, true
}

func parseSimpleSelectPlan(query string) (simpleSelectPlan, bool) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	if !hasFoldPrefix(q, "select ") {
		return simpleSelectPlan{}, false
	}
	fromAt := indexFold(q, " from ")
	if fromAt < 0 {
		return simpleSelectPlan{}, false
	}
	selectPart := strings.TrimSpace(q[len("select "):fromAt])
	rest := strings.TrimSpace(q[fromAt+len(" from "):])
	limit := -1
	offset := 0
	if limitAt := indexFold(rest, " limit "); limitAt >= 0 {
		var ok bool
		limitPart := strings.TrimSpace(rest[limitAt+len(" limit "):])
		limit, offset, ok = parseSimpleLimit(limitPart)
		if !ok {
			return simpleSelectPlan{}, false
		}
		rest = strings.TrimSpace(rest[:limitAt])
	}
	var order simpleSelectOrder
	if orderAt := indexFold(rest, " order by "); orderAt >= 0 {
		var ok bool
		orderPart := strings.TrimSpace(rest[orderAt+len(" order by "):])
		order, ok = parseSimpleOrder(orderPart)
		if !ok {
			return simpleSelectPlan{}, false
		}
		rest = strings.TrimSpace(rest[:orderAt])
	}
	whereAt := indexFold(rest, " where ")
	tablePart := rest
	wherePart := ""
	if whereAt >= 0 {
		tablePart = strings.TrimSpace(rest[:whereAt])
		wherePart = strings.TrimSpace(rest[whereAt+len(" where "):])
	}
	if tablePart == "" || strings.ContainsAny(tablePart, " \t\n\r,()") {
		return simpleSelectPlan{}, false
	}
	table := catalog.Normalize(tablePart)
	if !isBareIdentifier(table) {
		return simpleSelectPlan{}, false
	}
	if table == "sqlite_schema" || table == "sqlite_master" {
		return simpleSelectPlan{}, false
	}
	rawCols := strings.Split(selectPart, ",")
	cols := make([]string, 0, len(rawCols))
	for _, raw := range rawCols {
		col := catalog.Normalize(strings.TrimSpace(raw))
		if col == "" || !isBareIdentifier(col) {
			return simpleSelectPlan{}, false
		}
		cols = append(cols, col)
	}
	conds, ok := parseSimpleSelectConds(wherePart)
	if !ok {
		return simpleSelectPlan{}, false
	}
	return simpleSelectPlan{table: table, cols: cols, conds: conds, order: order, limit: limit, offset: offset}, true
}

func parseSimpleOrder(part string) (simpleSelectOrder, bool) {
	fields := strings.Fields(part)
	if len(fields) == 0 || len(fields) > 2 {
		return simpleSelectOrder{}, false
	}
	col := catalog.Normalize(fields[0])
	if !isBareIdentifier(col) {
		return simpleSelectOrder{}, false
	}
	order := simpleSelectOrder{col: col}
	if len(fields) == 2 {
		switch strings.ToLower(fields[1]) {
		case "asc":
		case "desc":
			order.desc = true
		default:
			return simpleSelectOrder{}, false
		}
	}
	return order, true
}

func parseSimpleLimit(part string) (int, int, bool) {
	fields := strings.Fields(part)
	if len(fields) != 1 && len(fields) != 3 {
		return 0, 0, false
	}
	limit, err := strconv.Atoi(fields[0])
	if err != nil || limit < 0 {
		return 0, 0, false
	}
	offset := 0
	if len(fields) == 3 {
		if !strings.EqualFold(fields[1], "offset") {
			return 0, 0, false
		}
		offset, err = strconv.Atoi(fields[2])
		if err != nil || offset < 0 {
			return 0, 0, false
		}
	}
	return limit, offset, true
}

func parseSimpleSelectConds(wherePart string) ([]simpleSelectCond, bool) {
	if strings.TrimSpace(wherePart) == "" {
		return nil, true
	}
	parts := splitFoldAND(wherePart)
	conds := make([]simpleSelectCond, 0, len(parts))
	param := 0
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) != 3 {
			return nil, false
		}
		col := catalog.Normalize(fields[0])
		if !isBareIdentifier(col) {
			return nil, false
		}
		op := fields[1]
		switch op {
		case "=", "!=", "<>", "<", "<=", ">", ">=":
		default:
			return nil, false
		}
		cond := simpleSelectCond{col: col, op: op}
		if fields[2] == "?" {
			cond.param = param
			cond.hasParam = true
			param++
		} else {
			lit, ok := parseSimpleLiteral(fields[2])
			if !ok {
				return nil, false
			}
			cond.literal = lit
		}
		conds = append(conds, cond)
	}
	return conds, true
}

func splitFoldAND(s string) []string {
	var out []string
	rest := s
	for {
		i := indexFold(rest, " and ")
		if i < 0 {
			out = append(out, rest)
			return out
		}
		out = append(out, rest[:i])
		rest = rest[i+len(" and "):]
	}
}

func parseSimpleLiteral(raw string) (any, bool) {
	if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), true
	}
	if strings.EqualFold(raw, "null") {
		return nil, true
	}
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return i, true
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f, true
	}
	return nil, false
}

func simpleSelectProjectors(t *catalog.Table, cols []string, projectors []int) ([]int, bool) {
	projectors = projectors[:0]
	for _, col := range cols {
		if col == "rowid" || col == "_rowid_" || col == "oid" {
			projectors = append(projectors, -1)
			continue
		}
		if _, idx, ok := t.Column(col); !ok {
			return nil, false
		} else {
			projectors = append(projectors, idx)
			continue
		}
	}
	return projectors, true
}

func simpleSelectColumnsValid(t *catalog.Table, cols []string) bool {
	var projectors [8]int
	_, ok := simpleSelectProjectors(t, cols, projectors[:0])
	return ok
}

func appendSimpleSelectProjection(out []any, row rowVersion, projectors []int, cols []string) []any {
	for i, projector := range projectors {
		if projector < 0 {
			out = append(out, row.RowID)
			continue
		}
		if value, ok := row.positionalAt(projector); ok {
			out = append(out, value)
		} else {
			out = append(out, row.Values[cols[i]])
		}
	}
	return out
}

func simpleSelectCandidatePositions(s *persistedState, plan simpleSelectPlan, data *tableData, args []any) []int {
	if data.runtimeDeferred() {
		return nil
	}
	for _, cond := range plan.conds {
		if cond.op != "=" {
			continue
		}
		v, ok := simpleSelectCondValue(cond, args)
		if !ok {
			return []int{}
		}
		if positions, used := lookupSimpleSelectEquality(s, plan.table, data, cond.col, v); used {
			return positions
		}
	}
	col, start, end, ok := simpleSelectRange(plan.conds, args)
	if !ok {
		return nil
	}
	return rangeCandidatePositions(s, plan.table, data, col, start, end)
}

func simpleSelectEachCandidatePosition(s *persistedState, plan simpleSelectPlan, data *tableData, args []any, fn func(int) bool) bool {
	if data.runtimeDeferred() {
		return false
	}
	for _, cond := range plan.conds {
		if cond.op != "=" {
			continue
		}
		v, ok := simpleSelectCondValue(cond, args)
		if !ok {
			return true
		}
		if used := eachSimpleSelectEquality(s, plan.table, data, cond.col, v, fn); used {
			return true
		}
	}
	col, start, end, ok := simpleSelectRange(plan.conds, args)
	if !ok {
		return false
	}
	return eachRangeCandidatePosition(s, plan.table, data, col, start, end, fn)
}

func eachRangeCandidatePosition(s *persistedState, table string, data *tableData, col string, start, end *rangeBound, fn func(int) bool) bool {
	t, _ := s.Catalog.Table(table)
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		lo, hi, ok := rowidRangeBounds(start, end)
		if !ok {
			return true
		}
		return data.eachRangeRowID(lo, hi, fn)
	}
	idxName := usableIndexForColumn(s, table, col)
	if idxName == "" {
		return false
	}
	startKey, endKey := indexRangeKeys(start, end)
	return data.eachRangeIndexPosition(idxName, startKey, endKey, fn)
}

func simpleSelectOrderPositions(s *persistedState, plan simpleSelectPlan, data *tableData) []int {
	if data.runtimeDeferred() {
		return nil
	}
	t, _ := s.Catalog.Table(plan.table)
	col := plan.order.col
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		return data.rangeRowIDs(nil, nil)
	}
	idxName := usableIndexForColumn(s, plan.table, col)
	if idxName == "" {
		return nil
	}
	return data.rangeIndexPositions(idxName, nil, nil)
}

func simpleSelectOrderCompatible(plan simpleSelectPlan, args []any) bool {
	for _, cond := range plan.conds {
		if cond.col == plan.order.col && cond.op == "=" {
			return true
		}
	}
	col, _, _, ok := simpleSelectRange(plan.conds, args)
	return ok && col == plan.order.col
}

func lookupSimpleSelectEquality(s *persistedState, table string, data *tableData, col string, val any) ([]int, bool) {
	if data.runtimeDeferred() {
		return nil, false
	}
	t, _ := s.Catalog.Table(table)
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		id, ok := asInt(val)
		if !ok {
			return []int{}, true
		}
		pos, ok := data.lookupRowID(id)
		if !ok {
			return []int{}, true
		}
		return []int{pos}, true
	}
	idxName := usableIndexForColumn(s, table, col)
	if idxName == "" {
		return nil, false
	}
	var idx catalog.Index
	for _, candidate := range t.Indexes {
		if candidate.Name == idxName {
			idx = candidate
			break
		}
	}
	return data.lookupIndexPositions(idxName, runtimeIndexLookupKey(idx, val)), true
}

func eachSimpleSelectEquality(s *persistedState, table string, data *tableData, col string, val any, fn func(int) bool) bool {
	t, _ := s.Catalog.Table(table)
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		id, ok := asInt(val)
		if !ok {
			return true
		}
		if pos, ok := data.lookupRowID(id); ok {
			fn(pos)
		}
		return true
	}
	idxName := usableIndexForColumn(s, table, col)
	if idxName == "" {
		return false
	}
	var idx catalog.Index
	for _, candidate := range t.Indexes {
		if candidate.Name == idxName {
			idx = candidate
			break
		}
	}
	key := runtimeIndexLookupKeyBytes(idx, val)
	used := true
	data.eachIndexPositionBytes(idxName, key, fn)
	return used
}

func simpleSelectRange(conds []simpleSelectCond, args []any) (string, *rangeBound, *rangeBound, bool) {
	var col string
	var start, end *rangeBound
	for _, cond := range conds {
		switch cond.op {
		case "<", "<=", ">", ">=":
		default:
			continue
		}
		v, ok := simpleSelectCondValue(cond, args)
		if !ok {
			return "", nil, nil, false
		}
		if col == "" {
			col = cond.col
		} else if col != cond.col {
			return "", nil, nil, false
		}
		bound := &rangeBound{value: v, inclusive: cond.op == "<=" || cond.op == ">="}
		if cond.op == ">" || cond.op == ">=" {
			start = tighterStart(start, bound)
		} else {
			end = tighterEnd(end, bound)
		}
	}
	return col, start, end, col != ""
}

func simpleSelectMatches(t *catalog.Table, plan simpleSelectPlan, row rowVersion, args []any) bool {
	for _, cond := range plan.conds {
		right, ok := simpleSelectCondValue(cond, args)
		if !ok {
			return false
		}
		left := any(nil)
		if cond.col == "rowid" || cond.col == "_rowid_" || cond.col == "oid" {
			left = row.RowID
		} else {
			left = row.value(t, cond.col)
		}
		c, ok := compareValues(left, right, columnCollation(t, cond.col))
		if !ok {
			return false
		}
		switch cond.op {
		case "=":
			if c != 0 {
				return false
			}
		case "!=", "<>":
			if c == 0 {
				return false
			}
		case "<":
			if c >= 0 {
				return false
			}
		case "<=":
			if c > 0 {
				return false
			}
		case ">":
			if c <= 0 {
				return false
			}
		case ">=":
			if c < 0 {
				return false
			}
		}
	}
	return true
}

func applySimpleFlatLimit(rows []any, width, limit, offset int) []any {
	if width <= 0 {
		return rows
	}
	total := len(rows) / width
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return rows[offset*width : end*width]
}

func reverseInts(values []int) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func simpleSelectCondValue(cond simpleSelectCond, args []any) (any, bool) {
	if !cond.hasParam {
		return cond.literal, true
	}
	if cond.param < 0 || cond.param >= len(args) {
		return nil, false
	}
	return args[cond.param], true
}

func columnCollation(t *catalog.Table, col string) string {
	if t == nil {
		return "BINARY"
	}
	if c, _, ok := t.Column(col); ok && c.Collation != "" {
		return strings.ToUpper(c.Collation)
	}
	return "BINARY"
}

func hasFoldPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func indexFold(s, needle string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(needle))
}

func queryRows(ctx context.Context, s *persistedState, query string, args ...any) (*Rows, error) {
	if rows, ok, err := queryPragma(s, query); ok || err != nil {
		return rows, err
	}
	if !s.compositeRead {
		if rows, ok := queryPlan(s, query); ok {
			return rows, nil
		}
		if rows, ok, err := execSimpleSelectPlan(s, query, args); ok || err != nil {
			return rows, err
		}
	}
	s.ensureRuntime()
	return withQueryStatement(query, func(stmt ast.Statement) (*Rows, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		switch st := stmt.(type) {
		case *ast.SelectStmt:
			return execSelect(s, st, args)
		case *ast.ExplainStmt:
			return explain(st), nil
		default:
			return nil, fmt.Errorf("statement does not return rows in v1: %T", stmt)
		}
	})
}

func execCreateTable(s *persistedState, st *ast.CreateTableStmt, strict bool, rawSQL string) (Result, error) {
	name := qname(st.Table)
	if st.Select != nil {
		return execCreateTableAsSelect(s, st, rawSQL)
	}
	if st.IfNotExists {
		if _, ok := s.Catalog.Table(name); ok {
			return Result{}, nil
		}
	}
	t := &catalog.Table{Name: name, Strict: strict, SQL: strings.TrimSpace(rawSQL)}
	seenPK := false
	for _, col := range st.Columns {
		typ, err := catalog.ParseType(string(col.Type.Name))
		if err != nil {
			return Result{}, err
		}
		c := catalog.Column{
			Name:       catalog.Normalize(col.Name.Unquoted),
			Type:       typ,
			DeclType:   strings.ToUpper(string(col.Type.Name)),
			NotNull:    col.NotNull || col.PrimaryKey,
			PrimaryKey: col.PrimaryKey,
			Unique:     col.Unique,
			Collation:  columnCollationFromCreate(rawSQL, col.Name.Unquoted),
		}
		if col.Default != nil {
			v, err := evalExpr(col.Default, nil, &binder{})
			if err != nil {
				return Result{}, fmt.Errorf("invalid default for %s: %w", c.Name, err)
			}
			c.Default = applyAffinity(c, v)
			c.DefaultSQL = formatDefaultSQL(c.Default)
		}
		if col.Check != nil {
			c.Check = columnCheckFromCreate(rawSQL, col.Name.Unquoted)
		}
		if col.References != nil {
			refCols := make([]string, 0, len(col.References.Columns))
			for _, id := range col.References.Columns {
				refCols = append(refCols, catalog.Normalize(id.Unquoted))
			}
			if len(refCols) == 0 {
				refCols = []string{primaryKeyNamePlaceholder}
			}
			c.References = &catalog.ForeignKey{
				Columns:    []string{c.Name},
				RefTable:   qname(col.References.Table),
				RefColumns: refCols,
				OnDelete:   refAction(col.References.OnDelete),
				OnUpdate:   refAction(col.References.OnUpdate),
			}
			t.ForeignKeys = append(t.ForeignKeys, *c.References)
		}
		if c.PrimaryKey {
			if seenPK {
				return Result{}, errors.New("multiple primary keys are not supported")
			}
			seenPK = true
			t.PrimaryKeyColumns = append(t.PrimaryKeyColumns, c.Name)
		}
		if strict && !catalog.StrictTypeAllowed(c.Type) {
			return Result{}, fmt.Errorf("unsupported STRICT column type: %s", c.DeclType)
		}
		t.Columns = append(t.Columns, c)
	}
	t.Checks = tableChecksFromCreate(rawSQL)
	for _, tc := range st.Constraints {
		if tc.Type != ast.PrimaryKeyConstraint && tc.Type != ast.UniqueConstraint && tc.Type != ast.ForeignKeyConstraint {
			continue
		}
		cols := make([]string, 0, len(tc.Columns))
		for _, c := range tc.Columns {
			cols = append(cols, catalog.Normalize(c.Name.Unquoted))
		}
		if tc.Type == ast.ForeignKeyConstraint {
			refCols := make([]string, 0, len(tc.RefCols))
			for _, id := range tc.RefCols {
				refCols = append(refCols, catalog.Normalize(id.Unquoted))
			}
			if len(refCols) == 0 {
				refCols = []string{primaryKeyNamePlaceholder}
			}
			t.ForeignKeys = append(t.ForeignKeys, catalog.ForeignKey{
				Columns:    cols,
				RefTable:   qname(tc.RefTable),
				RefColumns: refCols,
				OnDelete:   refAction(tc.OnDelete),
				OnUpdate:   refAction(tc.OnUpdate),
			})
			continue
		}
		if tc.Type == ast.PrimaryKeyConstraint {
			if seenPK {
				return Result{}, errors.New("multiple primary keys are not supported")
			}
			seenPK = true
			t.PrimaryKeyColumns = append(t.PrimaryKeyColumns, cols...)
			for _, c := range cols {
				col, i, ok := t.Column(c)
				if !ok {
					return Result{}, fmt.Errorf("unknown primary key column: %s", c)
				}
				col.PrimaryKey = true
				col.NotNull = true
				t.Columns[i] = col
			}
		}
		t.Indexes = append(t.Indexes, catalog.Index{Name: strings.Join(cols, "_") + "_idx", Columns: cols, Unique: tc.Type != ast.IndexConstraint})
	}
	if err := s.Catalog.CreateTable(t, st.IfNotExists); err != nil {
		return Result{}, err
	}
	s.Pragmas.SchemaVer++
	s.Tables[catalog.Normalize(name)] = &tableData{}
	s.Tables[catalog.Normalize(name)].rebuildRuntime(t)
	return Result{}, nil
}

func execCreateTableAsSelect(s *persistedState, st *ast.CreateTableStmt, rawSQL string) (Result, error) {
	name := qname(st.Table)
	rows, err := execSelect(s, st.Select, nil)
	if err != nil {
		return Result{}, err
	}
	t := &catalog.Table{Name: name, SQL: strings.TrimSpace(rawSQL)}
	for i, col := range rows.Columns {
		decl := ctasDeclType(s, st.Select, i)
		typ, _ := catalog.ParseType(decl)
		if decl == "" {
			typ = catalog.Any
		}
		t.Columns = append(t.Columns, catalog.Column{Name: catalog.Normalize(col), Type: typ, DeclType: decl, Collation: "BINARY"})
	}
	if err := s.Catalog.CreateTable(t, st.IfNotExists); err != nil {
		return Result{}, err
	}
	data := &tableData{}
	s.Tables[catalog.Normalize(name)] = data
	for {
		values, ok := rows.Next()
		if !ok {
			break
		}
		row := make(map[string]any, len(t.Columns))
		for i, col := range t.Columns {
			if i < len(values) {
				row[col.Name] = values[i]
			}
		}
		rowid := t.NextRowID
		data.Rows = append(data.Rows, newRowVersion(t, rowid, row, s.NextTx))
		t.NextRowID++
	}
	data.rebuildRuntime(t)
	s.Pragmas.SchemaVer++
	return Result{RowsAffected: int64(len(data.Rows))}, nil
}

func columnCollationFromCreate(rawSQL, colName string) string {
	defs := tableDefinitionParts(rawSQL)
	needle := catalog.Normalize(strings.Trim(colName, `"'[]`))
	for _, def := range defs {
		fields := strings.Fields(def)
		if len(fields) < 3 || catalog.Normalize(strings.Trim(fields[0], `"'[]`)) != needle {
			continue
		}
		for i := 1; i+1 < len(fields); i++ {
			if strings.EqualFold(fields[i], "collate") {
				switch strings.ToUpper(strings.Trim(fields[i+1], `"'[]`)) {
				case "NOCASE":
					return "NOCASE"
				case "RTRIM":
					return "RTRIM"
				default:
					return "BINARY"
				}
			}
		}
	}
	return "BINARY"
}

func columnCheckFromCreate(rawSQL, colName string) string {
	defs := tableDefinitionParts(rawSQL)
	needle := catalog.Normalize(strings.Trim(colName, `"'[]`))
	for _, def := range defs {
		fields := strings.Fields(def)
		if len(fields) == 0 || catalog.Normalize(strings.Trim(fields[0], `"'[]`)) != needle {
			continue
		}
		return firstCheckExpression(def)
	}
	return ""
}

func tableChecksFromCreate(rawSQL string) []string {
	var checks []string
	for _, def := range tableDefinitionParts(rawSQL) {
		fields := strings.Fields(def)
		if len(fields) == 0 {
			continue
		}
		head := strings.ToLower(strings.Trim(fields[0], `"'[]`))
		if head != "check" && head != "constraint" {
			continue
		}
		if check := firstCheckExpression(def); check != "" {
			checks = append(checks, check)
		}
	}
	return checks
}

func firstCheckExpression(def string) string {
	lower := strings.ToLower(def)
	idx := strings.Index(lower, "check")
	if idx < 0 {
		return ""
	}
	open := strings.Index(def[idx:], "(")
	if open < 0 {
		return ""
	}
	open += idx
	depth := 0
	inQuote := rune(0)
	for i, r := range def[open:] {
		pos := open + i
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			inQuote = r
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(def[open+1 : pos])
			}
		}
	}
	return ""
}

func tableDefinitionParts(rawSQL string) []string {
	start := strings.Index(rawSQL, "(")
	end := strings.LastIndex(rawSQL, ")")
	if start < 0 || end <= start {
		return nil
	}
	body := rawSQL[start+1 : end]
	var parts []string
	depth := 0
	inQuote := rune(0)
	last := 0
	for i, r := range body {
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			inQuote = r
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(body[last:i]))
				last = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(body[last:]))
	return parts
}

func ctasDeclType(s *persistedState, sel *ast.SelectStmt, idx int) string {
	if sel == nil || idx < 0 || idx >= len(sel.Columns) {
		return ""
	}
	name := exprColumnName(sel.Columns[idx].Expr)
	if name == "" {
		return ""
	}
	for _, ref := range sel.From {
		if simple, ok := ref.(*ast.SimpleTable); ok {
			t, ok := s.Catalog.Table(qname(simple.Name))
			if !ok {
				continue
			}
			if col, _, ok := t.Column(name); ok {
				switch col.Type {
				case catalog.Integer:
					return "INT"
				case catalog.Real:
					return "REAL"
				case catalog.Text:
					return "TEXT"
				case catalog.Numeric:
					return "NUM"
				case catalog.Blob:
					return ""
				default:
					return col.DeclType
				}
			}
		}
	}
	return ""
}

func execCreateView(s *persistedState, st *ast.CreateViewStmt, rawSQL string) (Result, error) {
	name := qname(st.Name)
	selectSQL := selectSQLFromCreateView(rawSQL)
	if strings.TrimSpace(selectSQL) == "" {
		return Result{}, errors.New("invalid CREATE VIEW")
	}
	cols, err := inferViewColumns(s, st)
	if err != nil {
		return Result{}, err
	}
	view := &catalog.View{
		Name:      name,
		Columns:   cols,
		SQL:       strings.TrimSpace(rawSQL),
		SelectSQL: selectSQL,
	}
	if err := s.Catalog.CreateView(view, st.OrReplace); err != nil {
		return Result{}, err
	}
	s.Pragmas.SchemaVer++
	return Result{}, nil
}

func inferViewColumns(s *persistedState, st *ast.CreateViewStmt) ([]catalog.Column, error) {
	tables, _, err := buildRowSet(s, st.Select.From)
	if err != nil {
		return nil, err
	}
	names, _, err := selectList(tables, st.Select.Columns)
	if err != nil {
		return nil, err
	}
	if len(st.Columns) > 0 {
		names = names[:0]
		for _, id := range st.Columns {
			names = append(names, catalog.Normalize(id.Unquoted))
		}
	}
	cols := make([]catalog.Column, 0, len(names))
	for _, name := range names {
		cols = append(cols, catalog.Column{Name: catalog.Normalize(name), Type: catalog.Any, DeclType: "ANY"})
	}
	return cols, nil
}

func selectSQLFromCreateView(rawSQL string) string {
	q := strings.TrimSpace(strings.TrimSuffix(rawSQL, ";"))
	lower := strings.ToLower(q)
	idx := strings.Index(lower, " as ")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(q[idx+len(" as "):])
}

func execDropTable(s *persistedState, st *ast.DropTableStmt, rawSQL string) (Result, error) {
	for _, t := range st.Tables {
		if _, ok := hasVirtualTables(s, qname(t)); ok {
			if err := execDropVirtualTable(s, qname(t)); err != nil {
				return Result{}, err
			}
		}
	}
	if isDropViewSQL(rawSQL) {
		for _, view := range st.Tables {
			if err := s.Catalog.DropView(qname(view), st.IfExists); err != nil {
				return Result{}, err
			}
			s.Pragmas.SchemaVer++
		}
		return Result{}, nil
	}
	for _, table := range st.Tables {
		name := catalog.Normalize(qname(table))
		if err := s.Catalog.DropTable(name, st.IfExists); err != nil {
			return Result{}, err
		}
		s.Pragmas.SchemaVer++
		delete(s.Tables, name)
	}
	return Result{}, nil
}

func isDropViewSQL(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	return strings.HasPrefix(q, "drop view ") || strings.HasPrefix(q, "drop materialized view ")
}

func execManualDropView(s *persistedState, query string) (Result, bool, error) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	lower := strings.ToLower(q)
	prefix := "drop view "
	if !strings.HasPrefix(lower, prefix) {
		return Result{}, false, nil
	}
	rest := strings.TrimSpace(q[len(prefix):])
	ifExists := false
	if strings.HasPrefix(strings.ToLower(rest), "if exists ") {
		ifExists = true
		rest = strings.TrimSpace(rest[len("if exists "):])
	}
	if rest == "" || strings.Contains(rest, ",") {
		return Result{}, true, errors.New("invalid DROP VIEW")
	}
	name := strings.Trim(rest, `"'`)
	if err := s.Catalog.DropView(name, ifExists); err != nil {
		return Result{}, true, err
	}
	s.Pragmas.SchemaVer++
	return Result{}, true, nil
}

func execCreateIndex(s *persistedState, st *ast.CreateIndexStmt, rawSQL string) (Result, error) {
	t, ok := s.Catalog.Table(qname(st.Table))
	if !ok {
		return Result{}, fmt.Errorf("table does not exist: %s", qname(st.Table))
	}
	idxName := catalog.Normalize(st.Name.Unquoted)
	if _, ok := catalogIndexByName(s, idxName); ok {
		return Result{}, fmt.Errorf("index already exists: %s", st.Name.Unquoted)
	}
	cols := make([]string, 0, len(st.Columns))
	for _, c := range st.Columns {
		name := catalog.Normalize(c.Name.Unquoted)
		if _, _, ok := t.Column(name); !ok {
			return Result{}, fmt.Errorf("unknown index column: %s", name)
		}
		cols = append(cols, name)
	}
	where := partialIndexWhereClause(rawSQL)
	idx := catalog.Index{Name: idxName, Columns: cols, Unique: st.Type == ast.UniqueConstraint, SQL: strings.TrimSpace(rawSQL), Where: where, Partial: where != ""}
	if idx.Unique {
		if err := validateUniqueIndexExisting(s, t, s.Tables[t.Name], idx); err != nil {
			return Result{}, err
		}
	}
	t.Indexes = append(t.Indexes, idx)
	if data := s.Tables[t.Name]; data != nil {
		data.rebuildRuntime(t)
	}
	t.SchemaVers++
	s.Pragmas.SchemaVer++
	return Result{}, nil
}

func partialIndexWhereClause(query string) string {
	onAt := indexFold(query, " on ")
	if onAt < 0 {
		return ""
	}
	afterOn := query[onAt+len(" on "):]
	openRel := strings.IndexByte(afterOn, '(')
	if openRel < 0 {
		return ""
	}
	depth := 0
	var quote byte
	closeAt := -1
	for i := openRel; i < len(afterOn); i++ {
		c := afterOn[i]
		if quote != 0 {
			if c == quote {
				if i+1 < len(afterOn) && afterOn[i+1] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '[':
			quote = ']'
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				closeAt = i
				i = len(afterOn)
			}
		}
	}
	if closeAt < 0 {
		return ""
	}
	tail := strings.TrimSpace(afterOn[closeAt+1:])
	if len(tail) >= len("where ") && strings.EqualFold(tail[:len("where ")], "where ") {
		return strings.TrimSpace(tail[len("where "):])
	}
	return ""
}

func execManualCreateIndex(s *persistedState, query string) (Result, bool, error) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	lower := strings.ToLower(q)
	if !strings.HasPrefix(lower, "create index ") && !strings.HasPrefix(lower, "create unique index ") {
		return Result{}, false, nil
	}
	unique := strings.HasPrefix(lower, "create unique index ")
	rest := strings.TrimSpace(q[len("create index "):])
	if unique {
		rest = strings.TrimSpace(q[len("create unique index "):])
	}
	ifNotExists := false
	if strings.HasPrefix(strings.ToLower(rest), "if not exists ") {
		ifNotExists = true
		rest = strings.TrimSpace(rest[len("if not exists "):])
	}
	parts := strings.SplitN(rest, " on ", 2)
	if len(parts) != 2 {
		parts = strings.SplitN(rest, " ON ", 2)
	}
	if len(parts) != 2 {
		return Result{}, true, errors.New("invalid CREATE INDEX")
	}
	idxName := catalog.Normalize(parts[0])
	if _, ok := catalogIndexByName(s, idxName); ok {
		if ifNotExists {
			return Result{}, true, nil
		}
		return Result{}, true, fmt.Errorf("index already exists: %s", idxName)
	}
	afterOn := strings.TrimSpace(parts[1])
	open := strings.Index(afterOn, "(")
	close := strings.LastIndex(afterOn, ")")
	if open < 0 || close < open {
		return Result{}, true, errors.New("invalid CREATE INDEX columns")
	}
	tableName := catalog.Normalize(strings.TrimSpace(afterOn[:open]))
	expr := strings.TrimSpace(afterOn[open+1 : close])
	where := strings.TrimSpace(afterOn[close+1:])
	if strings.HasPrefix(strings.ToLower(where), "where ") {
		where = strings.TrimSpace(where[len("where "):])
	} else {
		where = ""
	}
	t, ok := s.Catalog.Table(tableName)
	if !ok {
		return Result{}, true, fmt.Errorf("table does not exist: %s", tableName)
	}
	idx := catalog.Index{Name: idxName, Unique: unique, SQL: q, Expression: expr, Where: where, Partial: where != ""}
	if isBareIdentifier(expr) {
		idx.Columns = []string{catalog.Normalize(expr)}
	}
	if idx.Unique && len(idx.Columns) > 0 {
		if err := validateUniqueIndexExisting(s, t, s.Tables[t.Name], idx); err != nil {
			return Result{}, true, err
		}
	}
	t.Indexes = append(t.Indexes, idx)
	if data := s.Tables[t.Name]; data != nil {
		data.rebuildRuntime(t)
	}
	t.SchemaVers++
	s.Pragmas.SchemaVer++
	return Result{}, true, nil
}

func catalogIndexByName(s *persistedState, name string) (catalog.Index, bool) {
	name = catalog.Normalize(name)
	for _, t := range s.Catalog.Tables {
		for _, idx := range t.Indexes {
			if catalog.Normalize(idx.Name) == name {
				return idx, true
			}
		}
	}
	return catalog.Index{}, false
}

func partialIndexIncludesRow(s *persistedState, idx catalog.Index, values map[string]any) (bool, error) {
	if !idx.Partial || strings.TrimSpace(idx.Where) == "" {
		return true, nil
	}
	stmt, err := parseStatementCached("SELECT 1 WHERE " + idx.Where)
	if err != nil {
		return false, fmt.Errorf("parse partial index predicate %s: %w", idx.Name, err)
	}
	selectStmt, ok := stmt.(*ast.SelectStmt)
	if !ok {
		return false, fmt.Errorf("invalid partial index predicate: %s", idx.Name)
	}
	return matchWhereState(s, selectStmt.Where, values, &binder{})
}

func validateUniqueIndexExisting(s *persistedState, t *catalog.Table, data *tableData, idx catalog.Index) error {
	if t == nil || data == nil || !idx.Unique || len(idx.Columns) == 0 {
		return nil
	}
	for i := range data.Rows {
		left := data.Rows[i]
		if left.Deleted != 0 {
			continue
		}
		leftIncluded, err := partialIndexIncludesRow(s, idx, left.Values)
		if err != nil {
			return err
		}
		if !leftIncluded {
			continue
		}
		for j := i + 1; j < len(data.Rows); j++ {
			right := data.Rows[j]
			if right.Deleted != 0 {
				continue
			}
			rightIncluded, err := partialIndexIncludesRow(s, idx, right.Values)
			if err != nil {
				return err
			}
			if !rightIncluded {
				continue
			}
			same := true
			for _, col := range idx.Columns {
				same = same && valuesEqual(left.value(t, col), right.value(t, col), columnCollation(t, col))
			}
			if same {
				return fmt.Errorf("unique constraint failed: %s.%s", t.Name, idx.Name)
			}
		}
	}
	return nil
}

func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func execDropIndex(s *persistedState, st *ast.DropIndexStmt) (Result, error) {
	for _, t := range s.Catalog.Tables {
		for i, idx := range t.Indexes {
			if catalog.Normalize(idx.Name) == catalog.Normalize(st.Name.Unquoted) {
				t.Indexes = append(t.Indexes[:i], t.Indexes[i+1:]...)
				if data := s.Tables[t.Name]; data != nil {
					data.rebuildRuntime(t)
				}
				t.SchemaVers++
				s.Pragmas.SchemaVer++
				return Result{}, nil
			}
		}
	}
	if st.IfExists {
		return Result{}, nil
	}
	return Result{}, fmt.Errorf("index does not exist: %s", st.Name.Unquoted)
}

func refAction(a ast.RefAction) string {
	switch a {
	case ast.Cascade:
		return "cascade"
	case ast.SetNull:
		return "set null"
	case ast.SetDefault:
		return "set default"
	case ast.Restrict:
		return "restrict"
	default:
		return "no action"
	}
}

func execAlterTable(s *persistedState, st *ast.AlterTableStmt) (Result, error) {
	name := qname(st.Table)
	t, ok := s.Catalog.Table(name)
	if !ok {
		return Result{}, fmt.Errorf("table does not exist: %s", name)
	}
	for _, cmd := range st.Cmds {
		switch c := cmd.(type) {
		case *ast.AddColumnCmd:
			if _, _, ok := t.Column(c.Col.Name.Unquoted); ok {
				return Result{}, fmt.Errorf("duplicate column: %s", c.Col.Name.Unquoted)
			}
			typ, err := catalog.ParseType(string(c.Col.Type.Name))
			if err != nil {
				return Result{}, err
			}
			col := catalog.Column{Name: catalog.Normalize(c.Col.Name.Unquoted), Type: typ, DeclType: strings.ToUpper(string(c.Col.Type.Name)), NotNull: c.Col.NotNull, Collation: "BINARY"}
			if c.Col.Default != nil {
				v, err := evalExpr(c.Col.Default, nil, &binder{})
				if err != nil {
					return Result{}, err
				}
				col.Default = applyAffinity(col, v)
				col.DefaultSQL = formatDefaultSQL(col.Default)
			}
			t.Columns = append(t.Columns, col)
			for i := range s.Tables[t.Name].Rows {
				s.Tables[t.Name].copyRow(i)
				s.Tables[t.Name].Rows[i].Values[col.Name] = col.Default
				s.Tables[t.Name].Rows[i].syncPositioned(t)
			}
		case *ast.AlterColumnCmd:
			colName := catalog.Normalize(c.Name.Unquoted)
			_, colIdx, ok := t.Column(colName)
			if !ok {
				return Result{}, fmt.Errorf("no such column: %s", colName)
			}
			switch strings.ToLower(string(c.Action)) {
			case "rename":
				id, ok := c.Expr.(*ast.Ident)
				if !ok {
					return Result{}, errors.New("RENAME COLUMN requires a new column name")
				}
				newCol := catalog.Normalize(id.Unquoted)
				if newCol != colName {
					if _, _, dup := t.Column(newCol); dup {
						return Result{}, fmt.Errorf("duplicate column: %s", newCol)
					}
					t.Columns[colIdx].Name = newCol
					for i := range t.Indexes {
						for j := range t.Indexes[i].Columns {
							if t.Indexes[i].Columns[j] == colName {
								t.Indexes[i].Columns[j] = newCol
							}
						}
					}
					for i := range t.PrimaryKeyColumns {
						if t.PrimaryKeyColumns[i] == colName {
							t.PrimaryKeyColumns[i] = newCol
						}
					}
					for i := range t.ForeignKeys {
						for j := range t.ForeignKeys[i].Columns {
							if t.ForeignKeys[i].Columns[j] == colName {
								t.ForeignKeys[i].Columns[j] = newCol
							}
						}
					}
					if data := s.Tables[t.Name]; data != nil {
						for ri := range data.Rows {
							data.copyRow(ri)
							row := &data.Rows[ri]
							if v, exists := row.Values[colName]; exists {
								delete(row.Values, colName)
								row.Values[newCol] = v
							}
							row.syncPositioned(t)
						}
					}
				}
			case "set":
				if c.Expr == nil {
					// ALTER COLUMN ... SET NOT NULL
					t.Columns[colIdx].NotNull = true
					break
				}
				v, err := evalExpr(c.Expr, nil, &binder{})
				if err != nil {
					return Result{}, err
				}
				col := t.Columns[colIdx]
				col.Default = applyAffinity(col, v)
				col.DefaultSQL = formatDefaultSQL(col.Default)
				t.Columns[colIdx] = col
			case "set_not_null":
				t.Columns[colIdx].NotNull = true
			case "drop_default":
				t.Columns[colIdx].Default = nil
				t.Columns[colIdx].DefaultSQL = ""
			case "drop_not_null":
				t.Columns[colIdx].NotNull = false
			case "drop":
				t.Columns[colIdx].Default = nil
				t.Columns[colIdx].DefaultSQL = ""
			default:
				return Result{}, fmt.Errorf("unsupported ALTER COLUMN action: %q", string(c.Action))
			}
		case *ast.RenameTableCmd:
			newName := qname(c.NewName)
			delete(s.Catalog.Tables, t.Name)
			s.Catalog.Tables[newName] = t
			s.Tables[newName] = s.Tables[t.Name]
			delete(s.Tables, t.Name)
			t.Name = newName
		case *ast.DropColumnCmd:
			name := catalog.Normalize(c.Name.Unquoted)
			_, idx, ok := t.Column(name)
			if !ok {
				return Result{}, fmt.Errorf("no such column: %s", name)
			}
			if t.Columns[idx].PrimaryKey {
				return Result{}, errors.New("cannot drop primary key column")
			}
			t.Columns = append(t.Columns[:idx], t.Columns[idx+1:]...)
			for i := range t.Indexes {
				t.Indexes[i].Columns = removeString(t.Indexes[i].Columns, name)
			}
			for i := range t.ForeignKeys {
				t.ForeignKeys[i].Columns = removeString(t.ForeignKeys[i].Columns, name)
			}
			for i := range s.Tables[t.Name].Rows {
				s.Tables[t.Name].copyRow(i)
				delete(s.Tables[t.Name].Rows[i].Values, name)
				s.Tables[t.Name].Rows[i].syncPositioned(t)
			}
		default:
			return Result{}, fmt.Errorf("unsupported ALTER TABLE command: %T", cmd)
		}
	}
	t.SchemaVers++
	s.Pragmas.SchemaVer++
	if data := s.Tables[t.Name]; data != nil {
		data.rebuildRuntime(t)
	}
	return Result{}, nil
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

// viewRowsMaterialize reads the rows a view currently exposes.
func viewRowsMaterialize(s *persistedState, name string) ([]string, []map[string]any, error) {
	view, ok := s.Catalog.View(name)
	if !ok {
		return nil, nil, fmt.Errorf("no such view: %s", name)
	}
	s.ensureRuntime()
	stmt, err := parseViewSelectStatement(view.SelectSQL)
	if err != nil {
		return nil, nil, err
	}
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		return nil, nil, fmt.Errorf("view is not a SELECT: %s", view.Name)
	}
	rows, err := execSelectGeneric(s, sel, nil)
	if err != nil {
		return nil, nil, err
	}
	cols := make([]string, 0, len(view.Columns))
	for _, c := range view.Columns {
		cols = append(cols, c.Name)
	}
	if len(cols) == 0 {
		cols = append(cols, rows.Columns...)
	}
	var out []map[string]any
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			if i < len(row) {
				m[c] = row[i]
			}
		}
		out = append(out, m)
	}
	return cols, out, nil
}

// execViewInsteadOfUpdate applies an UPDATE to a view by firing its INSTEAD OF
// trigger for each exposed row the WHERE clause matches.
func execViewInsteadOfUpdate(s *persistedState, st *ast.UpdateStmt, name string, args []any) (Result, error) {
	cols, rows, err := viewRowsMaterialize(s, name)
	if err != nil {
		return Result{}, err
	}
	bind := binder{args: args}
	var affected int64
	for _, row := range rows {
		whereBind := bind
		ok, err := matchWhereState(s, st.Where, row, &whereBind)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			continue
		}
		next := make(map[string]any, len(row))
		for k, v := range row {
			next[k] = v
		}
		setBind := binder{args: args}
		for _, asn := range st.Set {
			col := catalog.Normalize(asn.Column.Unquoted)
			v, err := evalExpr(asn.Value, next, &setBind)
			if err != nil {
				return Result{}, err
			}
			next[col] = v
		}
		if _, err := fireInsteadOf(s, name, "update", row, next); err != nil {
			return Result{}, err
		}
		affected++
	}
	_ = cols
	return Result{RowsAffected: affected}, nil
}

// execViewInsteadOfDelete applies a DELETE to a view by firing its INSTEAD OF
// trigger for each exposed row the WHERE clause matches.
func execViewInsteadOfDelete(s *persistedState, st *ast.DeleteStmt, name string, args []any) (Result, error) {
	_, rows, err := viewRowsMaterialize(s, name)
	if err != nil {
		return Result{}, err
	}
	bind := binder{args: args}
	var affected int64
	for _, row := range rows {
		whereBind := bind
		ok, err := matchWhereState(s, st.Where, row, &whereBind)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			continue
		}
		if _, err := fireInsteadOf(s, name, "delete", row, nil); err != nil {
			return Result{}, err
		}
		affected++
	}
	return Result{RowsAffected: affected}, nil
}

// execInsertInsteadOf applies an INSERT to a view by firing its INSTEAD OF
// trigger once per proposed row.
func execInsertInsteadOf(s *persistedState, st *ast.InsertStmt, t *catalog.Table, cols []catalog.Column, args []any) (Result, error) {
	buildRow := func(values []any) (map[string]any, error) {
		if len(values) != len(cols) {
			return nil, errors.New("insert column/value count mismatch")
		}
		row := make(map[string]any, len(t.Columns))
		for _, col := range t.Columns {
			row[col.Name] = col.Default
		}
		for i, v := range values {
			cv, err := coerce(cols[i], v, t.Strict)
			if err != nil {
				return nil, err
			}
			row[cols[i].Name] = cv
		}
		return row, nil
	}
	var affected int64
	emit := func(values []any) error {
		row, err := buildRow(values)
		if err != nil {
			return err
		}
		if _, err := fireInsteadOf(s, t.Name, "insert", nil, row); err != nil {
			return err
		}
		affected++
		return nil
	}
	if st.Select != nil {
		rows, err := execSelect(s, st.Select, args)
		if err != nil {
			return Result{}, err
		}
		for {
			values, ok := rows.Next()
			if !ok {
				break
			}
			if err := emit(values); err != nil {
				return Result{}, err
			}
		}
		return Result{RowsAffected: affected}, nil
	}
	if st.ValuesExpr != nil {
		expanded, err := expandInsertValuesExpr(st.ValuesExpr, args, cols)
		if err != nil {
			return Result{}, err
		}
		for _, values := range expanded {
			if err := emit(values); err != nil {
				return Result{}, err
			}
		}
		return Result{RowsAffected: affected}, nil
	}
	for _, valueExprs := range st.Values {
		values := make([]any, 0, len(valueExprs))
		b := binder{args: args}
		for _, e := range valueExprs {
			v, err := evalExpr(e, nil, &b)
			if err != nil {
				return Result{}, err
			}
			values = append(values, v)
		}
		if err := emit(values); err != nil {
			return Result{}, err
		}
	}
	return Result{RowsAffected: affected}, nil
}

func execInsert(s *persistedState, st *ast.InsertStmt, args []any) (Result, error) {
	name := qname(st.Table)
	if vt, ok := hasVirtualTables(s, name); ok {
		return execVTabInsert(s, st, vt)
	}
	insteadOf := insteadOfTrigger(s, name, "insert")
	t, data, err := tableForWrite(s, name)
	if err != nil {
		if insteadOf {
			if view, ok := s.Catalog.View(name); ok {
				t = &catalog.Table{Name: view.Name, Columns: view.Columns}
				data = &tableData{}
			} else {
				return Result{}, err
			}
		} else {
			return Result{}, err
		}
	}
	cols := make([]catalog.Column, 0, len(st.Columns))
	if len(st.Columns) == 0 {
		cols = append(cols, t.Columns...)
	} else {
		for _, id := range st.Columns {
			col, _, ok := t.Column(id.Unquoted)
			if !ok {
				return Result{}, fmt.Errorf("unknown column: %s", id.Unquoted)
			}
			cols = append(cols, col)
		}
	}
	// A view with an INSTEAD OF trigger has no base rows of its own; the
	// trigger program performs the real work, so build the proposed row and
	// hand it over.
	if insteadOf {
		return execInsertInsteadOf(s, st, t, cols, args)
	}
	var affected int64
	var last int64
	var insert func(row map[string]any) (bool, int64, error)
	insertValues := func(values []any) (bool, int64, error) {
		if len(values) != len(cols) {
			return false, 0, errors.New("insert column/value count mismatch")
		}
		row := make(map[string]any, len(t.Columns))
		for _, col := range t.Columns {
			row[col.Name] = col.Default
		}
		for i, v := range values {
			cv, err := coerce(cols[i], v, t.Strict)
			if err != nil {
				return false, 0, err
			}
			row[cols[i].Name] = cv
		}
		return insert(row)
	}
	insert = func(row map[string]any) (bool, int64, error) {
		if proceed, err := beforeRowChange(s, t.Name, "insert", nil, nil, row); err != nil {
			return false, 0, err
		} else if !proceed {
			return false, 0, nil
		}
		rowid := t.NextRowID
		if pk := integerPrimaryKeyColumn(t); pk != "" {
			if row[pk] == nil {
				row[pk] = rowid
			} else if id, ok := asInt(row[pk]); ok {
				rowid = id
			}
		}
		if err := validateRow(s, t, data, row, 0); err != nil {
			if st.Replace {
				deleteReplaceConflicts(s, t, data, row)
				data.rebuildRuntime(t)
			} else if st.OnConflictDoNothing || st.Ignore {
				return false, 0, nil
			} else if len(st.OnConflictUpdate) == 0 {
				return false, 0, err
			} else {
				if err := applyUpsert(s, t, data, row, st.OnConflictUpdate, args); err != nil {
					return false, 0, err
				}
				return true, 0, nil
			}
		}
		if st.Replace {
			if err := validateRow(s, t, data, row, 0); err != nil {
				return false, 0, err
			}
		}
		data.Rows = append(data.Rows, newRowVersion(t, rowid, row, s.NextTx))
		if rowid >= t.NextRowID {
			t.NextRowID = rowid + 1
		}
		data.addRuntime(t, len(data.Rows)-1)
		if err := afterRowChange(s, t.Name, "insert", nil, nil, row); err != nil {
			return false, 0, err
		}
		return true, rowid, nil
	}
	if st.Select != nil {
		rows, err := execSelect(s, st.Select, args)
		if err != nil {
			return Result{}, err
		}
		for {
			values, ok := rows.Next()
			if !ok {
				break
			}
			if len(values) != len(cols) {
				return Result{}, errors.New("insert column/value count mismatch")
			}
			ok, rowid, err := insertValues(values)
			if err != nil {
				return Result{}, err
			}
			if ok {
				affected++
				if rowid != 0 {
					last = rowid
				}
			}
		}
		return Result{RowsAffected: affected, LastInsertID: last}, nil
	}
	if st.ValuesExpr != nil {
		rows, err := expandInsertValuesExpr(st.ValuesExpr, args, cols)
		if err != nil {
			return Result{}, err
		}
		for _, values := range rows {
			ok, rowid, err := insertValues(values)
			if err != nil {
				return Result{}, err
			}
			if ok {
				affected++
				if rowid != 0 {
					last = rowid
				}
			}
		}
		return Result{RowsAffected: affected, LastInsertID: last}, nil
	}
	bind := binder{args: args}
	for _, values := range st.Values {
		if len(values) != len(cols) {
			return Result{}, errors.New("insert column/value count mismatch")
		}
		rawValues := make([]any, len(values))
		for i, expr := range values {
			v, err := evalExpr(expr, nil, &bind)
			if err != nil {
				return Result{}, err
			}
			rawValues[i] = v
		}
		ok, rowid, err := insertValues(rawValues)
		if err != nil {
			return Result{}, err
		}
		if ok {
			affected++
			if rowid != 0 {
				last = rowid
			}
		}
	}
	return Result{RowsAffected: affected, LastInsertID: last}, nil
}

func expandInsertValuesExpr(expr ast.Expr, args []any, cols []catalog.Column) ([][]any, error) {
	bind := binder{args: args}
	var value any
	var err error
	if param, ok := expr.(*ast.Param); ok {
		// Keep row objects intact until insertObjectRows has expanded them into
		// columns. Ordinary bound values still pass through SQLite coercion.
		value, err = bind.rawParam(param.Raw)
	} else {
		value, err = evalExpr(expr, nil, &bind)
	}
	if err != nil {
		return nil, err
	}
	if rows, ok, err := insertObjectRows(value, cols); ok || err != nil {
		return rows, err
	}
	if len(cols) == 1 {
		return [][]any{{value}}, nil
	}
	return nil, errors.New("insert column/value count mismatch")
}

func insertObjectRows(value any, cols []catalog.Column) ([][]any, bool, error) {
	if values, ok, err := insertRowObjectValues(value, cols); ok || err != nil {
		return [][]any{values}, ok, err
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return nil, false, nil
		}
		fallthrough
	case reflect.Array:
		rows := make([][]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			elem := rv.Index(i).Interface()
			values, ok, err := insertRowObjectValues(elem, cols)
			if err != nil {
				return nil, true, err
			}
			if !ok {
				if len(cols) == 1 {
					rows = append(rows, []any{elem})
					continue
				}
				return nil, true, errors.New("insert row source must be a map or struct")
			}
			rows = append(rows, values)
		}
		return rows, true, nil
	default:
		return nil, false, nil
	}
}

func insertRowObjectValues(value any, cols []catalog.Column) ([]any, bool, error) {
	named := make(map[string]any, len(cols)*2)
	if !addRowNamedSource(named, value) {
		return nil, false, nil
	}
	values := make([]any, len(cols))
	for i, col := range cols {
		if v, ok := named[col.Name]; ok {
			values[i] = v
			continue
		}
		if v, ok := named[catalog.Normalize(col.Name)]; ok {
			values[i] = v
			continue
		}
		return nil, true, fmt.Errorf("missing insert row value for column: %s", col.Name)
	}
	return values, true, nil
}

func addRowNamedSource(out map[string]any, value any) bool {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return false
		}
		addNamedSource(out, rv.Interface())
		return true
	case reflect.Struct:
		addStructNamedValues(out, rv)
		return true
	default:
		return false
	}
}

func applyUpsert(s *persistedState, t *catalog.Table, data *tableData, incoming map[string]any, assigns []ast.Assignment, args []any) error {
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		conflict := false
		for _, col := range t.Columns {
			if (col.Unique || isSingleColumnPrimaryKey(t, col.Name)) && valuesEqual(row.Values[col.Name], incoming[col.Name], col.Collation) {
				conflict = true
				break
			}
		}
		if !conflict {
			for _, idx := range t.Indexes {
				if !idx.Unique || len(idx.Columns) == 0 {
					continue
				}
				newIncluded, err := partialIndexIncludesRow(s, idx, incoming)
				if err != nil {
					return err
				}
				if !newIncluded {
					continue
				}
				same := true
				for _, name := range idx.Columns {
					if incoming[name] == nil || !valuesEqual(row.Values[name], incoming[name], columnCollation(t, name)) {
						same = false
						break
					}
				}
				if same {
					conflict = true
					break
				}
			}
		}
		if !conflict {
			continue
		}
		next := cloneValues(row.Values)
		for _, asn := range assigns {
			col, _, ok := t.Column(asn.Column.Unquoted)
			if !ok {
				return fmt.Errorf("unknown column: %s", asn.Column.Unquoted)
			}
			scope := cloneValues(next)
			for k, v := range incoming {
				scope["excluded."+k] = v
			}
			v, err := evalExpr(asn.Value, scope, &binder{args: args})
			if err != nil {
				return err
			}
			cv, err := coerce(col, v, t.Strict)
			if err != nil {
				return err
			}
			next[col.Name] = cv
		}
		if err := validateRow(s, t, data, next, row.RowID); err != nil {
			return err
		}
		data.copyRow(i)
		row = &data.Rows[i]
		row.Values = next
		row.syncPositioned(t)
		data.rebuildRuntime(t)
		return nil
	}
	return errors.New("upsert conflict row not found")
}

func deleteReplaceConflicts(s *persistedState, t *catalog.Table, data *tableData, incoming map[string]any) {
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		if rowConflictsForReplace(t, row.Values, incoming) {
			data.copyRow(i)
			row = &data.Rows[i]
			row.Deleted = s.NextTx
		}
	}
}

func rowConflictsForReplace(t *catalog.Table, existing, incoming map[string]any) bool {
	for _, col := range t.Columns {
		if (col.Unique || isSingleColumnPrimaryKey(t, col.Name)) && incoming[col.Name] != nil && valuesEqual(existing[col.Name], incoming[col.Name], col.Collation) {
			return true
		}
	}
	for _, idx := range t.Indexes {
		if !idx.Unique || len(idx.Columns) == 0 {
			continue
		}
		same := true
		for _, col := range idx.Columns {
			if incoming[col] == nil || !valuesEqual(existing[col], incoming[col], columnCollation(t, col)) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

func execUpdate(s *persistedState, st *ast.UpdateStmt, args []any) (Result, error) {
	name, err := tableNameFromRefs(st.Tables)
	if err != nil {
		return Result{}, err
	}
	if vt, ok := hasVirtualTables(s, name); ok {
		return execVTabUpdate(s, st, vt)
	}
	insteadOf := insteadOfTrigger(s, name, "update")
	t, data, err := tableForWrite(s, name)
	if err != nil {
		if insteadOf {
			if view, ok := s.Catalog.View(name); ok {
				t = &catalog.Table{Name: view.Name, Columns: view.Columns}
				data = &tableData{}
			} else {
				return Result{}, err
			}
		} else {
			return Result{}, err
		}
	}
	if insteadOf {
		return execViewInsteadOfUpdate(s, st, name, args)
	}
	setParamCount := 0
	for _, asn := range st.Set {
		setParamCount += countParams(asn.Value)
	}
	var affected int64
	positions := mutationCandidatePositions(s, name, data, st.Where, args, setParamCount)
	changed := false
	runtimeTouched := updateTouchesRuntime(t, st.Set)
	updCols := changedColumns(st.Set)
	for _, i := range positions {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		row.ensureValues(t)
		whereBind := binder{args: args, pos: setParamCount}
		ok, err := matchWhereState(s, st.Where, row.Values, &whereBind)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			continue
		}
		next := cloneValues(row.Values)
		setBind := binder{args: args}
		for _, asn := range st.Set {
			col, _, ok := t.Column(asn.Column.Unquoted)
			if !ok {
				return Result{}, fmt.Errorf("unknown column: %s", asn.Column.Unquoted)
			}
			v, err := evalExpr(asn.Value, next, &setBind)
			if err != nil {
				return Result{}, err
			}
			cv, err := coerce(col, v, t.Strict)
			if err != nil {
				return Result{}, err
			}
			next[col.Name] = cv
		}
		if err := validateRow(s, t, data, next, row.RowID); err != nil {
			return Result{}, err
		}
		if s.Pragmas.ForeignKeys {
			if err := applyUpdateReferences(s, t.Name, row.Values, next); err != nil {
				return Result{}, err
			}
		}
		if insteadOf {
			if _, err := fireInsteadOf(s, t.Name, "update", row.Values, next); err != nil {
				return Result{}, err
			}
			continue
		}
		if proceed, err := beforeRowChange(s, t.Name, "update", updCols, row.Values, next); err != nil {
			return Result{}, err
		} else if !proceed {
			continue
		}
		oldValues := cloneValues(row.Values)
		data.copyRow(i)
		row = &data.Rows[i]
		row.Values = next
		row.syncPositioned(t)
		changed = true
		affected++
		if err := afterRowChange(s, t.Name, "update", updCols, oldValues, next); err != nil {
			return Result{}, err
		}
	}
	if changed && runtimeTouched {
		data.rebuildRuntime(t)
	}
	return Result{RowsAffected: affected}, nil
}

func updateTouchesRuntime(t *catalog.Table, assigns []ast.Assignment) bool {
	for _, asn := range assigns {
		name := catalog.Normalize(asn.Column.Unquoted)
		if pk := integerPrimaryKeyColumn(t); pk == name {
			return true
		}
		for _, col := range t.Columns {
			if col.Name == name && (col.PrimaryKey || col.Unique) {
				return true
			}
		}
		for _, idx := range t.Indexes {
			for _, idxCol := range idx.Columns {
				if idxCol == name {
					return true
				}
			}
		}
	}
	return false
}

func execDelete(s *persistedState, st *ast.DeleteStmt, args []any) (Result, error) {
	name, err := tableNameFromRefs(st.From)
	if err != nil {
		return Result{}, err
	}
	if vt, ok := hasVirtualTables(s, name); ok {
		return execVTabDelete(s, st, vt)
	}
	insteadOf := insteadOfTrigger(s, name, "delete")
	if insteadOf {
		return execViewInsteadOfDelete(s, st, name, args)
	}
	_, data, err := tableForWrite(s, name)
	if err != nil {
		if insteadOf {
			if view, ok := s.Catalog.View(name); ok {
				_ = view
				data = &tableData{}
			} else {
				return Result{}, err
			}
		} else {
			return Result{}, err
		}
	}
	bind := binder{args: args}
	var affected int64
	positions := mutationCandidatePositions(s, name, data, st.Where, args, 0)
	changed := false
	for _, i := range positions {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		row.ensureValues(s.Catalog.Tables[catalog.Normalize(name)])
		whereBind := bind
		ok, err := matchWhereState(s, st.Where, row.Values, &whereBind)
		if err != nil {
			return Result{}, err
		}
		if ok {
			if s.Pragmas.ForeignKeys {
				if err := validateDeleteReferences(s, name, row.Values); err != nil {
					return Result{}, err
				}
			}
			oldValues := cloneValues(row.Values)
			if insteadOf {
				if _, err := fireInsteadOf(s, name, "delete", oldValues, nil); err != nil {
					return Result{}, err
				}
				continue
			}
			if proceed, err := beforeRowChange(s, name, "delete", nil, oldValues, nil); err != nil {
				return Result{}, err
			} else if !proceed {
				continue
			}
			data.copyRow(i)
			row = &data.Rows[i]
			row.Deleted = s.NextTx
			changed = true
			affected++
			if err := afterRowChange(s, name, "delete", nil, oldValues, nil); err != nil {
				return Result{}, err
			}
		}
	}
	if changed {
		data.rebuildRuntime(s.Catalog.Tables[catalog.Normalize(name)])
	}
	return Result{RowsAffected: affected}, nil
}

func mutationCandidatePositions(s *persistedState, table string, data *tableData, where ast.Expr, args []any, argOffset int) []int {
	if argOffset == 0 {
		if positions := candidatePositions(s, table, data, where, args); positions != nil {
			return positions
		}
	}
	if argOffset > 0 {
		if argOffset <= len(args) {
			args = args[argOffset:]
			if positions := candidatePositions(s, table, data, where, args); positions != nil {
				return positions
			}
		}
	}
	positions := make([]int, len(data.Rows))
	for i := range data.Rows {
		positions[i] = i
	}
	return positions
}

func validateDeleteReferences(s *persistedState, table string, old map[string]any) error {
	refTable, ok := s.Catalog.Table(table)
	if !ok {
		return nil
	}
	for _, child := range s.Catalog.Tables {
		for _, fk := range child.ForeignKeys {
			if fk.RefTable != refTable.Name {
				continue
			}
			refCols := resolveFKColumns(refTable, fk.RefColumns)
			if len(refCols) != len(fk.Columns) {
				return errors.New("foreign key mismatch")
			}
			childData := s.Tables[child.Name]
			for i := range childData.Rows {
				row := &childData.Rows[i]
				if row.Deleted != 0 {
					continue
				}
				match := true
				for i, refCol := range refCols {
					match = match && reflect.DeepEqual(row.Values[fk.Columns[i]], old[refCol])
				}
				if match {
					childData.copyRow(i)
					row = &childData.Rows[i]
					switch fk.OnDelete {
					case "cascade":
						row.Deleted = s.NextTx
					case "set null":
						for _, col := range fk.Columns {
							row.Values[col] = nil
						}
						row.syncPositioned(child)
					case "set default":
						for _, col := range fk.Columns {
							if def, _, ok := child.Column(col); ok {
								row.Values[col] = def.Default
							}
						}
						row.syncPositioned(child)
						if err := validateRow(s, child, childData, row.Values, row.RowID); err != nil {
							return err
						}
					default:
						return errors.New("foreign key constraint failed")
					}
				}
			}
		}
	}
	return nil
}

func applyUpdateReferences(s *persistedState, table string, old, next map[string]any) error {
	refTable, ok := s.Catalog.Table(table)
	if !ok {
		return nil
	}
	for _, child := range s.Catalog.Tables {
		for _, fk := range child.ForeignKeys {
			if fk.RefTable != refTable.Name {
				continue
			}
			refCols := resolveFKColumns(refTable, fk.RefColumns)
			if len(refCols) != len(fk.Columns) {
				return errors.New("foreign key mismatch")
			}
			changed := false
			for _, refCol := range refCols {
				if !reflect.DeepEqual(old[refCol], next[refCol]) {
					changed = true
					break
				}
			}
			if !changed {
				continue
			}
			childData := s.Tables[child.Name]
			for i := range childData.Rows {
				row := &childData.Rows[i]
				if row.Deleted != 0 {
					continue
				}
				match := true
				for i, refCol := range refCols {
					match = match && reflect.DeepEqual(row.Values[fk.Columns[i]], old[refCol])
				}
				if !match {
					continue
				}
				childData.copyRow(i)
				row = &childData.Rows[i]
				switch fk.OnUpdate {
				case "cascade":
					for i, refCol := range refCols {
						row.Values[fk.Columns[i]] = next[refCol]
					}
					row.syncPositioned(child)
				case "set null":
					for _, col := range fk.Columns {
						row.Values[col] = nil
					}
					row.syncPositioned(child)
				case "set default":
					for _, col := range fk.Columns {
						if def, _, ok := child.Column(col); ok {
							row.Values[col] = def.Default
						}
					}
					row.syncPositioned(child)
					if err := validateRow(s, child, childData, row.Values, row.RowID); err != nil {
						return err
					}
				default:
					return errors.New("foreign key constraint failed")
				}
			}
		}
	}
	return nil
}

func execSelect(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, error) {
	if rows, ok, err := execSelectExt(s, st, args); ok || err != nil {
		return rows, err
	}
	if len(st.From) == 1 {
		if vt, simple, ok := vtabFromRef(s, st.From[0]); ok {
			// MATCH drives the row set directly and rank/bm25() need relevance
			// scores, so those go through the virtual-table path. Anything else
			// (aggregates, plain scans) uses the generic executor, which reads
			// the virtual table through buildVirtualRows.
			if usesFTSFeatures(st) {
				return execVTabSelect(s, st, vt, simple, args)
			}
		}
	}
	if len(st.From) == 1 {
		if name := refSimpleName(st.From[0]); catalog.Normalize(name) == "sqlite_schema" {
			return selectVirtualSchema(s, st, args)
		}
	}
	// A CTE shadows any physical table of the same name, so the catalog-driven
	// fast paths must not run while a WITH scope is active.
	if len(s.ctes) == 0 {
		if rows, ok, err := fastJoinAggregateSelect(s, st, args); ok || err != nil {
			return rows, err
		}
		if rows, ok, err := fastSingleTableSelect(s, st, args); ok || err != nil {
			return rows, err
		}
	}
	return execSelectGeneric(s, st, args)
}

func execSelectGeneric(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, error) {
	tables, contexts, err := buildRowSetForSelect(s, st.From, st.Where, args)
	if err != nil {
		return nil, err
	}
	bind := binder{args: args}
	cols, exprs, err := selectList(tables, st.Columns)
	if err != nil {
		return nil, err
	}
	winKeys, winCalls, err := collectWindowCalls(st, exprs)
	if err != nil {
		return nil, err
	}
	var filtered []rowContext
	for _, row := range contexts {
		whereBind := bind
		ok, err := matchWhereState(s, st.Where, row.Values, &whereBind)
		if err != nil {
			return nil, err
		}
		if ok {
			filtered = append(filtered, row)
		}
	}
	aggregated := len(st.GroupBy) > 0 || hasAggregate(exprs)
	if aggregated {
		auxExprs := collectNestedAggregates(winCalls)
		out, cols, extraOut, err := aggregateRows(s, contexts, st, cols, exprs, auxExprs, args)
		if err != nil {
			return nil, err
		}
		if len(winCalls) > 0 {
			base := make([]map[string]any, len(out))
			for i, r := range out {
				base[i] = rowFromOutput(cols, r)
			}
			var aux []map[ast.Expr]any
			if len(auxExprs) > 0 {
				aux = make([]map[ast.Expr]any, len(out))
				for i := range out {
					m := make(map[ast.Expr]any, len(auxExprs))
					for j, e := range auxExprs {
						if j < len(extraOut[i]) {
							m[e] = extraOut[i][j]
						}
					}
					aux[i] = m
				}
			}
			winVals, err := evalWindowValues(s, winCalls, base, args, aux)
			if err != nil {
				return nil, err
			}
			substituteWindowValues(out, exprs, winKeys, winVals)
		}
		if len(st.OrderBy) > 0 {
			sort.SliceStable(out, func(i, j int) bool {
				li := rowFromOutput(cols, out[i])
				lj := rowFromOutput(cols, out[j])
				for _, item := range st.OrderBy {
					lv, _ := evalExprState(s, item.Expr, li, &binder{args: args})
					rv, _ := evalExprState(s, item.Expr, lj, &binder{args: args})
					c, _ := compareValues(lv, rv, collationForExpr(tables, item.Expr))
					if c == 0 {
						continue
					}
					if item.Desc {
						return c > 0
					}
					return c < 0
				}
				return false
			})
		}
		if st.Distinct {
			out = distinctRowsKeepFirst(len(cols), out)
		}
		out, err = applyLimit(out, st.Limit, args, st)
		if err != nil {
			return nil, err
		}
		return isql.NewRows(cols, out), nil
	}

	var winVals map[ast.Expr][]any
	if len(winCalls) > 0 {
		base := make([]map[string]any, len(filtered))
		for i, r := range filtered {
			base[i] = r.Values
		}
		winVals, err = evalWindowValues(s, winCalls, base, args, nil)
		if err != nil {
			return nil, err
		}
	}

	// Projection runs first so that ORDER BY can resolve output column
	// ordinals and aliases, then rows are reordered together with their input
	// contexts so ORDER BY can also reference columns that are not selected.
	var out [][]any
	for i, row := range filtered {
		values := make([]any, 0, len(exprs))
		for _, expr := range exprs {
			var v any
			var err error
			if winVals != nil {
				v, err = evalExprWithWindows(s, expr, row.Values, winVals, i, &bind)
			} else {
				v, err = evalExprState(s, expr, row.Values, &bind)
			}
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		out = append(out, values)
	}
	if len(st.OrderBy) > 0 && len(out) > 1 {
		sortProjectedRows(s, st, tables, cols, out, filtered, winVals, args)
	}
	if st.Distinct {
		out = distinctRowsKeepFirst(len(cols), out)
	}
	out, err = applyLimit(out, st.Limit, args, st)
	if err != nil {
		return nil, err
	}
	return isql.NewRows(cols, out), nil
}

// substituteWindowValues overwrites window-function slots in already-projected
// rows. Window expressions are not visible to ordinary expression evaluation,
// so aggregateRows leaves those positions empty and they are filled here.
func substituteWindowValues(out [][]any, exprs, _ []ast.Expr, winVals map[ast.Expr][]any) {
	if len(winVals) == 0 {
		return
	}
	lookup := make(map[ast.Expr][]any, len(winVals))
	for k, v := range winVals {
		lookup[k] = v
	}
	for rowIdx := range out {
		for colIdx, expr := range exprs {
			if vals, ok := lookup[expr]; ok && rowIdx < len(vals) {
				if colIdx < len(out[rowIdx]) {
					out[rowIdx][colIdx] = vals[rowIdx]
				}
			}
		}
	}
}

// sortProjectedRows reorders projected rows by an ORDER BY clause. Terms may
// reference output ordinals (ORDER BY 1), output column names or aliases, or
// ordinary expressions over the input row including window results.
func sortProjectedRows(s *persistedState, st *ast.SelectStmt, tables []*catalog.Table, cols []string, out [][]any, filtered []rowContext, winVals map[ast.Expr][]any, args []any) {
	// out is indexed together with filtered; sort a permutation so that the
	// input contexts stay reachable for ORDER BY expressions.
	rank := map[string]int{}
	for i := range cols {
		rank[strings.ToLower(cols[i])] = i
	}
	termValue := func(item ast.OrderByItem, idx int) any {
		expr := item.Expr
		if lit, ok := expr.(*ast.Literal); ok {
			raw := strings.TrimSpace(string(lit.Raw))
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 1 && int(n) <= len(cols) {
				return out[idx][int(n)-1]
			}
		}
		if name := sortTermName(expr); name != "" {
			if c, ok := rank[name]; ok && c < len(out[idx]) {
				return out[idx][c]
			}
		}
		if winVals != nil {
			if v, err := evalExprWithWindows(s, expr, filtered[idx].Values, winVals, idx, &binder{args: args}); err == nil {
				return v
			}
		}
		v, _ := evalExprState(s, expr, filtered[idx].Values, &binder{args: args})
		return v
	}
	idxs := make([]int, len(out))
	for i := range idxs {
		idxs[i] = i
	}
	sort.SliceStable(idxs, func(i, j int) bool {
		a, b := idxs[i], idxs[j]
		for _, item := range st.OrderBy {
			lv := termValue(item, a)
			rv := termValue(item, b)
			c, _ := compareValues(lv, rv, collationForExpr(tables, item.Expr))
			if c == 0 {
				continue
			}
			if item.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	sortedOut := make([][]any, len(out))
	sortedIn := make([]rowContext, len(filtered))
	for i, src := range idxs {
		sortedOut[i] = out[src]
		if src < len(filtered) {
			sortedIn[i] = filtered[src]
		}
	}
	copy(out, sortedOut)
	copy(filtered, sortedIn)
}

// sortTermName returns the identifier an ORDER BY term names, if any.
func sortTermName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return strings.ToLower(e.Unquoted)
	case *ast.QualifiedIdent:
		if len(e.Parts) > 0 {
			return strings.ToLower(e.Parts[len(e.Parts)-1].Unquoted)
		}
	}
	return ""
}

func explain(st *ast.ExplainStmt) *Rows {
	return isql.NewRows([]string{"node", "detail"}, [][]any{{"parse", fmt.Sprintf("%T", st.Stmt)}, {"execute", "single-table v1 executor"}})
}

func fastSingleTableSelect(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, bool, error) {
	if len(st.From) != 1 || len(st.GroupBy) > 0 || st.Having != nil || st.SetOp != nil || hasAggregateSelect(st.Columns) {
		return nil, false, nil
	}
	if st.Distinct || hasWindowSelect(st) {
		return nil, false, nil
	}
	simple, ok := st.From[0].(*ast.SimpleTable)
	if !ok {
		return nil, false, nil
	}
	name := qname(simple.Name)
	if _, isVT := hasVirtualTables(s, name); isVT {
		return nil, false, nil
	}
	t, ok := s.Catalog.Table(name)
	if !ok {
		if _, viewOK := s.Catalog.View(name); viewOK {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("table does not exist: %s", name)
	}
	data := s.Tables[catalog.Normalize(name)]
	if data == nil {
		return isql.NewRows(nil, nil), true, nil
	}
	cols, projectors, ok := fastProjectors(t, st.Columns)
	if !ok {
		return nil, false, nil
	}
	if len(st.OrderBy) > 0 {
		return nil, false, nil
	}
	whereBind := &binder{args: args}
	predicate, ok, err := fastPredicate(t, st.Where, whereBind)
	if err != nil {
		return nil, true, err
	}
	if !ok {
		return nil, false, nil
	}
	positions := candidatePositions(s, name, data, st.Where, args)
	width := len(projectors)
	capHint := len(data.Rows)
	if positions != nil {
		capHint = len(positions)
	}
	out := make([]any, 0, capHint*width)
	if positions == nil {
		for _, row := range data.Rows {
			if row.Deleted != 0 || !predicate(row) {
				continue
			}
			for _, p := range projectors {
				out = append(out, p(row))
			}
		}
	} else {
		for _, pos := range positions {
			if pos < 0 || pos >= len(data.Rows) {
				continue
			}
			row := data.Rows[pos]
			if row.Deleted != 0 {
				continue
			}
			for _, p := range projectors {
				out = append(out, p(row))
			}
		}
	}
	out, err = applyFlatLimit(out, width, st.Limit, args, st)
	if err != nil {
		return nil, true, err
	}
	return isql.NewFlatRows(cols, out), true, nil
}

func fastJoinAggregateSelect(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, bool, error) {
	if len(args) != 0 || len(st.From) != 1 || st.Where != nil || st.Having != nil || st.Limit != nil || st.SetOp != nil {
		return nil, false, nil
	}
	if _, isVT := hasVirtualTables(s, refSimpleName(st.From[0])); isVT {
		return nil, false, nil
	}
	if st.Distinct || hasWindowSelect(st) {
		return nil, false, nil
	}
	if len(st.Columns) != 3 || len(st.GroupBy) != 1 {
		return nil, false, nil
	}
	join, ok := st.From[0].(*ast.JoinTable)
	if !ok || join.Kind != ast.InnerJoin {
		return nil, false, nil
	}
	leftRef, ok := join.Left.(*ast.SimpleTable)
	if !ok {
		return nil, false, nil
	}
	rightRef, ok := join.Right.(*ast.SimpleTable)
	if !ok {
		return nil, false, nil
	}
	leftName := qname(leftRef.Name)
	rightName := qname(rightRef.Name)
	on, ok := join.On.(*ast.BinaryExpr)
	if !ok || on.Op != lexer.EQ {
		return nil, false, nil
	}
	onLeft, ok := qualifiedColumn(on.Left)
	if !ok {
		return nil, false, nil
	}
	onRight, ok := qualifiedColumn(on.Right)
	if !ok {
		return nil, false, nil
	}
	groupRef, ok := qualifiedColumn(st.GroupBy[0])
	if !ok {
		return nil, false, nil
	}
	groupTable, groupCol := groupRef.table, groupRef.col
	selectGroupRef, ok := qualifiedColumn(st.Columns[0].Expr)
	if !ok || selectGroupRef.table != groupTable || selectGroupRef.col != groupCol {
		return nil, false, nil
	}
	countFn, ok := st.Columns[1].Expr.(*ast.FuncCall)
	if !ok || !countFn.Star || funcName(countFn) != "count" {
		return nil, false, nil
	}
	sumFn, ok := st.Columns[2].Expr.(*ast.FuncCall)
	if !ok || funcName(sumFn) != "sum" || len(sumFn.Args) != 1 {
		return nil, false, nil
	}
	sumRef, ok := qualifiedColumn(sumFn.Args[0])
	if !ok {
		return nil, false, nil
	}
	sumTable, sumCol := sumRef.table, sumRef.col
	if len(st.OrderBy) > 1 {
		return nil, false, nil
	}
	if len(st.OrderBy) == 1 {
		if st.OrderBy[0].NullsFirst != nil {
			return nil, false, nil
		}
		orderRef, ok := qualifiedColumn(st.OrderBy[0].Expr)
		if !ok || orderRef.table != groupTable || orderRef.col != groupCol {
			return nil, false, nil
		}
	}
	if !((onLeft.table == leftName && onRight.table == rightName) || (onLeft.table == rightName && onRight.table == leftName)) {
		return nil, false, nil
	}
	if groupTable != leftName && groupTable != rightName || sumTable != leftName && sumTable != rightName || groupTable == sumTable {
		return nil, false, nil
	}
	groupJoinCol := ""
	sumJoinCol := ""
	if onLeft.table == groupTable {
		groupJoinCol = onLeft.col
		sumJoinCol = onRight.col
	} else if onRight.table == groupTable {
		groupJoinCol = onRight.col
		sumJoinCol = onLeft.col
	} else {
		return nil, false, nil
	}
	groupData := s.Tables[groupTable]
	sumData := s.Tables[sumTable]
	if groupData == nil || sumData == nil {
		return isql.NewRows([]string{groupCol, "expr", "expr"}, nil), true, nil
	}
	groupSchema := s.Catalog.Tables[groupTable]
	sumSchema := s.Catalog.Tables[sumTable]
	if groupSchema == nil || sumSchema == nil {
		return nil, false, nil
	}
	_, groupJoinPos, ok := groupSchema.Column(groupJoinCol)
	if !ok {
		return nil, false, nil
	}
	_, groupValuePos, ok := groupSchema.Column(groupCol)
	if !ok {
		return nil, false, nil
	}
	_, sumJoinPos, ok := sumSchema.Column(sumJoinCol)
	if !ok {
		return nil, false, nil
	}
	_, sumValuePos, ok := sumSchema.Column(sumCol)
	if !ok {
		return nil, false, nil
	}
	groupValueByJoin := make(map[any]any, len(groupData.Rows))
	for _, row := range groupData.Rows {
		if row.Deleted != 0 {
			continue
		}
		joinValue, exists := row.positionalAt(groupJoinPos)
		if !exists {
			joinValue = row.Values[groupJoinCol]
		}
		if joinValue == nil {
			continue
		}
		key, ok := comparableKey(joinValue)
		if !ok {
			return nil, false, nil
		}
		groupValue, exists := row.positionalAt(groupValuePos)
		if !exists {
			groupValue = row.Values[groupCol]
		}
		groupValueByJoin[key] = groupValue
	}
	type aggregate struct {
		key      any
		count    int64
		sumCount int64
		sumFloat float64
		sumInt   int64
		allInt   bool
		overflow bool
	}
	byGroup := make(map[any]aggregate, len(groupValueByJoin))
	for _, row := range sumData.Rows {
		if row.Deleted != 0 {
			continue
		}
		joinValue, exists := row.positionalAt(sumJoinPos)
		if !exists {
			joinValue = row.Values[sumJoinCol]
		}
		if joinValue == nil {
			continue
		}
		joinKey, ok := comparableKey(joinValue)
		if !ok {
			return nil, false, nil
		}
		groupValue, ok := groupValueByJoin[joinKey]
		if !ok {
			continue
		}
		key, ok := comparableKey(groupValue)
		if !ok {
			return nil, false, nil
		}
		agg := byGroup[key]
		if agg.count == 0 {
			agg.key = groupValue
		}
		agg.count++
		scoreValue, exists := row.positionalAt(sumValuePos)
		if !exists {
			scoreValue = row.Values[sumCol]
		}
		if integer, ok := aggregateIntegerValue(scoreValue); ok {
			if agg.sumCount == 0 {
				agg.allInt = true
			}
			agg.sumCount++
			if agg.allInt {
				if (integer > 0 && agg.sumInt > math.MaxInt64-integer) || (integer < 0 && agg.sumInt < math.MinInt64-integer) {
					agg.overflow = true
				} else {
					agg.sumInt += integer
				}
			}
			agg.sumFloat += float64(integer)
		} else if score, ok := sumFloatValue(scoreValue); ok {
			if agg.sumCount == 0 {
				agg.allInt = false
			}
			agg.sumCount++
			agg.allInt = false
			agg.sumFloat += score
		}
		byGroup[key] = agg
	}
	for _, agg := range byGroup {
		if agg.overflow {
			return nil, true, errors.New("integer overflow")
		}
	}
	groups := make([]aggregate, 0, len(byGroup))
	for _, agg := range byGroup {
		groups = append(groups, agg)
	}
	groupCollation := columnCollation(groupSchema, groupCol)
	descending := len(st.OrderBy) == 1 && st.OrderBy[0].Desc
	sort.Slice(groups, func(i, j int) bool {
		c, ok := compareGroupOrderValues(groups[i].key, groups[j].key, groupCollation)
		if !ok {
			return fmt.Sprint(groups[i].key) < fmt.Sprint(groups[j].key)
		}
		if descending {
			return c > 0
		}
		return c < 0
	})
	out := make([]any, 0, len(groups)*3)
	for _, agg := range groups {
		var sum any
		if agg.sumCount > 0 {
			if agg.allInt {
				sum = agg.sumInt
			} else {
				sum = agg.sumFloat
			}
		}
		out = append(out, agg.key, agg.count, sum)
	}
	return isql.NewFlatRows([]string{groupCol, "expr", "expr"}, out), true, nil
}

func compareGroupOrderValues(left, right any, collation string) (int, bool) {
	leftClass, rightClass := sqliteOrderClass(left), sqliteOrderClass(right)
	if leftClass >= 0 && rightClass >= 0 && leftClass != rightClass {
		return leftClass - rightClass, true
	}
	leftText, leftIsText := left.(string)
	rightText, rightIsText := right.(string)
	if leftIsText && rightIsText {
		switch collation {
		case "NOCASE":
			return compareNoCaseASCII(leftText, rightText), true
		case "RTRIM":
			leftText = strings.TrimRight(leftText, " ")
			rightText = strings.TrimRight(rightText, " ")
		}
		return strings.Compare(leftText, rightText), true
	}
	leftBlob, leftIsBlob := left.([]byte)
	rightBlob, rightIsBlob := right.([]byte)
	if leftIsBlob && rightIsBlob {
		return bytes.Compare(leftBlob, rightBlob), true
	}
	if leftClass < 0 || rightClass < 0 {
		return compareValues(left, right, collation)
	}
	return compareValues(left, right, collation)
}

func aggregateIntegerValue(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	default:
		if !isIntegerValue(value) {
			return 0, false
		}
		return asInt(value)
	}
}

func sumFloatValue(value any) (float64, bool) {
	switch v := value.(type) {
	case string:
		parsed, _ := sqliteNumericPrefix(v)
		return parsed, true
	case []byte:
		parsed, _ := sqliteNumericPrefix(string(v))
		return parsed, true
	default:
		return asFloat(value)
	}
}

func sqliteNumericPrefix(value string) (float64, bool) {
	value = strings.TrimLeft(value, " \t\n\r\f\v")
	if value == "" {
		return 0, false
	}
	pos := 0
	if value[pos] == '+' || value[pos] == '-' {
		pos++
	}
	digits := 0
	for pos < len(value) && value[pos] >= '0' && value[pos] <= '9' {
		pos++
		digits++
	}
	if pos < len(value) && value[pos] == '.' {
		pos++
		for pos < len(value) && value[pos] >= '0' && value[pos] <= '9' {
			pos++
			digits++
		}
	}
	if digits == 0 {
		return 0, false
	}
	mantissaEnd := pos
	if pos < len(value) && (value[pos] == 'e' || value[pos] == 'E') {
		exponent := pos + 1
		if exponent < len(value) && (value[exponent] == '+' || value[exponent] == '-') {
			exponent++
		}
		exponentDigits := exponent
		for exponent < len(value) && value[exponent] >= '0' && value[exponent] <= '9' {
			exponent++
		}
		if exponent > exponentDigits {
			pos = exponent
		} else {
			pos = mantissaEnd
		}
	}
	parsed, err := strconv.ParseFloat(value[:pos], 64)
	return parsed, err == nil
}

func sqliteOrderClass(value any) int {
	switch value.(type) {
	case nil:
		return 0
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, bool, json.Number:
		return 1
	case string:
		return 2
	case []byte:
		return 3
	default:
		return -1
	}
}

func compareNoCaseASCII(left, right string) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		l, r := left[i], right[i]
		if l >= 'A' && l <= 'Z' {
			l += 'a' - 'A'
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if l < r {
			return -1
		}
		if l > r {
			return 1
		}
	}
	return len(left) - len(right)
}

func comparableKey(v any) (any, bool) {
	switch x := v.(type) {
	case nil, string:
		return x, true
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case bool:
		if x {
			return int64(1), true
		}
		return int64(0), true
	case float32:
		return comparableFloatKey(float64(x)), true
	case float64:
		return comparableFloatKey(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
		if f, err := x.Float64(); err == nil {
			return comparableFloatKey(f), true
		}
		return nil, false
	default:
		return nil, false
	}
}

func comparableFloatKey(value float64) any {
	const int64UpperExclusive = 9223372036854775808.0
	const int64LowerInclusive = -9223372036854775808.0
	if !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) && value >= int64LowerInclusive && value < int64UpperExclusive {
		return int64(value)
	}
	return value
}

func fastProjectors(t *catalog.Table, cols []ast.SelectColumn) ([]string, []func(rowVersion) any, bool) {
	var names []string
	var projectors []func(rowVersion) any
	for _, sel := range cols {
		if sel.Star {
			for idx, col := range t.Columns {
				c := col.Name
				colIdx := idx
				names = append(names, c)
				projectors = append(projectors, func(row rowVersion) any {
					if v, ok := row.positionalAt(colIdx); ok {
						return v
					}
					return row.Values[c]
				})
			}
			continue
		}
		col := exprColumnName(sel.Expr)
		if col == "" {
			return nil, nil, false
		}
		name := col
		if sel.Alias != nil {
			name = sel.Alias.Unquoted
		}
		if col == "rowid" || col == "_rowid_" || col == "oid" {
			names = append(names, name)
			projectors = append(projectors, func(row rowVersion) any { return row.RowID })
			continue
		}
		if _, idx, ok := t.Column(col); !ok {
			return nil, nil, false
		} else {
			c := col
			colIdx := idx
			names = append(names, name)
			projectors = append(projectors, func(row rowVersion) any {
				if v, ok := row.positionalAt(colIdx); ok {
					return v
				}
				return row.Values[c]
			})
			continue
		}
	}
	return names, projectors, true
}

func fastPredicate(t *catalog.Table, expr ast.Expr, bind *binder) (func(rowVersion) bool, bool, error) {
	if expr == nil {
		return func(rowVersion) bool { return true }, true, nil
	}
	be, ok := expr.(*ast.BinaryExpr)
	if !ok {
		return nil, false, nil
	}
	switch be.Op {
	case lexer.AND, lexer.OR:
		left, ok, err := fastPredicate(t, be.Left, bind)
		if err != nil || !ok {
			return nil, ok, err
		}
		right, ok, err := fastPredicate(t, be.Right, bind)
		if err != nil || !ok {
			return nil, ok, err
		}
		if be.Op == lexer.AND {
			return func(row rowVersion) bool { return left(row) && right(row) }, true, nil
		}
		return func(row rowVersion) bool { return left(row) || right(row) }, true, nil
	}

	leftCol := exprColumnName(be.Left)
	rightCol := exprColumnName(be.Right)
	if leftCol == "" && rightCol == "" || leftCol != "" && rightCol != "" {
		return nil, false, nil
	}
	col := leftCol
	valueExpr := be.Right
	columnOnLeft := true
	if col == "" {
		col = rightCol
		valueExpr = be.Left
		columnOnLeft = false
	}
	colIdx := -1
	if col != "rowid" && col != "_rowid_" && col != "oid" {
		if _, idx, ok := t.Column(col); ok {
			colIdx = idx
		}
	}
	val, err := evalExpr(valueExpr, nil, bind)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported expression") {
			return nil, false, nil
		}
		return nil, true, err
	}
	return func(row rowVersion) bool {
		rv := any(nil)
		if col == "rowid" || col == "_rowid_" || col == "oid" {
			rv = row.RowID
		} else if v, ok := row.positionalAt(colIdx); ok {
			rv = v
		} else {
			rv = row.value(t, col)
		}
		left, right := rv, val
		if !columnOnLeft {
			left, right = val, rv
		}
		c, ok := compareValues(left, right, "BINARY")
		if !ok {
			return false
		}
		switch be.Op {
		case lexer.EQ:
			return c == 0
		case lexer.NEQ:
			return c != 0
		case lexer.LT:
			return c < 0
		case lexer.LTE:
			return c <= 0
		case lexer.GT:
			return c > 0
		case lexer.GTE:
			return c >= 0
		default:
			return false
		}
	}, true, nil
}

func candidatePositions(s *persistedState, table string, data *tableData, where ast.Expr, args []any) []int {
	if data.runtimeDeferred() {
		return nil
	}
	col, val, ok, err := indexedEqualityPredicate(where, args)
	if err != nil {
		return nil
	}
	if !ok {
		if rcol, start, end, rok, rerr := indexedRangePredicate(where, args); rerr == nil && rok {
			return rangeCandidatePositions(s, table, data, rcol, start, end)
		}
		return nil
	}
	t, _ := s.Catalog.Table(table)
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		id, ok := asInt(val)
		if !ok {
			return []int{}
		}
		pos, ok := data.lookupRowID(id)
		if !ok {
			return []int{}
		}
		return []int{pos}
	}
	idxName := usableIndexForColumn(s, table, col)
	if idxName == "" {
		return nil
	}
	idx := catalog.Index{Name: idxName, Columns: []string{col}}
	return data.lookupIndexPositions(idxName, runtimeIndexLookupKey(idx, val))
}

type rangeBound struct {
	value     any
	inclusive bool
}

func rangeCandidatePositions(s *persistedState, table string, data *tableData, col string, start, end *rangeBound) []int {
	t, _ := s.Catalog.Table(table)
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		lo, hi, ok := rowidRangeBounds(start, end)
		if !ok {
			return []int{}
		}
		return data.rangeRowIDs(lo, hi)
	}
	idxName := usableIndexForColumn(s, table, col)
	if idxName == "" {
		return nil
	}
	startKey, endKey := indexRangeKeys(start, end)
	return data.rangeIndexPositions(idxName, startKey, endKey)
}

func rowidRangeBounds(start, end *rangeBound) (*int64, *int64, bool) {
	var lo, hi *int64
	if start != nil {
		v, ok := asInt(start.value)
		if !ok {
			return nil, nil, false
		}
		if !start.inclusive {
			v++
		}
		lo = &v
	}
	if end != nil {
		v, ok := asInt(end.value)
		if !ok {
			return nil, nil, false
		}
		if end.inclusive {
			v++
		}
		hi = &v
	}
	return lo, hi, true
}

func indexRangeKeys(start, end *rangeBound) ([]byte, []byte) {
	var startKey, endKey []byte
	if start != nil {
		startKey = []byte(runtimeValueKey(start.value))
	}
	if end != nil {
		if end.inclusive {
			endKey = append([]byte(runtimeValueKey(end.value)), 0xff)
		} else {
			endKey = []byte(runtimeValueKey(end.value))
		}
	}
	return startKey, endKey
}

func runtimeIndexLookupKey(idx catalog.Index, val any) string {
	if len(idx.Columns) == 1 {
		return runtimeValueKey(val)
	}
	return indexRuntimeKey(idx, map[string]any{idx.Columns[0]: val})
}

func runtimeIndexLookupKeyBytes(idx catalog.Index, val any) []byte {
	if len(idx.Columns) != 1 {
		return []byte(runtimeIndexLookupKey(idx, val))
	}
	switch x := val.(type) {
	case string:
		key := make([]byte, 0, len(x)+2)
		key = append(key, 's', ':')
		key = append(key, x...)
		return key
	case int64:
		key := make([]byte, 0, 10)
		key = append(key, 'i', ':')
		return append(key, encodeInt64Key(x)...)
	case int:
		key := make([]byte, 0, 10)
		key = append(key, 'i', ':')
		return append(key, encodeInt64Key(int64(x))...)
	default:
		return []byte(runtimeValueKey(val))
	}
}

func hasAggregateSelect(cols []ast.SelectColumn) bool {
	for _, col := range cols {
		if _, ok := col.Expr.(*ast.FuncCall); ok {
			return true
		}
	}
	return false
}

func queryPlan(s *persistedState, query string) (*Rows, bool) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	if !strings.HasPrefix(strings.ToLower(q), "explain query plan ") {
		return nil, false
	}
	parseMu.Lock()
	defer parseMu.Unlock()
	inner := strings.TrimSpace(q[len("explain query plan "):])
	stmt, err := parseStatementCached(inner)
	if err != nil {
		return isql.NewRows([]string{"id", "parent", "notused", "detail"}, [][]any{{int64(0), int64(0), int64(0), "PARSE ERROR"}}), true
	}
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		return isql.NewRows([]string{"id", "parent", "notused", "detail"}, [][]any{{int64(0), int64(0), int64(0), "EXECUTE " + fmt.Sprintf("%T", stmt)}}), true
	}
	var rows [][]any
	var walk func(ast.TableRef)
	walk = func(ref ast.TableRef) {
		switch r := ref.(type) {
		case *ast.SimpleTable:
			name := qname(r.Name)
			detail := "SCAN " + name
			if idx, primary := usableAccessForWhere(s, name, sel.Where); idx != "" {
				if primary {
					detail = "SEARCH " + name + " USING INTEGER PRIMARY KEY"
				} else if coveringIndexForSelect(s, name, idx, sel.Columns) {
					detail = "SEARCH " + name + " USING COVERING INDEX " + idx
				} else {
					detail = "SEARCH " + name + " USING INDEX " + idx
				}
			}
			rows = append(rows, []any{int64(len(rows)), int64(0), int64(0), detail})
		case *ast.JoinTable:
			rows = append(rows, []any{int64(len(rows)), int64(0), int64(0), "NESTED LOOP JOIN"})
			walk(r.Left)
			walk(r.Right)
		}
	}
	for _, ref := range sel.From {
		walk(ref)
	}
	if len(sel.OrderBy) > 0 && !orderByUsesIndex(s, sel) {
		rows = append(rows, []any{int64(len(rows)), int64(0), int64(0), "USE TEMP B-TREE FOR ORDER BY"})
	}
	if len(sel.GroupBy) > 0 {
		rows = append(rows, []any{int64(len(rows)), int64(0), int64(0), "USE TEMP B-TREE FOR GROUP BY"})
	}
	return isql.NewRows([]string{"id", "parent", "notused", "detail"}, rows), true
}

func coveringIndexForSelect(s *persistedState, table, index string, cols []ast.SelectColumn) bool {
	t, ok := s.Catalog.Table(table)
	if !ok {
		return false
	}
	var idx *catalog.Index
	for i := range t.Indexes {
		if t.Indexes[i].Name == index {
			idx = &t.Indexes[i]
			break
		}
	}
	if idx == nil || len(idx.Columns) == 0 {
		return false
	}
	indexCols := make(map[string]struct{}, len(idx.Columns))
	for _, col := range idx.Columns {
		indexCols[col] = struct{}{}
	}
	for _, sel := range cols {
		if sel.Star {
			return false
		}
		col := exprColumnName(sel.Expr)
		if col == "" {
			return false
		}
		if col == "rowid" || col == "_rowid_" || col == "oid" {
			if integerPrimaryKeyColumn(t) != "" {
				continue
			}
			return false
		}
		if _, ok := indexCols[col]; !ok {
			return false
		}
	}
	return len(cols) > 0
}

func orderByUsesIndex(s *persistedState, sel *ast.SelectStmt) bool {
	if sel == nil || len(sel.From) != 1 || len(sel.OrderBy) != 1 {
		return false
	}
	simple, ok := sel.From[0].(*ast.SimpleTable)
	if !ok {
		return false
	}
	table := qname(simple.Name)
	col := exprColumnName(sel.OrderBy[0].Expr)
	if col == "" {
		return false
	}
	if _, primary := usableAccessForColumn(s, table, col); primary {
		return true
	}
	if usableIndexForColumn(s, table, col) == "" {
		return false
	}
	if sel.Where == nil {
		return true
	}
	rcol, _, _, rok, _ := indexedRangePredicate(sel.Where, nil)
	if rok {
		return rcol == col
	}
	ecol, _, eok, _ := indexedEqualityPredicate(sel.Where, nil)
	return eok && ecol == col
}

func usableAccessForWhere(s *persistedState, table string, expr ast.Expr) (string, bool) {
	if expr == nil {
		return "", false
	}
	if col, _, _, ok, err := indexedRangePredicate(expr, nil); err == nil && ok {
		return usableAccessForColumn(s, table, col)
	}
	if be, ok := expr.(*ast.BinaryExpr); ok {
		if col := exprColumnName(be.Left); col != "" {
			return usableAccessForColumn(s, table, col)
		}
		if col := exprColumnName(be.Right); col != "" {
			return usableAccessForColumn(s, table, col)
		}
	}
	return "", false
}

func execPragma(s *persistedState, query string) (Result, bool, error) {
	name, value, ok := parsePragma(query)
	if !ok || value == "" {
		return Result{}, ok, nil
	}
	switch name {
	case "foreign_keys":
		s.Pragmas.ForeignKeys = value == "1" || strings.EqualFold(value, "on") || strings.EqualFold(value, "true")
	case "journal_mode":
		s.Pragmas.JournalMode = strings.ToLower(value)
	case "synchronous":
		s.Pragmas.Synchronous = strings.ToLower(value)
	case "user_version":
		i, _ := strconv.ParseInt(value, 10, 64)
		s.Pragmas.UserVersion = i
	case "cache_size":
		i, _ := strconv.Atoi(value)
		s.Pragmas.CacheSize = i
	case "page_size":
		i, _ := strconv.Atoi(value)
		s.Pragmas.PageSize = i
	default:
		return Result{}, true, nil
	}
	return Result{}, true, nil
}

func execMaintenance(s *persistedState, query string) (Result, bool, error) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	if q == "" {
		return Result{}, false, nil
	}
	first, _ := nextSQLField(q)
	switch strings.ToLower(first) {
	case "vacuum", "analyze":
		return Result{}, true, nil
	case "reindex":
		s.rebuildRuntime()
		return Result{}, true, nil
	default:
		return Result{}, false, nil
	}
}

func queryPragma(s *persistedState, query string) (*Rows, bool, error) {
	name, value, ok := parsePragma(query)
	if !ok || value != "" {
		return nil, ok, nil
	}
	switch name {
	case "integrity_check", "quick_check":
		check := "ok"
		for _, table := range s.Catalog.Tables {
			data := s.Tables[table.Name]
			if data == nil {
				continue
			}
			for _, row := range data.Rows {
				if row.Deleted != 0 {
					continue
				}
				if err := validateRow(s, table, data, row.Values, row.RowID); err != nil {
					check = err.Error()
					break
				}
			}
			if check != "ok" {
				break
			}
		}
		return isql.NewRows([]string{name}, [][]any{{check}}), true, nil
	case "foreign_keys":
		if s.Pragmas.ForeignKeys {
			return isql.NewRows([]string{"foreign_keys"}, [][]any{{int64(1)}}), true, nil
		}
		return isql.NewRows([]string{"foreign_keys"}, [][]any{{int64(0)}}), true, nil
	case "journal_mode":
		return isql.NewRows([]string{"journal_mode"}, [][]any{{s.Pragmas.JournalMode}}), true, nil
	case "synchronous":
		return isql.NewRows([]string{"synchronous"}, [][]any{{s.Pragmas.Synchronous}}), true, nil
	case "schema_version":
		return isql.NewRows([]string{"schema_version"}, [][]any{{s.Pragmas.SchemaVer}}), true, nil
	case "user_version":
		return isql.NewRows([]string{"user_version"}, [][]any{{s.Pragmas.UserVersion}}), true, nil
	case "page_size":
		return isql.NewRows([]string{"page_size"}, [][]any{{int64(s.Pragmas.PageSize)}}), true, nil
	case "cache_size":
		return isql.NewRows([]string{"cache_size"}, [][]any{{int64(s.Pragmas.CacheSize)}}), true, nil
	default:
		if strings.HasPrefix(name, "table_info(") {
			table := strings.TrimSuffix(strings.TrimPrefix(name, "table_info("), ")")
			return pragmaTableInfo(s, strings.Trim(table, `'"`)), true, nil
		}
		if strings.HasPrefix(name, "index_list(") {
			table := strings.TrimSuffix(strings.TrimPrefix(name, "index_list("), ")")
			return pragmaIndexList(s, strings.Trim(table, `'"`)), true, nil
		}
		if strings.HasPrefix(name, "index_info(") {
			index := strings.TrimSuffix(strings.TrimPrefix(name, "index_info("), ")")
			return pragmaIndexInfo(s, strings.Trim(index, `'"`)), true, nil
		}
		if strings.HasPrefix(name, "foreign_key_list(") {
			table := strings.TrimSuffix(strings.TrimPrefix(name, "foreign_key_list("), ")")
			return pragmaForeignKeyList(s, strings.Trim(table, `'"`)), true, nil
		}
		return isql.NewRows([]string{name}, nil), true, nil
	}
}

func parsePragma(query string) (name, value string, ok bool) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	if !strings.HasPrefix(strings.ToLower(q), "pragma ") {
		return "", "", false
	}
	body := strings.TrimSpace(q[len("pragma "):])
	if i := strings.IndexAny(body, "=("); i >= 0 {
		if body[i] == '(' {
			return strings.ToLower(strings.TrimSpace(body)), "", true
		}
		name = strings.ToLower(strings.TrimSpace(body[:i]))
		value = strings.Trim(strings.TrimSpace(body[i+1:]), `'"`)
		return name, value, true
	}
	return strings.ToLower(body), "", true
}

func stripSQLiteStrict(query string) (string, bool) {
	q := strings.TrimSpace(query)
	semi := strings.HasSuffix(q, ";")
	if semi {
		q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	}
	upper := strings.ToUpper(q)
	if !strings.HasSuffix(upper, " STRICT") {
		if semi {
			return q + ";", false
		}
		return q, false
	}
	stripped := strings.TrimSpace(q[:len(q)-len(" STRICT")])
	if semi {
		stripped += ";"
	}
	return stripped, true
}

func normalizeInsertOrReplace(query string) string {
	q := strings.TrimSpace(query)
	prefix := "insert or replace into "
	if len(q) >= len(prefix) && strings.EqualFold(q[:len(prefix)], prefix) {
		return "REPLACE INTO " + q[len(prefix):]
	}
	return query
}

func formatDefaultSQL(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		return fmt.Sprint(x)
	}
}

func pragmaTableInfo(s *persistedState, table string) *Rows {
	t, ok := s.Catalog.Table(table)
	if !ok {
		if v, viewOK := s.Catalog.View(table); viewOK {
			var rows [][]any
			for i, col := range v.Columns {
				rows = append(rows, []any{int64(i), col.Name, col.DeclType, int64(0), nil, int64(0)})
			}
			return isql.NewRows([]string{"cid", "name", "type", "notnull", "dflt_value", "pk"}, rows)
		}
		return isql.NewRows([]string{"cid", "name", "type", "notnull", "dflt_value", "pk"}, nil)
	}
	var rows [][]any
	for i, col := range t.Columns {
		notnull := int64(0)
		if col.NotNull && !col.PrimaryKey {
			notnull = 1
		}
		pk := int64(0)
		if col.PrimaryKey {
			pk = 1
		}
		var dflt any
		if col.Default != nil {
			dflt = col.DefaultSQL
		}
		rows = append(rows, []any{int64(i), col.Name, col.DeclType, notnull, dflt, pk})
	}
	return isql.NewRows([]string{"cid", "name", "type", "notnull", "dflt_value", "pk"}, rows)
}

func pragmaIndexList(s *persistedState, table string) *Rows {
	t, ok := s.Catalog.Table(table)
	if !ok {
		return isql.NewRows([]string{"seq", "name", "unique", "origin", "partial"}, nil)
	}
	var rows [][]any
	for i, idx := range t.Indexes {
		unique := int64(0)
		if idx.Unique {
			unique = 1
		}
		partial := int64(0)
		if idx.Partial {
			partial = 1
		}
		rows = append(rows, []any{int64(i), idx.Name, unique, "c", partial})
	}
	return isql.NewRows([]string{"seq", "name", "unique", "origin", "partial"}, rows)
}

func pragmaIndexInfo(s *persistedState, index string) *Rows {
	cols := []string{"seqno", "cid", "name"}
	for _, t := range s.Catalog.Tables {
		for _, idx := range t.Indexes {
			if idx.Name != catalog.Normalize(index) && idx.Name != index {
				continue
			}
			rows := make([][]any, 0, len(idx.Columns))
			for seq, name := range idx.Columns {
				cid := int64(-1)
				if _, i, ok := t.Column(name); ok {
					cid = int64(i)
				}
				rows = append(rows, []any{int64(seq), cid, name})
			}
			return isql.NewRows(cols, rows)
		}
	}
	return isql.NewRows(cols, nil)
}

func pragmaForeignKeyList(s *persistedState, table string) *Rows {
	t, ok := s.Catalog.Table(table)
	cols := []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"}
	if !ok {
		return isql.NewRows(cols, nil)
	}
	var rows [][]any
	for i, fk := range t.ForeignKeys {
		refCols := resolveFKColumnsForPragma(fk.RefColumns)
		if parent, ok := s.Catalog.Table(fk.RefTable); ok {
			refCols = resolveFKColumns(parent, fk.RefColumns)
		}
		for j, col := range fk.Columns {
			to := ""
			if j < len(refCols) {
				to = refCols[j]
			}
			rows = append(rows, []any{int64(i), int64(j), fk.RefTable, col, to, sqliteActionName(fk.OnUpdate), sqliteActionName(fk.OnDelete), "NONE"})
		}
	}
	return isql.NewRows(cols, rows)
}

func resolveFKColumnsForPragma(cols []string) []string {
	out := append([]string(nil), cols...)
	for i, col := range out {
		if col == primaryKeyNamePlaceholder {
			out[i] = ""
		}
	}
	return out
}

func sqliteActionName(action string) string {
	switch action {
	case "cascade":
		return "CASCADE"
	case "set null":
		return "SET NULL"
	case "set default":
		return "SET DEFAULT"
	case "restrict":
		return "RESTRICT"
	default:
		return "NO ACTION"
	}
}

func selectVirtualSchema(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, error) {
	var contexts []rowContext
	for _, t := range s.Catalog.Tables {
		sql := t.SQL
		if sql == "" {
			sql = "CREATE TABLE " + t.Name
		}
		contexts = append(contexts, rowContext{Values: map[string]any{
			"type": "table", "name": t.Name, "tbl_name": t.Name, "rootpage": int64(0), "sql": sql,
		}})
		for _, idx := range t.Indexes {
			sql := any(idx.SQL)
			if idx.SQL == "" {
				sql = nil
			}
			contexts = append(contexts, rowContext{Values: map[string]any{
				"type": "index", "name": idx.Name, "tbl_name": t.Name, "rootpage": int64(0), "sql": sql,
			}})
		}
	}
	for _, v := range s.Catalog.Views {
		sql := v.SQL
		if sql == "" {
			sql = "CREATE VIEW " + v.Name + " AS " + v.SelectSQL
		}
		contexts = append(contexts, rowContext{Values: map[string]any{
			"type": "view", "name": v.Name, "tbl_name": v.Name, "rootpage": int64(0), "sql": sql,
		}})
	}
	cols, exprs, err := selectList([]*catalog.Table{{Columns: []catalog.Column{{Name: "type"}, {Name: "name"}, {Name: "tbl_name"}, {Name: "rootpage"}, {Name: "sql"}}}}, st.Columns)
	if err != nil {
		return nil, err
	}
	var out [][]any
	for _, row := range contexts {
		ok, err := matchWhereState(s, st.Where, row.Values, &binder{args: args})
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var vals []any
		for _, expr := range exprs {
			v, err := evalExpr(expr, row.Values, &binder{args: args})
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
		out = append(out, vals)
	}
	return isql.NewRows(cols, out), nil
}

type rowContext struct {
	Values map[string]any
}

func buildRowSet(s *persistedState, refs []ast.TableRef) ([]*catalog.Table, []rowContext, error) {
	if len(refs) == 0 {
		return nil, []rowContext{{Values: map[string]any{}}}, nil
	}
	var tables []*catalog.Table
	rows := []rowContext{{Values: map[string]any{}}}
	for _, ref := range refs {
		ts, rs, err := buildRefRows(s, ref)
		if err != nil {
			return nil, nil, err
		}
		tables = append(tables, ts...)
		rows = crossRows(rows, rs)
	}
	return tables, rows, nil
}

func buildRowSetForSelect(s *persistedState, refs []ast.TableRef, where ast.Expr, args []any) ([]*catalog.Table, []rowContext, error) {
	if len(refs) == 1 {
		if simple, ok := refs[0].(*ast.SimpleTable); ok {
			name := qname(simple.Name)
			_, isCTE := s.ctes[catalog.Normalize(name)]
			if col, val, ok, err := indexedEqualityPredicate(where, args); err != nil {
				return nil, nil, err
			} else if ok && !isCTE {
				if rows, used, err := buildIndexedRows(s, simple, col, val); used || err != nil {
					if err != nil {
						return nil, nil, err
					}
					t, _ := s.Catalog.Table(name)
					return []*catalog.Table{t}, rows, nil
				}
			} else if col, start, end, ok, err := indexedRangePredicate(where, args); err != nil {
				return nil, nil, err
			} else if ok && !isCTE {
				if rows, used, err := buildRangeRows(s, simple, col, start, end); used || err != nil {
					if err != nil {
						return nil, nil, err
					}
					t, _ := s.Catalog.Table(name)
					return []*catalog.Table{t}, rows, nil
				}
			}
		}
	}
	return buildRowSet(s, refs)
}

func buildRangeRows(s *persistedState, simple *ast.SimpleTable, col string, start, end *rangeBound) ([]rowContext, bool, error) {
	name := qname(simple.Name)
	if _, isVT := hasVirtualTables(s, name); isVT {
		return nil, false, nil
	}
	t, ok := s.Catalog.Table(name)
	if !ok {
		if _, viewOK := s.Catalog.View(name); viewOK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("table does not exist: %s", name)
	}
	data := s.Tables[catalog.Normalize(name)]
	if data == nil {
		return nil, false, nil
	}
	positions := rangeCandidatePositions(s, name, data, col, start, end)
	if positions == nil {
		return nil, false, nil
	}
	rows := make([]rowContext, 0, len(positions))
	for _, pos := range positions {
		if pos < 0 || pos >= len(data.Rows) || data.Rows[pos].Deleted != 0 {
			continue
		}
		rows = append(rows, rowToContext(name, simple, t, data.Rows[pos]))
	}
	return rows, true, nil
}

func buildIndexedRows(s *persistedState, simple *ast.SimpleTable, col string, val any) ([]rowContext, bool, error) {
	name := qname(simple.Name)
	if _, isCTE := s.ctes[catalog.Normalize(name)]; isCTE {
		return nil, false, nil
	}
	if _, isVT := hasVirtualTables(s, name); isVT {
		return nil, false, nil
	}
	t, ok := s.Catalog.Table(name)
	if !ok {
		if _, viewOK := s.Catalog.View(name); viewOK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("table does not exist: %s", name)
	}
	data := s.Tables[catalog.Normalize(name)]
	if data == nil {
		return nil, false, nil
	}
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		id, ok := asInt(val)
		if !ok {
			return nil, true, nil
		}
		pos, ok := data.lookupRowID(id)
		if !ok || data.Rows[pos].Deleted != 0 {
			return nil, true, nil
		}
		row := rowToContext(name, simple, t, data.Rows[pos])
		return []rowContext{row}, true, nil
	}
	idxName := usableIndexForColumn(s, name, col)
	if idxName == "" {
		return nil, false, nil
	}
	idx := catalog.Index{Name: idxName, Columns: []string{col}}
	rows := make([]rowContext, 0, 4)
	data.eachIndexPosition(idxName, runtimeIndexLookupKey(idx, val), func(pos int) bool {
		if pos < 0 || pos >= len(data.Rows) || data.Rows[pos].Deleted != 0 {
			return true
		}
		rows = append(rows, rowToContext(name, simple, t, data.Rows[pos]))
		return true
	})
	return rows, true, nil
}

func indexedEqualityPredicate(expr ast.Expr, args []any) (string, any, bool, error) {
	be, ok := expr.(*ast.BinaryExpr)
	if !ok || be.Op != lexer.EQ {
		return "", nil, false, nil
	}
	if col := exprColumnName(be.Left); col != "" {
		v, err := evalExpr(be.Right, nil, &binder{args: args})
		if err != nil && strings.Contains(err.Error(), "unsupported expression") {
			return "", nil, false, nil
		}
		return col, v, err == nil, err
	}
	if col := exprColumnName(be.Right); col != "" {
		v, err := evalExpr(be.Left, nil, &binder{args: args})
		if err != nil && strings.Contains(err.Error(), "unsupported expression") {
			return "", nil, false, nil
		}
		return col, v, err == nil, err
	}
	return "", nil, false, nil
}

func indexedRangePredicate(expr ast.Expr, args []any) (string, *rangeBound, *rangeBound, bool, error) {
	be, ok := expr.(*ast.BinaryExpr)
	if !ok {
		return "", nil, nil, false, nil
	}
	if be.Op == lexer.AND {
		lcol, lstart, lend, lok, err := indexedRangePredicate(be.Left, args)
		if err != nil || !lok {
			return "", nil, nil, lok, err
		}
		rcol, rstart, rend, rok, err := indexedRangePredicate(be.Right, args)
		if err != nil || !rok || lcol != rcol {
			return "", nil, nil, rok, err
		}
		return lcol, tighterStart(lstart, rstart), tighterEnd(lend, rend), true, nil
	}
	col, bound, isStart, ok, err := simpleRangeBound(be, args)
	if err != nil || !ok {
		return "", nil, nil, ok, err
	}
	if isStart {
		return col, bound, nil, true, nil
	}
	return col, nil, bound, true, nil
}

func simpleRangeBound(be *ast.BinaryExpr, args []any) (string, *rangeBound, bool, bool, error) {
	if col := exprColumnName(be.Left); col != "" {
		v, err := evalExpr(be.Right, nil, &binder{args: args})
		if err != nil {
			if strings.Contains(err.Error(), "unsupported expression") {
				return "", nil, false, false, nil
			}
			return "", nil, false, false, err
		}
		switch be.Op {
		case lexer.GT:
			return col, &rangeBound{value: v, inclusive: false}, true, true, nil
		case lexer.GTE:
			return col, &rangeBound{value: v, inclusive: true}, true, true, nil
		case lexer.LT:
			return col, &rangeBound{value: v, inclusive: false}, false, true, nil
		case lexer.LTE:
			return col, &rangeBound{value: v, inclusive: true}, false, true, nil
		}
	}
	if col := exprColumnName(be.Right); col != "" {
		v, err := evalExpr(be.Left, nil, &binder{args: args})
		if err != nil {
			if strings.Contains(err.Error(), "unsupported expression") {
				return "", nil, false, false, nil
			}
			return "", nil, false, false, err
		}
		switch be.Op {
		case lexer.GT:
			return col, &rangeBound{value: v, inclusive: false}, false, true, nil
		case lexer.GTE:
			return col, &rangeBound{value: v, inclusive: true}, false, true, nil
		case lexer.LT:
			return col, &rangeBound{value: v, inclusive: false}, true, true, nil
		case lexer.LTE:
			return col, &rangeBound{value: v, inclusive: true}, true, true, nil
		}
	}
	return "", nil, false, false, nil
}

func tighterStart(a, b *rangeBound) *rangeBound {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	c, ok := compareValues(a.value, b.value, "BINARY")
	if !ok || c > 0 || (c == 0 && !a.inclusive) {
		return a
	}
	return b
}

func tighterEnd(a, b *rangeBound) *rangeBound {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	c, ok := compareValues(a.value, b.value, "BINARY")
	if !ok || c < 0 || (c == 0 && !a.inclusive) {
		return a
	}
	return b
}

func exprColumnName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return catalog.Normalize(e.Unquoted)
	case *ast.QualifiedIdent:
		if len(e.Parts) > 0 {
			return catalog.Normalize(e.Parts[len(e.Parts)-1].Unquoted)
		}
	}
	return ""
}

func countParams(expr ast.Expr) int {
	switch e := expr.(type) {
	case nil:
		return 0
	case *ast.Param:
		return 1
	case *ast.BinaryExpr:
		return countParams(e.Left) + countParams(e.Right)
	case *ast.UnaryExpr:
		return countParams(e.Expr)
	case *ast.BetweenExpr:
		return countParams(e.Expr) + countParams(e.Lo) + countParams(e.Hi)
	case *ast.InExpr:
		n := countParams(e.Expr)
		for _, item := range e.List {
			n += countParams(item)
		}
		return n
	case *ast.LikeExpr:
		return countParams(e.Expr) + countParams(e.Pattern) + countParams(e.Escape)
	case *ast.CaseExpr:
		n := countParams(e.Operand) + countParams(e.Else)
		for _, when := range e.Whens {
			n += countParams(when.Cond) + countParams(when.Result)
		}
		return n
	case *ast.CastExpr:
		return countParams(e.Expr)
	case *ast.FuncCall:
		n := 0
		for _, arg := range e.Args {
			n += countParams(arg)
		}
		return n
	default:
		return 0
	}
}

func collationForExpr(tables []*catalog.Table, expr ast.Expr) string {
	col := exprColumnName(expr)
	if col == "" {
		return "BINARY"
	}
	for _, t := range tables {
		if c, _, ok := t.Column(col); ok && c.Collation != "" {
			return strings.ToUpper(c.Collation)
		}
	}
	return "BINARY"
}

type qualifiedColumnRef struct {
	table string
	col   string
}

func qualifiedColumn(expr ast.Expr) (qualifiedColumnRef, bool) {
	q, ok := expr.(*ast.QualifiedIdent)
	if !ok || len(q.Parts) < 2 {
		return qualifiedColumnRef{}, false
	}
	return qualifiedColumnRef{
		table: catalog.Normalize(q.Parts[len(q.Parts)-2].Unquoted),
		col:   catalog.Normalize(q.Parts[len(q.Parts)-1].Unquoted),
	}, true
}

func funcName(fn *ast.FuncCall) string {
	if fn == nil || fn.Name == nil || len(fn.Name.Parts) == 0 {
		return ""
	}
	return strings.ToLower(fn.Name.Parts[len(fn.Name.Parts)-1].Unquoted)
}

func usableIndexForColumn(s *persistedState, table, col string) string {
	idx, primary := usableAccessForColumn(s, table, col)
	if primary {
		return ""
	}
	return idx
}

func usableAccessForColumn(s *persistedState, table, col string) (string, bool) {
	t, ok := s.Catalog.Table(table)
	if !ok {
		return "", false
	}
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		return "PRIMARY", true
	}
	for _, idx := range t.Indexes {
		// Single-column lookups build a single-value index key. A composite
		// index has a tuple key, so using it for only its first column can
		// incorrectly return no rows. Fall back to a scan in that case.
		if len(idx.Columns) == 1 && idx.Columns[0] == col {
			return idx.Name, false
		}
	}
	return "", false
}

func buildRefRows(s *persistedState, ref ast.TableRef) ([]*catalog.Table, []rowContext, error) {
	switch r := ref.(type) {
	case *ast.SimpleTable:
		return buildSimpleRows(s, r, nil)
	case *ast.JoinTable:
		lt, lr, err := buildRefRows(s, r.Left)
		if err != nil {
			return nil, nil, err
		}
		rt, rr, err := buildRefRows(s, r.Right)
		if err != nil {
			return nil, nil, err
		}
		var out []rowContext
		for _, l := range lr {
			matched := false
			for _, right := range rr {
				row := mergeRows(l, right)
				ok := true
				if r.On != nil {
					v, err := evalExpr(r.On, row.Values, &binder{})
					if err != nil {
						return nil, nil, err
					}
					ok = truthy(v)
				}
				if ok {
					matched = true
					out = append(out, row)
				}
			}
			if !matched && r.Kind == ast.LeftJoin {
				out = append(out, l)
			}
		}
		return append(lt, rt...), out, nil
	default:
		return nil, nil, fmt.Errorf("unsupported table reference: %T", ref)
	}
}

func buildSimpleRows(s *persistedState, r *ast.SimpleTable, keep func(rowVersion) bool) ([]*catalog.Table, []rowContext, error) {
	name := qname(r.Name)
	if s.compositeRead && r.Name != nil && len(r.Name.Parts) > 1 {
		name = catalog.Normalize(r.Name.Parts[0].Unquoted) + "." + name
	}
	if mat, ok := s.ctes[catalog.Normalize(name)]; ok {
		if keep != nil {
			return nil, nil, errors.New("CTE rows cannot be filtered by row predicate")
		}
		return buildCTERows(r, mat)
	}
	if vt, isVT := hasVirtualTables(s, name); isVT {
		return buildVirtualRows(s, vt, r, keep)
	}
	t, ok := s.Catalog.Table(name)
	if !ok {
		if view, viewOK := s.Catalog.View(name); viewOK {
			return buildViewRows(s, r, view)
		}
		return nil, nil, fmt.Errorf("table does not exist: %s", name)
	}
	data := s.Tables[catalog.Normalize(name)]
	var rows []rowContext
	for _, row := range data.Rows {
		if row.Deleted != 0 {
			continue
		}
		if keep != nil && !keep(row) {
			continue
		}
		rows = append(rows, rowToContext(name, r, t, row))
	}
	return []*catalog.Table{t}, rows, nil
}

func buildViewRows(s *persistedState, r *ast.SimpleTable, view *catalog.View) ([]*catalog.Table, []rowContext, error) {
	s.ensureRuntime()
	stmt, err := parseViewSelectStatement(view.SelectSQL)
	if err != nil {
		return nil, nil, err
	}
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		return nil, nil, fmt.Errorf("view is not a SELECT: %s", view.Name)
	}
	rows, err := execSelectGeneric(s, sel, nil)
	if err != nil {
		return nil, nil, err
	}
	cols := view.Columns
	if len(cols) == 0 {
		for _, name := range rows.Columns {
			cols = append(cols, catalog.Column{Name: catalog.Normalize(name), Type: catalog.Any, DeclType: "ANY"})
		}
	}
	t := &catalog.Table{Name: view.Name, Columns: cols, SQL: view.SQL}
	var contexts []rowContext
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		contexts = append(contexts, viewRowToContext(view.Name, r, t, row))
	}
	return []*catalog.Table{t}, contexts, nil
}

func parseViewSelectStatement(sql string) (ast.Statement, error) {
	parser := sqlparser.NewString(sql)
	return parser.Next()
}

func viewRowToContext(name string, r *ast.SimpleTable, t *catalog.Table, row []any) rowContext {
	alias := name
	if r.Alias != nil {
		alias = catalog.Normalize(r.Alias.Unquoted)
	}
	vals := make(map[string]any, len(t.Columns)*3)
	for i, col := range t.Columns {
		var v any
		if i < len(row) {
			v = row[i]
		}
		vals[col.Name] = v
		vals[name+"."+col.Name] = v
		vals[alias+"."+col.Name] = v
	}
	return rowContext{Values: vals}
}

func rowToContext(name string, r *ast.SimpleTable, t *catalog.Table, row rowVersion) rowContext {
	alias := name
	if r.Alias != nil {
		alias = catalog.Normalize(r.Alias.Unquoted)
	}
	vals := make(map[string]any, len(t.Columns)*3+9)
	for _, col := range t.Columns {
		v := row.value(t, col.Name)
		vals[col.Name] = v
		vals[name+"."+col.Name] = v
		vals[alias+"."+col.Name] = v
	}
	for _, rowidName := range []string{"rowid", "_rowid_", "oid"} {
		vals[rowidName] = row.RowID
		vals[name+"."+rowidName] = row.RowID
		vals[alias+"."+rowidName] = row.RowID
	}
	return rowContext{Values: vals}
}

func crossRows(left, right []rowContext) []rowContext {
	var out []rowContext
	for _, l := range left {
		for _, r := range right {
			out = append(out, mergeRows(l, r))
		}
	}
	return out
}

func mergeRows(left, right rowContext) rowContext {
	vals := cloneValues(left.Values)
	for k, v := range right.Values {
		vals[k] = v
	}
	return rowContext{Values: vals}
}

func tableForWrite(s *persistedState, name string) (*catalog.Table, *tableData, error) {
	t, ok := s.Catalog.Table(name)
	if !ok {
		return nil, nil, fmt.Errorf("table does not exist: %s", name)
	}
	key := catalog.Normalize(name)
	data := s.Tables[key]
	if data == nil {
		data = &tableData{}
		s.Tables[key] = data
	}
	return t, data, nil
}

func validateRow(s *persistedState, t *catalog.Table, data *tableData, values map[string]any, self int64) error {
	if err := validateChecks(t, values); err != nil {
		return err
	}
	for _, col := range t.Columns {
		v := values[col.Name]
		if col.NotNull && v == nil {
			return fmt.Errorf("column %s cannot be null", col.Name)
		}
		if col.Unique || isSingleColumnPrimaryKey(t, col.Name) {
			for _, row := range data.Rows {
				if row.Deleted == 0 && row.RowID != self && valuesEqual(row.Values[col.Name], v, col.Collation) {
					return fmt.Errorf("unique constraint failed: %s.%s", t.Name, col.Name)
				}
			}
			for _, row := range data.Pending {
				if row.Deleted == 0 && row.RowID != self && valuesEqual(row.Values[col.Name], v, col.Collation) {
					return fmt.Errorf("unique constraint failed: %s.%s", t.Name, col.Name)
				}
			}
		}
	}
	for _, idx := range t.Indexes {
		if !idx.Unique {
			continue
		}
		newIncluded, err := partialIndexIncludesRow(s, idx, values)
		if err != nil {
			return err
		}
		if !newIncluded {
			continue
		}
		for _, row := range data.Rows {
			if row.Deleted != 0 || row.RowID == self {
				continue
			}
			oldIncluded, err := partialIndexIncludesRow(s, idx, row.Values)
			if err != nil {
				return err
			}
			if !oldIncluded {
				continue
			}
			same := true
			for _, col := range idx.Columns {
				same = same && valuesEqual(row.Values[col], values[col], columnCollation(t, col))
			}
			if same {
				return fmt.Errorf("unique constraint failed: %s.%s", t.Name, idx.Name)
			}
		}
		for _, row := range data.Pending {
			if row.Deleted != 0 || row.RowID == self {
				continue
			}
			oldIncluded, err := partialIndexIncludesRow(s, idx, row.Values)
			if err != nil {
				return err
			}
			if !oldIncluded {
				continue
			}
			same := true
			for _, col := range idx.Columns {
				same = same && valuesEqual(row.Values[col], values[col], columnCollation(t, col))
			}
			if same {
				return fmt.Errorf("unique constraint failed: %s.%s", t.Name, idx.Name)
			}
		}
	}
	if s.Pragmas.ForeignKeys {
		for _, fk := range t.ForeignKeys {
			if err := validateForeignKey(s, fk, values); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateChecks(t *catalog.Table, values map[string]any) error {
	for _, col := range t.Columns {
		if col.Check == "" {
			continue
		}
		ok, err := evalCheckExpression(col.Check, values)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("check constraint failed: %s", col.Name)
		}
	}
	for i, check := range t.Checks {
		ok, err := evalCheckExpression(check, values)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("check constraint failed: %s.%d", t.Name, i+1)
		}
	}
	return nil
}

func evalCheckExpression(expr string, values map[string]any) (bool, error) {
	expr = trimOuterParens(strings.TrimSpace(expr))
	if expr == "" {
		return true, nil
	}
	for _, part := range splitCheckAND(expr) {
		part = trimOuterParens(strings.TrimSpace(part))
		ok, err := evalCheckTerm(part, values)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func evalCheckTerm(term string, values map[string]any) (bool, error) {
	fields := strings.Fields(term)
	if len(fields) == 4 && strings.EqualFold(fields[1], "is") && strings.EqualFold(fields[2], "not") && strings.EqualFold(fields[3], "null") {
		return values[catalog.Normalize(strings.Trim(fields[0], `"'[]`))] != nil, nil
	}
	if len(fields) == 3 && strings.EqualFold(fields[1], "is") && strings.EqualFold(fields[2], "null") {
		return values[catalog.Normalize(strings.Trim(fields[0], `"'[]`))] == nil, nil
	}
	for _, op := range []string{">=", "<=", "<>", "!=", "=", ">", "<"} {
		if idx := strings.Index(term, op); idx >= 0 {
			left, ok := checkOperandValue(strings.TrimSpace(term[:idx]), values)
			if !ok {
				return false, fmt.Errorf("unsupported check operand: %s", strings.TrimSpace(term[:idx]))
			}
			right, ok := checkOperandValue(strings.TrimSpace(term[idx+len(op):]), values)
			if !ok {
				return false, fmt.Errorf("unsupported check operand: %s", strings.TrimSpace(term[idx+len(op):]))
			}
			if left == nil || right == nil {
				return true, nil
			}
			c, ok := compareValues(left, right, "BINARY")
			if !ok {
				return false, nil
			}
			switch op {
			case "=":
				return c == 0, nil
			case "!=", "<>":
				return c != 0, nil
			case ">":
				return c > 0, nil
			case ">=":
				return c >= 0, nil
			case "<":
				return c < 0, nil
			case "<=":
				return c <= 0, nil
			}
		}
	}
	v, ok := checkOperandValue(term, values)
	if !ok {
		return false, fmt.Errorf("unsupported check expression: %s", term)
	}
	if v == nil {
		return true, nil
	}
	return truthy(v), nil
}

func checkOperandValue(raw string, values map[string]any) (any, bool) {
	raw = trimOuterParens(strings.TrimSpace(raw))
	if raw == "" {
		return nil, false
	}
	name := catalog.Normalize(strings.Trim(raw, `"'[]`))
	if v, ok := values[name]; ok && !isQuotedSQLString(raw) {
		return v, true
	}
	return parseSimpleLiteral(raw)
}

func splitCheckAND(s string) []string {
	var out []string
	depth := 0
	inQuote := rune(0)
	last := 0
	for i, r := range s {
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			inQuote = r
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && i+5 <= len(s) && strings.EqualFold(s[i:i+5], " and ") {
			out = append(out, s[last:i])
			last = i + 5
		}
	}
	out = append(out, s[last:])
	return out
}

func trimOuterParens(s string) string {
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		depth := 0
		wraps := true
		for i, r := range s {
			switch r {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 && i != len(s)-1 {
					wraps = false
				}
			}
		}
		if !wraps {
			return s
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func isQuotedSQLString(raw string) bool {
	return len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\''
}

func validateForeignKey(s *persistedState, fk catalog.ForeignKey, values map[string]any) error {
	refTable, ok := s.Catalog.Table(fk.RefTable)
	if !ok {
		return fmt.Errorf("foreign key mismatch: missing table %s", fk.RefTable)
	}
	refData := s.Tables[refTable.Name]
	refCols := resolveFKColumns(refTable, fk.RefColumns)
	if len(refCols) != len(fk.Columns) {
		return errors.New("foreign key mismatch")
	}
	for _, col := range fk.Columns {
		if values[col] == nil {
			return nil
		}
	}
	for _, row := range refData.Rows {
		if row.Deleted != 0 {
			continue
		}
		match := true
		for i, col := range fk.Columns {
			match = match && reflect.DeepEqual(values[col], row.Values[refCols[i]])
		}
		if match {
			return nil
		}
	}
	return errors.New("foreign key constraint failed")
}

func resolveFKColumns(t *catalog.Table, cols []string) []string {
	var out []string
	for _, col := range cols {
		if col == primaryKeyNamePlaceholder {
			out = append(out, primaryKeyColumns(t)...)
		} else {
			out = append(out, col)
		}
	}
	return out
}

func primaryKeyColumns(t *catalog.Table) []string {
	if t == nil {
		return nil
	}
	if len(t.PrimaryKeyColumns) > 0 {
		return t.PrimaryKeyColumns
	}
	// Older snapshots do not store key order explicitly. Preserve their
	// primary-key membership using the table's column order until rewritten.
	var cols []string
	for _, col := range t.Columns {
		if col.PrimaryKey {
			cols = append(cols, col.Name)
		}
	}
	return cols
}

func integerPrimaryKeyColumn(t *catalog.Table) string {
	if t == nil {
		return ""
	}
	primaryKeyName := ""
	primaryKeyDecl := ""
	for _, col := range t.Columns {
		if !col.PrimaryKey {
			continue
		}
		if primaryKeyName != "" {
			return ""
		}
		primaryKeyName = col.Name
		primaryKeyDecl = col.DeclType
	}
	if primaryKeyName != "" && strings.EqualFold(strings.TrimSpace(primaryKeyDecl), "INTEGER") {
		return primaryKeyName
	}
	return ""
}

func isSingleColumnPrimaryKey(t *catalog.Table, name string) bool {
	if t == nil {
		return false
	}
	var primaryKey string
	for _, col := range t.Columns {
		if !col.PrimaryKey {
			continue
		}
		if primaryKey != "" {
			return false
		}
		primaryKey = col.Name
	}
	return primaryKey != "" && primaryKey == name
}

func coerce(col catalog.Column, v any, strict bool) (any, error) {
	// These values are already canonical SQLite storage values. Keep common
	// integer and text binds out of the reflection and conversion path.
	switch v.(type) {
	case int64:
	case string:
		v = strings.Clone(v.(string))
	default:
		var err error
		v, err = normalizeSQLiteBindValue(v)
		if err != nil {
			return nil, err
		}
	}
	if v == nil {
		if col.NotNull {
			return nil, fmt.Errorf("column %s cannot be null", col.Name)
		}
		return nil, nil
	}
	if col.Type == catalog.JSON {
		// JSON columns keep documents as UTF-8 text, but unlike ordinary TEXT
		// columns they reject malformed documents. Structured Go bind values
		// have already been JSON-marshaled by normalizeSQLiteBindValue.
		var document string
		switch value := v.(type) {
		case string:
			document = value
		case []byte:
			document = string(value)
		default:
			return nil, fmt.Errorf("cannot store %T in JSON column %s", v, col.Name)
		}
		if !json.Valid([]byte(document)) {
			return nil, fmt.Errorf("invalid JSON for column %s", col.Name)
		}
		return document, nil
	}
	if !strict {
		return applyAffinity(col, v), nil
	}
	if !catalog.StrictTypeAllowed(col.Type) {
		return nil, fmt.Errorf("unsupported STRICT column type: %s", col.DeclType)
	}
	switch col.Type {
	case catalog.Any:
		return v, nil
	case catalog.Integer:
		if i, ok := asInt(v); ok {
			return i, nil
		}
	case catalog.Real:
		if f, ok := asFloat(v); ok {
			return f, nil
		}
	case catalog.Text:
		if s, ok := v.(string); ok {
			return s, nil
		}
		return fmt.Sprint(v), nil
	case catalog.JSON:
		return v, nil // handled above; retained for exhaustive type handling
	case catalog.Blob:
		if x, ok := v.([]byte); ok {
			return x, nil
		}
	}
	return nil, fmt.Errorf("cannot store %T in %s column %s", v, col.Type, col.Name)
}

func applyAffinity(col catalog.Column, v any) any {
	if v == nil {
		return nil
	}
	switch col.Type {
	case catalog.Integer:
		if _, ok := v.(int64); ok {
			return v
		}
		if _, ok := v.(int); ok {
			return v
		}
		if i, ok := asInt(v); ok {
			return i
		}
	case catalog.Real:
		if _, ok := v.(float64); ok {
			return v
		}
		if f, ok := asFloat(v); ok {
			return f
		}
	case catalog.Text, catalog.JSON:
		switch x := v.(type) {
		case string:
			return v
		case []byte:
			return string(x)
		default:
			return fmt.Sprint(v)
		}
	case catalog.Numeric:
		if _, ok := v.(int64); ok {
			return v
		}
		if _, ok := v.(float64); ok {
			return v
		}
		if i, ok := asInt(v); ok {
			return i
		}
		if f, ok := asFloat(v); ok {
			return f
		}
	}
	return v
}

type binder struct {
	args  []any
	pos   int
	named map[string]any
}

func (b *binder) next() (any, error) {
	if b.pos >= len(b.args) {
		return nil, errors.New("not enough bind parameters")
	}
	for b.pos < len(b.args) {
		v := b.args[b.pos]
		b.pos++
		if isNamedSource(v) {
			continue
		}
		return normalizeSQLiteBindValue(v)
	}
	return nil, errors.New("not enough bind parameters")
}

func (b *binder) param(raw []byte) (any, error) {
	name := parameterName(raw)
	if name == "" {
		return b.next()
	}
	if b.named == nil {
		b.named = make(map[string]any)
		for _, arg := range b.args {
			addNamedSource(b.named, arg)
		}
	}
	if v, ok := b.named[name]; ok {
		return normalizeSQLiteBindValue(v)
	}
	if v, ok := b.named[catalog.Normalize(name)]; ok {
		return normalizeSQLiteBindValue(v)
	}
	return nil, fmt.Errorf("missing named bind parameter: %s", name)
}

func (b *binder) rawParam(raw []byte) (any, error) {
	name := parameterName(raw)
	if name == "" {
		if b.pos >= len(b.args) {
			return nil, errors.New("not enough bind parameters")
		}
		value := b.args[b.pos]
		b.pos++
		return value, nil
	}
	if b.named == nil {
		b.named = make(map[string]any)
		for _, arg := range b.args {
			addNamedSource(b.named, arg)
		}
	}
	if value, ok := b.named[name]; ok {
		return value, nil
	}
	if value, ok := b.named[catalog.Normalize(name)]; ok {
		return value, nil
	}
	return nil, fmt.Errorf("missing named bind parameter: %s", name)
}

// normalizeSQLiteBindValue maps common Go inputs onto SQLite's five storage
// classes. database/sql already does this conversion for the driver API, but
// users of DB.Exec and DB.Query can call the embedded API directly.
func normalizeSQLiteBindValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if valuer, ok := v.(driver.Valuer); ok {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer && rv.IsNil() {
			return nil, nil
		}
		value, err := valuer.Value()
		if err != nil {
			return nil, err
		}
		if !driver.IsValue(value) {
			return nil, fmt.Errorf("driver.Valuer returned unsupported SQLite value %T", value)
		}
		return normalizeSQLiteBindValue(value)
	}
	if marshaler, ok := v.(encoding.TextMarshaler); ok {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer && rv.IsNil() {
			return nil, nil
		}
		text, err := marshaler.MarshalText()
		if err != nil {
			return nil, err
		}
		return string(text), nil
	}
	// These are already canonical SQLite bind values. Handle them before
	// reflection to keep ordinary scalar binds on the fast path.
	switch x := v.(type) {
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		return normalizeSQLiteFloat(x), nil
	case string:
		return x, nil
	case []byte:
		if x == nil {
			return nil, nil
		}
		return cloneSQLiteBlob(x), nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, nil
		}
		v = rv.Elem().Interface()
		rv = reflect.ValueOf(v)
	}
	switch x := v.(type) {
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return nil, errors.New("unsigned integer exceeds SQLite INTEGER range")
		}
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return nil, errors.New("unsigned integer exceeds SQLite INTEGER range")
		}
		return int64(x), nil
	case float32:
		return normalizeSQLiteFloat(float64(x)), nil
	case float64:
		return normalizeSQLiteFloat(x), nil
	case string:
		return v, nil
	case json.Number:
		// database/sql treats json.Number as its underlying string type. Keep
		// the same SQLite TEXT storage class for direct DB bindings.
		return string(x), nil
	case []byte:
		if x == nil {
			return nil, nil
		}
		return cloneSQLiteBlob(x), nil
	case time.Time:
		return x.Format(time.RFC3339Nano), nil
	default:
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return rv.Int(), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if rv.Uint() > math.MaxInt64 {
				return nil, errors.New("unsigned integer exceeds SQLite INTEGER range")
			}
			return int64(rv.Uint()), nil
		case reflect.Float32, reflect.Float64:
			return normalizeSQLiteFloat(rv.Convert(reflect.TypeOf(float64(0))).Float()), nil
		case reflect.String:
			return rv.String(), nil
		case reflect.Slice:
			if rv.Type().Elem().Kind() == reflect.Uint8 {
				if rv.IsNil() {
					return nil, nil
				}
				return cloneSQLiteBlob(rv.Bytes()), nil
			}
		}
		// The direct API accepts structured values as JSON text. Convert before
		// storing so live rows and recovered rows have the same SQLite class.
		switch rv.Kind() {
		case reflect.Array, reflect.Slice, reflect.Map, reflect.Struct:
			encoded, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			return string(encoded), nil
		default:
			return nil, fmt.Errorf("unsupported SQLite bind type %T", v)
		}
	}
}

func normalizeSQLiteFloat(value float64) any {
	if math.IsNaN(value) {
		return nil
	}
	return value
}

func namedArg(v any) (NamedArg, bool) {
	switch x := v.(type) {
	case NamedArg:
		return x, true
	case *NamedArg:
		if x == nil {
			return NamedArg{}, false
		}
		return *x, true
	default:
		return NamedArg{}, false
	}
}

func isNamedSource(v any) bool {
	if _, ok := namedArg(v); ok {
		return true
	}
	if _, ok := v.(driver.Valuer); ok {
		return false
	}
	if _, ok := v.(encoding.TextMarshaler); ok {
		return false
	}
	if _, ok := v.(time.Time); ok {
		return false
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return false
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	if rv.Type() == reflect.TypeOf(time.Time{}) {
		return false
	}
	switch rv.Kind() {
	case reflect.Map:
		return rv.Type().Key().Kind() == reflect.String
	case reflect.Struct:
		return true
	default:
		return false
	}
}

func addNamedSource(out map[string]any, v any) {
	if n, ok := namedArg(v); ok {
		addNamedValue(out, n.Name, n.Value)
		return
	}
	if !isNamedSource(v) {
		return
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return
		}
		iter := rv.MapRange()
		for iter.Next() {
			addNamedValue(out, iter.Key().String(), iter.Value().Interface())
		}
	case reflect.Struct:
		addStructNamedValues(out, rv)
	}
}

func addStructNamedValues(out map[string]any, rv reflect.Value) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if field.PkgPath != "" || field.Anonymous {
			continue
		}
		value := rv.Field(i).Interface()
		addNamedValue(out, field.Name, value)
		addNamedValue(out, camelToSnake(field.Name), value)
		for _, tagName := range []string{"db", "sql", "json"} {
			name := tagBindName(field.Tag.Get(tagName))
			if name != "" {
				addNamedValue(out, name, value)
			}
		}
	}
}

func tagBindName(tag string) string {
	if tag == "" || tag == "-" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag == "-" {
		return ""
	}
	return strings.TrimSpace(tag)
}

func addNamedValue(out map[string]any, name string, value any) {
	if name == "" {
		return
	}
	out[name] = value
	out[catalog.Normalize(name)] = value
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

func hasNamedArgs(args []any) bool {
	for _, arg := range args {
		if isNamedSource(arg) {
			return true
		}
	}
	return false
}

func parameterName(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "?" {
		return ""
	}
	if s[0] == ':' || s[0] == '@' || s[0] == '$' {
		return strings.TrimSpace(s[1:])
	}
	if s[0] == '?' && len(s) > 1 {
		return ""
	}
	return s
}

func evalExpr(expr ast.Expr, row map[string]any, b *binder) (any, error) {
	switch e := expr.(type) {
	case *ast.Literal:
		return literal(e)
	case *ast.NullLit:
		return nil, nil
	case *ast.Param:
		return b.param(e.Raw)
	case *ast.Ident:
		if v, ok := triggerLookup("", e.Unquoted); ok {
			return v, nil
		}
		return row[catalog.Normalize(e.Unquoted)], nil
	case *ast.QualifiedIdent:
		if len(e.Parts) == 0 {
			return nil, errors.New("empty identifier")
		}
		var parts []string
		for _, p := range e.Parts {
			parts = append(parts, catalog.Normalize(p.Unquoted))
		}
		if len(e.Parts) >= 2 {
			if v, ok := triggerLookup(e.Parts[0].Unquoted, e.Parts[1].Unquoted); ok {
				return v, nil
			}
		}
		if v, ok := row[strings.Join(parts, ".")]; ok {
			return v, nil
		}
		return row[catalog.Normalize(e.Parts[len(e.Parts)-1].Unquoted)], nil
	case *ast.IsNullExpr:
		v, err := evalExpr(e.Expr, row, b)
		if err != nil {
			return nil, err
		}
		isNull := v == nil
		if e.Not {
			return !isNull, nil
		}
		return isNull, nil
	case *ast.BetweenExpr:
		v, err := evalExpr(e.Expr, row, b)
		if err != nil {
			return nil, err
		}
		lo, err := evalExpr(e.Lo, row, b)
		if err != nil {
			return nil, err
		}
		hi, err := evalExpr(e.Hi, row, b)
		if err != nil {
			return nil, err
		}
		cl, ok1 := compareValues(v, lo, "BINARY")
		ch, ok2 := compareValues(v, hi, "BINARY")
		ok := ok1 && ok2 && cl >= 0 && ch <= 0
		if e.Not {
			return !ok, nil
		}
		return ok, nil
	case *ast.InExpr:
		v, err := evalExpr(e.Expr, row, b)
		if err != nil {
			return nil, err
		}
		found := false
		for _, item := range e.List {
			iv, err := evalExpr(item, row, b)
			if err != nil {
				return nil, err
			}
			if c, ok := compareValues(v, iv, "BINARY"); ok && c == 0 {
				found = true
				break
			}
		}
		if e.Not {
			return !found, nil
		}
		return found, nil
	case *ast.LikeExpr:
		v, err := evalExpr(e.Expr, row, b)
		if err != nil {
			return nil, err
		}
		pat, err := evalExpr(e.Pattern, row, b)
		if err != nil {
			return nil, err
		}
		escape := byte(0)
		if e.Escape != nil {
			ev, err := evalExpr(e.Escape, row, b)
			if err != nil {
				return nil, err
			}
			es := fmt.Sprint(ev)
			if len(es) != 1 {
				return nil, errors.New("LIKE ESCAPE expression must evaluate to a single character")
			}
			escape = es[0]
		}
		ok := likeWithEscape(fmt.Sprint(v), fmt.Sprint(pat), escape)
		if e.Not {
			return !ok, nil
		}
		return ok, nil
	case *ast.CaseExpr:
		for _, w := range e.Whens {
			cv, err := evalExpr(w.Cond, row, b)
			if err != nil {
				return nil, err
			}
			if truthy(cv) {
				return evalExpr(w.Result, row, b)
			}
		}
		if e.Else != nil {
			return evalExpr(e.Else, row, b)
		}
		return nil, nil
	case *ast.CastExpr:
		v, err := evalExpr(e.Expr, row, b)
		if err != nil {
			return nil, err
		}
		typ, _ := catalog.ParseType(string(e.Type.Name))
		return applyAffinity(catalog.Column{Type: typ}, v), nil
	case *ast.FuncCall:
		return evalFunc(e, row, b)
	case *ast.BinaryExpr:
		left, err := evalExpr(e.Left, row, b)
		if err != nil {
			return nil, err
		}
		right, err := evalExpr(e.Right, row, b)
		if err != nil {
			return nil, err
		}
		return evalBinary(left, right, e.Op)
	case *ast.UnaryExpr:
		v, err := evalExpr(e.Expr, row, b)
		if err != nil {
			return nil, err
		}
		if e.Op == lexer.MINUS {
			return negateNumeric(v)
		}
		if e.Op == lexer.NOT || strings.EqualFold(e.Op.String(), "UNKNOWN") || fmt.Sprint(e.Op) == "UNKNOWN" {
			return !truthy(v), nil
		}
		return nil, fmt.Errorf("unsupported unary operator: %s", e.Op)
	default:
		return nil, fmt.Errorf("unsupported expression in v1: %T", expr)
	}
}

func literal(l *ast.Literal) (any, error) {
	raw := string(l.Raw)
	switch l.Kind {
	case lexer.INT:
		return strconv.ParseInt(raw, 10, 64)
	case lexer.FLOAT:
		return strconv.ParseFloat(raw, 64)
	case lexer.STRING, lexer.DQUOTE:
		return unquoteSQL(raw), nil
	case lexer.HEXLIT:
		clean := strings.TrimPrefix(strings.TrimPrefix(raw, "x'"), "X'")
		clean = strings.TrimSuffix(clean, "'")
		return hex.DecodeString(clean)
	case lexer.TRUE_KW:
		return true, nil
	case lexer.FALSE_KW:
		return false, nil
	default:
		if strings.EqualFold(raw, "true") {
			return true, nil
		}
		if strings.EqualFold(raw, "false") {
			return false, nil
		}
		return raw, nil
	}
}

func unquoteSQL(raw string) string {
	if len(raw) >= 2 {
		q := raw[0]
		if (q == '\'' || q == '"') && raw[len(raw)-1] == q {
			body := raw[1 : len(raw)-1]
			if q == '\'' {
				return strings.ReplaceAll(body, "''", "'")
			}
			if s, err := strconv.Unquote(raw); err == nil {
				return s
			}
			return body
		}
	}
	return raw
}

func evalBinary(left, right any, op lexer.TokenType) (any, error) {
	switch op {
	case lexer.EQ:
		if left == nil || right == nil {
			return nil, nil
		}
		if c, ok := compareValues(left, right, "BINARY"); ok {
			return c == 0, nil
		}
		return reflect.DeepEqual(left, right), nil
	case lexer.NEQ:
		if left == nil || right == nil {
			return nil, nil
		}
		if c, ok := compareValues(left, right, "BINARY"); ok {
			return c != 0, nil
		}
		return !reflect.DeepEqual(left, right), nil
	case lexer.LT, lexer.LTE, lexer.GT, lexer.GTE:
		if left == nil || right == nil {
			return nil, nil
		}
		c, ok := compareValues(left, right, "BINARY")
		if !ok {
			return nil, fmt.Errorf("values are not comparable: %T and %T", left, right)
		}
		switch op {
		case lexer.LT:
			return c < 0, nil
		case lexer.LTE:
			return c <= 0, nil
		case lexer.GT:
			return c > 0, nil
		case lexer.GTE:
			return c >= 0, nil
		}
	case lexer.AND:
		return truthy(left) && truthy(right), nil
	case lexer.OR:
		return truthy(left) || truthy(right), nil
	case lexer.PLUS, lexer.MINUS, lexer.STAR, lexer.SLASH, lexer.PERCENT:
		value, err := arithmetic(left, right, op)
		if err != nil {
			return nil, err
		}
		if number, ok := value.(float64); ok {
			return normalizeSQLiteFloat(number), nil
		}
		return value, nil
	case lexer.DBAR:
		return fmt.Sprint(left) + fmt.Sprint(right), nil
	}
	return nil, fmt.Errorf("unsupported binary operator: %s", op)
}

func compareValues(left, right any, collate string) (int, bool) {
	if left == nil || right == nil {
		return 0, false
	}
	lf, lok := asFloat(left)
	rf, rok := asFloat(right)
	if lok && rok {
		switch {
		case lf < rf:
			return -1, true
		case lf > rf:
			return 1, true
		default:
			return 0, true
		}
	}
	ls, lok := left.(string)
	rs, rok := right.(string)
	if lok && rok {
		switch strings.ToUpper(collate) {
		case "NOCASE":
			ls, rs = strings.ToLower(ls), strings.ToLower(rs)
		case "RTRIM":
			ls, rs = strings.TrimRight(ls, " "), strings.TrimRight(rs, " ")
		}
		return strings.Compare(ls, rs), true
	}
	return 0, false
}

func valuesEqual(left, right any, collate string) bool {
	if c, ok := compareValues(left, right, collate); ok {
		return c == 0
	}
	return reflect.DeepEqual(left, right)
}

func evalFunc(fn *ast.FuncCall, row map[string]any, b *binder) (any, error) {
	args := make([]any, 0, len(fn.Args))
	for _, expr := range fn.Args {
		v, err := evalExpr(expr, row, b)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	return callScalarFunc(funcCallName(fn), args)
}

// callScalarFunc dispatches a scalar function by name. It is shared by ordinary
// expression evaluation and window-function argument evaluation, where nested
// aggregates must be substituted before the call.
func callScalarFunc(name string, args []any) (any, error) {
	value, err := callScalarFuncRaw(name, args)
	if err != nil {
		return nil, err
	}
	if number, ok := value.(float64); ok {
		return normalizeSQLiteFloat(number), nil
	}
	return value, nil
}

func callScalarFuncRaw(name string, args []any) (any, error) {
	switch name {
	case "lower":
		if len(args) != 1 {
			return nil, errors.New("lower expects 1 argument")
		}
		if args[0] == nil {
			return nil, nil
		}
		// SQLite's lower() only folds ASCII unless an extension like ICU is
		// loaded, so non-ASCII runes are left alone.
		return asciiCase(stringifySQL(args[0]), false), nil
	case "upper":
		if len(args) != 1 {
			return nil, errors.New("upper expects 1 argument")
		}
		if args[0] == nil {
			return nil, nil
		}
		return asciiCase(stringifySQL(args[0]), true), nil
	case "length":
		if len(args) != 1 {
			return nil, errors.New("length expects 1 argument")
		}
		if args[0] == nil {
			return nil, nil
		}
		if b, ok := args[0].([]byte); ok {
			return int64(len(b)), nil
		}
		// Text length counts characters, not bytes.
		return int64(utf8.RuneCountInString(stringifySQL(args[0]))), nil
	case "hex":
		if len(args) != 1 {
			return nil, errors.New("hex expects 1 argument")
		}
		switch x := args[0].(type) {
		case []byte:
			return strings.ToUpper(hex.EncodeToString(x)), nil
		default:
			return strings.ToUpper(hex.EncodeToString([]byte(fmt.Sprint(x)))), nil
		}
	case "typeof":
		if len(args) != 1 {
			return nil, errors.New("typeof expects 1 argument")
		}
		return sqliteTypeOf(args[0]), nil
	case "coalesce":
		for _, arg := range args {
			if arg != nil {
				return arg, nil
			}
		}
		return nil, nil
	case "ifnull":
		if len(args) != 2 {
			return nil, errors.New("ifnull expects 2 arguments")
		}
		if args[0] != nil {
			return args[0], nil
		}
		return args[1], nil
	case "nullif":
		if len(args) != 2 {
			return nil, errors.New("nullif expects 2 arguments")
		}
		if c, ok := compareValues(args[0], args[1], "BINARY"); ok && c == 0 {
			return nil, nil
		}
		return args[0], nil
	case "count", "sum", "avg", "total", "group_concat", "string_agg":
		// Single-argument aggregates are resolved by the grouping machinery,
		// not here.
		return nil, nil
	case "min", "max":
		if len(args) <= 1 {
			return nil, nil
		}
		// min(a, b, c) with several arguments is a scalar function.
		return callExtendedFunc(name, args)
	default:
		return callExtendedFunc(name, args)
	}
}

// asciiCase upper- or lower-cases ASCII letters only, matching SQLite's
// built-in lower()/upper() without an ICU extension.
func asciiCase(s string, upper bool) string {
	b := []byte(s)
	for i := range b {
		c := b[i]
		if upper {
			if c >= 'a' && c <= 'z' {
				b[i] = c - 32
			}
		} else {
			if c >= 'A' && c <= 'Z' {
				b[i] = c + 32
			}
		}
	}
	return string(b)
}

func sqliteTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case int, int64, bool:
		return "integer"
	case float64:
		return "real"
	case []byte:
		return "blob"
	default:
		return "text"
	}
}

func likeWithEscape(value, pattern string, escape byte) bool {
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		if j == len(pattern) {
			return i == len(value)
		}
		if escape != 0 && pattern[j] == escape && j+1 < len(pattern) {
			return i < len(value) && strings.EqualFold(value[i:i+1], pattern[j+1:j+2]) && rec(i+1, j+2)
		}
		switch pattern[j] {
		case '%':
			for k := i; k <= len(value); k++ {
				if rec(k, j+1) {
					return true
				}
			}
			return false
		case '_':
			return i < len(value) && rec(i+1, j+1)
		default:
			return i < len(value) && strings.EqualFold(value[i:i+1], pattern[j:j+1]) && rec(i+1, j+1)
		}
	}
	return rec(0, 0)
}

// arithmetic applies SQLite's binary numeric operators. When both operands are
// integers the result stays an integer and division truncates toward zero;
// introducing a real anywhere makes the result a real. Division by zero yields
// NULL rather than an error.
func arithmetic(left, right any, op lexer.TokenType) (any, error) {
	lf, lok := asFloat(left)
	rf, rok := asFloat(right)
	if !lok || !rok {
		return nil, errors.New("arithmetic requires numeric values")
	}
	bothInt := isIntegerValue(left) && isIntegerValue(right)
	switch op {
	case lexer.PLUS:
		if bothInt {
			li, _ := asInt(left)
			ri, _ := asInt(right)
			if (ri > 0 && li > math.MaxInt64-ri) || (ri < 0 && li < math.MinInt64-ri) {
				return nil, errors.New("integer overflow")
			}
			return li + ri, nil
		}
		return lf + rf, nil
	case lexer.MINUS:
		if bothInt {
			li, _ := asInt(left)
			ri, _ := asInt(right)
			if (ri < 0 && li > math.MaxInt64+ri) || (ri > 0 && li < math.MinInt64+ri) {
				return nil, errors.New("integer overflow")
			}
			return li - ri, nil
		}
		return lf - rf, nil
	case lexer.STAR:
		if bothInt {
			li, _ := asInt(left)
			ri, _ := asInt(right)
			if li != 0 && ri != 0 {
				p := li * ri
				if p/ri != li {
					return nil, errors.New("integer overflow")
				}
			}
			return li * ri, nil
		}
		return lf * rf, nil
	case lexer.SLASH:
		if rf == 0 {
			return nil, nil
		}
		if bothInt {
			li, _ := asInt(left)
			ri, _ := asInt(right)
			if ri == 0 {
				return nil, nil
			}
			// SQLite truncates integer division toward zero.
			q := li / ri
			return q, nil
		}
		return lf / rf, nil
	case lexer.PERCENT:
		if rf == 0 {
			return nil, nil
		}
		li, lok := asInt(left)
		ri, rok := asInt(right)
		if !lok || !rok {
			return nil, errors.New("modulo requires integer values")
		}
		if ri == 0 {
			return nil, nil
		}
		return li % ri, nil
	default:
		return nil, errors.New("unknown arithmetic operator")
	}
}

// negateNumeric applies unary minus, preserving integer-ness.
func negateNumeric(v any) (any, error) {
	if isIntegerValue(v) {
		i, ok := asInt(v)
		if !ok {
			return nil, errors.New("unary minus requires a numeric value")
		}
		if i == math.MinInt64 {
			return nil, errors.New("integer overflow")
		}
		return -i, nil
	}
	f, ok := asFloat(v)
	if !ok {
		return nil, errors.New("unary minus requires a numeric value")
	}
	return -f, nil
}

// isIntegerValue reports whether a value carries SQLite's INTEGER class after
// numeric conversion. Text that parses as an integer counts as an integer —
// '3'+1 is 4, not 4.0 — while text that parses only as a real does not.
func isIntegerValue(v any) bool {
	switch x := v.(type) {
	case int, int64, bool:
		return true
	case string:
		_, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return err == nil
	case json.Number:
		_, err := x.Int64()
		return err == nil
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
		return 0, false
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return i, err == nil
	case json.Number:
		i, err := x.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return err == nil && f != 0
	default:
		return false
	}
}

func matchWhereState(s *persistedState, expr ast.Expr, row map[string]any, b *binder) (bool, error) {
	if expr == nil {
		return true, nil
	}
	v, err := evalExprState(s, expr, row, b)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

func evalExprState(s *persistedState, expr ast.Expr, row map[string]any, b *binder) (any, error) {
	switch e := expr.(type) {
	case *ast.BinaryExpr:
		left, err := evalExprState(s, e.Left, row, b)
		if err != nil {
			return nil, err
		}
		right, err := evalExprState(s, e.Right, row, b)
		if err != nil {
			return nil, err
		}
		return evalBinary(left, right, e.Op)
	case *ast.UnaryExpr:
		v, err := evalExprState(s, e.Expr, row, b)
		if err != nil {
			return nil, err
		}
		if e.Op == lexer.MINUS {
			return negateNumeric(v)
		}
		if e.Op == lexer.NOT || strings.EqualFold(e.Op.String(), "UNKNOWN") || fmt.Sprint(e.Op) == "UNKNOWN" {
			return !truthy(v), nil
		}
		return nil, fmt.Errorf("unsupported unary operator: %s", e.Op)
	case *ast.InExpr:
		return evalInExprState(s, e, row, b)
	case *ast.SubqueryExpr:
		return evalScalarSubquery(s, e)
	case *ast.ExistsExpr:
		return evalExistsSubquery(s, e)
	default:
		return evalExpr(expr, row, b)
	}
}

func evalScalarSubquery(s *persistedState, e *ast.SubqueryExpr) (any, error) {
	if s == nil {
		return nil, errors.New("subquery requires database state")
	}
	rows, err := execSelect(s, e.Subq, nil)
	if err != nil {
		return nil, err
	}
	row, ok := rows.Next()
	if !ok || len(row) == 0 {
		return nil, nil
	}
	return row[0], nil
}

func evalExistsSubquery(s *persistedState, e *ast.ExistsExpr) (any, error) {
	if s == nil {
		return nil, errors.New("subquery requires database state")
	}
	rows, err := execSelect(s, e.Subq, nil)
	if err != nil {
		return nil, err
	}
	_, ok := rows.Next()
	if e.Not {
		return !ok, nil
	}
	return ok, nil
}

func evalInExprState(s *persistedState, e *ast.InExpr, row map[string]any, b *binder) (any, error) {
	v, err := evalExprState(s, e.Expr, row, b)
	if err != nil {
		return nil, err
	}
	found := false
	if e.Subq != nil {
		if s == nil {
			return nil, errors.New("subquery requires database state")
		}
		rows, err := execSelect(s, e.Subq, b.args)
		if err != nil {
			return nil, err
		}
		for {
			subrow, ok := rows.Next()
			if !ok {
				break
			}
			if len(subrow) == 0 {
				continue
			}
			if c, ok := compareValues(v, subrow[0], "BINARY"); ok && c == 0 {
				found = true
				break
			}
		}
	} else {
		for _, item := range e.List {
			iv, err := evalExprState(s, item, row, b)
			if err != nil {
				return nil, err
			}
			if c, ok := compareValues(v, iv, "BINARY"); ok && c == 0 {
				found = true
				break
			}
		}
	}
	if e.Not {
		return !found, nil
	}
	return found, nil
}

func hasAggregate(exprs []ast.Expr) bool {
	return slices.ContainsFunc(exprs, hasAggregateExpr)
}

// hasAggregateExpr reports whether an expression contains a bare aggregate
// function call. Window invocations such as sum(x) OVER () are not aggregates:
// they are evaluated per row after grouping.
func hasAggregateExpr(expr ast.Expr) bool {
	found := false
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case *ast.FuncCall:
			if isWindowCall(x) {
				return
			}
			if isAggregateCall(x) {
				found = true
				return
			}
			for _, a := range x.Args {
				walk(a)
			}
		case *ast.BinaryExpr:
			walk(x.Left)
			walk(x.Right)
		case *ast.UnaryExpr:
			walk(x.Expr)
		case *ast.CastExpr:
			walk(x.Expr)
		case *ast.CaseExpr:
			if x.Else != nil {
				walk(x.Else)
			}
			if x.Operand != nil {
				walk(x.Operand)
			}
			for _, w := range x.Whens {
				walk(w.Cond)
				walk(w.Result)
			}
		case *ast.BetweenExpr:
			walk(x.Expr)
		case *ast.IsNullExpr:
			walk(x.Expr)
		case *ast.LikeExpr:
			if x.Expr != nil {
				walk(x.Expr)
			}
			if x.Pattern != nil {
				walk(x.Pattern)
			}
		case *ast.InExpr:
			walk(x.Expr)
		}
	}
	walk(expr)
	return found
}

// isAggregateCall reports whether a function call is a group aggregate.
// min()/max() are aggregates only in their single-argument form; min(a,b,c) is
// an ordinary scalar function.
func isAggregateCall(fn *ast.FuncCall) bool {
	if fn == nil || isWindowCall(fn) {
		return false
	}
	name := funcCallName(fn)
	if name == "min" || name == "max" {
		return len(fn.Args) <= 1
	}
	return isAggregateFuncName(name)
}

// isAggregateFuncName reports whether a function name is an aggregate.
func isAggregateFuncName(name string) bool {
	switch name {
	case "count", "sum", "total", "avg", "min", "max", "group_concat", "string_agg":
		return true
	}
	return false
}

// funcCallName lowercases the trailing identifier of a function call.
func funcCallName(fn *ast.FuncCall) string {
	if fn == nil || fn.Name == nil || len(fn.Name.Parts) == 0 {
		return ""
	}
	return strings.ToLower(fn.Name.Parts[len(fn.Name.Parts)-1].Unquoted)
}

func aggregateRows(s *persistedState, contexts []rowContext, st *ast.SelectStmt, cols []string, exprs, extra []ast.Expr, args []any) ([][]any, []string, [][]any, error) {
	groups := map[string][]rowContext{}
	order := []string{}
	for _, row := range contexts {
		b := binder{args: args}
		ok, err := matchWhereState(s, st.Where, row.Values, &b)
		if err != nil {
			return nil, nil, nil, err
		}
		if !ok {
			continue
		}
		keyParts := []string{"__all__"}
		if len(st.GroupBy) > 0 {
			keyParts = keyParts[:0]
			for _, g := range st.GroupBy {
				v, err := evalExpr(g, row.Values, &binder{args: args})
				if err != nil {
					return nil, nil, nil, err
				}
				keyParts = append(keyParts, fmt.Sprint(v))
			}
		}
		key := strings.Join(keyParts, "\x00")
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	if len(groups) == 0 && len(st.GroupBy) == 0 && hasAggregate(exprs) {
		groups["__all__"] = nil
		order = append(order, "__all__")
	}
	var out [][]any
	var extraOut [][]any
	for _, key := range order {
		group := groups[key]
		var values []any
		for _, expr := range exprs {
			v, err := evalAggregateExpr(expr, group, args)
			if err != nil {
				return nil, nil, nil, err
			}
			values = append(values, v)
		}
		if st.Having != nil {
			hv, err := evalAggregateExpr(st.Having, group, args)
			if err != nil {
				return nil, nil, nil, err
			}
			if !truthy(hv) {
				continue
			}
		}
		var extraVals []any
		for _, expr := range extra {
			v, err := evalAggregateExpr(expr, group, args)
			if err != nil {
				return nil, nil, nil, err
			}
			extraVals = append(extraVals, v)
		}
		out = append(out, values)
		extraOut = append(extraOut, extraVals)
	}
	return out, cols, extraOut, nil
}

func evalAggregateExpr(expr ast.Expr, group []rowContext, args []any) (any, error) {
	switch e := expr.(type) {
	case *ast.BinaryExpr:
		left, err := evalAggregateExpr(e.Left, group, args)
		if err != nil {
			return nil, err
		}
		right, err := evalAggregateExpr(e.Right, group, args)
		if err != nil {
			return nil, err
		}
		return evalBinary(left, right, e.Op)
	case *ast.UnaryExpr:
		v, err := evalAggregateExpr(e.Expr, group, args)
		if err != nil {
			return nil, err
		}
		if e.Op == lexer.MINUS {
			return negateNumeric(v)
		}
		if e.Op == lexer.NOT || strings.EqualFold(e.Op.String(), "UNKNOWN") || fmt.Sprint(e.Op) == "UNKNOWN" {
			return !truthy(v), nil
		}
		return nil, fmt.Errorf("unsupported unary operator: %s", e.Op)
	}
	fn, ok := expr.(*ast.FuncCall)
	if !ok {
		if len(group) == 0 {
			return nil, nil
		}
		return evalExpr(expr, group[0].Values, &binder{args: args})
	}
	// Window invocations are not group aggregates. They are computed after
	// grouping, over the grouped rows, and their slots are filled in by
	// substituteWindowValues.
	if isWindowCall(fn) {
		return nil, nil
	}
	if !isAggregateCall(fn) {
		if len(group) == 0 {
			return nil, nil
		}
		// Scalar functions (including multi-argument min/max) evaluate against
		// the group's first row, matching SQL's bare-column rules.
		return evalFunc(fn, group[0].Values, &binder{args: args})
	}
	name := ""
	if fn.Name != nil && len(fn.Name.Parts) > 0 {
		name = strings.ToLower(fn.Name.Parts[len(fn.Name.Parts)-1].Unquoted)
	}
	switch name {
	case "count":
		if fn.Star {
			return int64(len(group)), nil
		}
		if len(fn.Args) == 0 {
			return nil, errors.New("count expects 1 argument")
		}
		var n int64
		for _, row := range group {
			v, err := evalExpr(fn.Args[0], row.Values, &binder{args: args})
			if err != nil {
				return nil, err
			}
			if v != nil {
				n++
			}
		}
		return n, nil
	case "sum", "avg", "total":
		if len(fn.Args) == 0 {
			return nil, fmt.Errorf("%s expects 1 argument", name)
		}
		sumF := float64(0)
		var sumI int64
		allInt := true
		var n int64
		overflow := false
		for _, row := range group {
			v, err := evalExpr(fn.Args[0], row.Values, &binder{args: args})
			if err != nil {
				return nil, err
			}
			if v == nil {
				continue
			}
			f, ok := sumFloatValue(v)
			if !ok {
				continue
			}
			if !isIntegerValue(v) {
				allInt = false
			}
			if allInt {
				iv, iok := asInt(v)
				if !iok {
					allInt = false
				} else if (iv > 0 && sumI > math.MaxInt64-iv) || (iv < 0 && sumI < math.MinInt64-iv) {
					// SQLite raises an error on integer overflow in sum().
					overflow = true
				} else {
					sumI += iv
				}
			}
			sumF += f
			n++
		}
		if overflow {
			return nil, errors.New("integer overflow")
		}
		if name == "total" {
			return sumF, nil
		}
		if n == 0 {
			return nil, nil
		}
		if name == "avg" {
			// avg always yields a real, even for all-integer input.
			return sumF / float64(n), nil
		}
		if allInt {
			return sumI, nil
		}
		return sumF, nil
	case "min", "max":
		if len(fn.Args) == 0 {
			return nil, fmt.Errorf("%s expects 1 argument", name)
		}
		var best any
		for _, row := range group {
			v, err := evalExpr(fn.Args[0], row.Values, &binder{args: args})
			if err != nil {
				return nil, err
			}
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c, ok := compareValues(v, best, "BINARY")
			if ok && ((name == "min" && c < 0) || (name == "max" && c > 0)) {
				best = v
			}
		}
		return best, nil
	case "group_concat", "string_agg":
		if len(fn.Args) == 0 {
			return nil, fmt.Errorf("%s expects 1 argument", name)
		}
		sep := ","
		if len(fn.Args) > 1 {
			if s, ok := fn.Args[1].(*ast.Literal); ok {
				sep = unquoteSQL(string(s.Raw))
			}
		}
		var parts []string
		for _, row := range group {
			v, err := evalExpr(fn.Args[0], row.Values, &binder{args: args})
			if err != nil {
				return nil, err
			}
			if v == nil {
				continue
			}
			parts = append(parts, stringifySQL(v))
		}
		if len(parts) == 0 {
			return nil, nil
		}
		return strings.Join(parts, sep), nil
	default:
		if len(group) == 0 {
			return nil, nil
		}
		return evalFunc(fn, group[0].Values, &binder{args: args})
	}
}

// stringifySQL renders a value the way SQLite text-concatenates it.
func stringifySQL(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func rowFromOutput(cols []string, values []any) map[string]any {
	row := make(map[string]any, len(cols))
	for i, col := range cols {
		if i < len(values) {
			row[catalog.Normalize(col)] = values[i]
		}
	}
	return row
}

func countPositionalParamsBefore(root any, position int32) int {
	paramType := reflect.TypeOf((*ast.Param)(nil))
	seen := make(map[uintptr]bool)
	count := 0
	var walk func(reflect.Value)
	walk = func(value reflect.Value) {
		if !value.IsValid() {
			return
		}
		if value.Kind() == reflect.Interface {
			if !value.IsNil() {
				walk(value.Elem())
			}
			return
		}
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return
			}
			if value.Type() == paramType {
				param := value.Interface().(*ast.Param)
				if param.Pos() < position && parameterName(param.Raw) == "" {
					count++
				}
				return
			}
			ptr := value.Pointer()
			if seen[ptr] {
				return
			}
			seen[ptr] = true
			walk(value.Elem())
			return
		}
		switch value.Kind() {
		case reflect.Struct:
			for i := 0; i < value.NumField(); i++ {
				walk(value.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < value.Len(); i++ {
				walk(value.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(root))
	return count
}

func applyLimit(rows [][]any, lim *ast.LimitClause, args []any, statement ...any) ([][]any, error) {
	if lim == nil {
		return rows, nil
	}
	count := int64(len(rows))
	offset := int64(0)
	if lim.Count != nil {
		start := 0
		if len(statement) > 0 {
			start = countPositionalParamsBefore(statement[0], lim.Count.Pos())
		}
		v, err := evalExpr(lim.Count, nil, &binder{args: args, pos: start})
		if err != nil {
			return nil, err
		}
		count, _ = asInt(v)
	}
	if lim.Offset != nil {
		start := 0
		if len(statement) > 0 {
			start = countPositionalParamsBefore(statement[0], lim.Offset.Pos())
		}
		v, err := evalExpr(lim.Offset, nil, &binder{args: args, pos: start})
		if err != nil {
			return nil, err
		}
		offset, _ = asInt(v)
	}
	if offset < 0 {
		offset = 0
	}
	if count < 0 {
		count = int64(len(rows))
	}
	if offset > int64(len(rows)) {
		return nil, nil
	}
	end := offset + count
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	return rows[int(offset):int(end)], nil
}

func applyFlatLimit(rows []any, width int, lim *ast.LimitClause, args []any, statement ...any) ([]any, error) {
	if lim == nil || width <= 0 {
		return rows, nil
	}
	rowCount := int64(len(rows) / width)
	count := rowCount
	offset := int64(0)
	if lim.Count != nil {
		v, err := evalExpr(lim.Count, nil, &binder{args: args})
		if err != nil {
			return nil, err
		}
		count, _ = asInt(v)
	}
	if lim.Offset != nil {
		v, err := evalExpr(lim.Offset, nil, &binder{args: args})
		if err != nil {
			return nil, err
		}
		offset, _ = asInt(v)
	}
	if offset < 0 {
		offset = 0
	}
	if count < 0 {
		count = rowCount
	}
	if offset > rowCount {
		return nil, nil
	}
	end := offset + count
	if end > rowCount {
		end = rowCount
	}
	return rows[int(offset)*width : int(end)*width], nil
}

func selectList(tables []*catalog.Table, cols []ast.SelectColumn) ([]string, []ast.Expr, error) {
	var names []string
	var exprs []ast.Expr
	for _, col := range cols {
		if col.Star {
			for _, t := range tables {
				for _, c := range t.Columns {
					names = append(names, c.Name)
					exprs = append(exprs, &ast.Ident{Unquoted: c.Name})
				}
			}
			continue
		}
		name := "expr"
		switch e := col.Expr.(type) {
		case *ast.Ident:
			name = catalog.Normalize(e.Unquoted)
		case *ast.QualifiedIdent:
			if len(e.Parts) > 0 {
				name = catalog.Normalize(e.Parts[len(e.Parts)-1].Unquoted)
			}
		}
		if col.Alias != nil {
			name = col.Alias.Unquoted
		}
		names = append(names, name)
		exprs = append(exprs, col.Expr)
	}
	if len(names) == 0 {
		return nil, nil, errors.New("empty select list")
	}
	return names, exprs, nil
}

func tableNameFromRefs(refs []ast.TableRef) (string, error) {
	if len(refs) != 1 {
		return "", errors.New("v1 supports exactly one table")
	}
	switch r := refs[0].(type) {
	case *ast.SimpleTable:
		return qname(r.Name), nil
	default:
		return "", fmt.Errorf("unsupported table reference in v1: %T", refs[0])
	}
}

func refSimpleName(ref ast.TableRef) string {
	if r, ok := ref.(*ast.SimpleTable); ok {
		return qname(r.Name)
	}
	return ""
}

func qname(q *ast.QualifiedIdent) string {
	if q == nil || len(q.Parts) == 0 {
		return ""
	}
	return catalog.Normalize(q.Parts[len(q.Parts)-1].Unquoted)
}
