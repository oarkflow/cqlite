package db

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/oarkflow/sqlparser"
	"github.com/oarkflow/sqlparser/ast"
	"github.com/oarkflow/sqlparser/lexer"
	"github.com/sujit/sqlite/internal/catalog"
	isql "github.com/sujit/sqlite/internal/sql"
)

const primaryKeyNamePlaceholder = "__primary_key__"

var simpleDMLCache sync.Map

type simpleDMLPlan struct {
	kind   byte
	table  string
	col    string
	where  string
	inc    int64
	params int
}

func parseStatementCached(query string) (ast.Statement, error) {
	return sqlparser.ParseStatement(query)
}

func execute(ctx context.Context, s *persistedState, query string, args ...any) (Result, error) {
	if res, ok, err := execPragma(s, query); ok || err != nil {
		return res, err
	}
	parseQuery, strictTable := stripSQLiteStrict(query)
	stmt, err := parseStatementCached(parseQuery)
	if err != nil {
		if res, ok, manualErr := execManualCreateIndex(s, query); ok {
			return res, manualErr
		}
		return Result{}, err
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}
	switch st := stmt.(type) {
	case *ast.CreateTableStmt:
		return execCreateTable(s, st, strictTable, query)
	case *ast.DropTableStmt:
		return execDropTable(s, st)
	case *ast.CreateIndexStmt:
		return execCreateIndex(s, st, query)
	case *ast.DropIndexStmt:
		return execDropIndex(s, st)
	case *ast.AlterTableStmt:
		return execAlterTable(s, st)
	case *ast.InsertStmt:
		return execInsert(s, st, args)
	case *ast.UpdateStmt:
		return execUpdate(s, st, args)
	case *ast.DeleteStmt:
		return execDelete(s, st, args)
	case *ast.ExplainStmt:
		return Result{}, nil
	case *ast.TransactionStmt:
		return Result{}, nil
	default:
		return Result{}, fmt.Errorf("statement is not executable in v1: %T", stmt)
	}
}

func fastDirectDML(s *persistedState, stmt ast.Statement, args []any) (Result, bool, error) {
	if ins, ok := stmt.(*ast.InsertStmt); ok {
		return fastInsertDML(s, ins, args)
	}
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
	idv, err := evalExpr(where.Right, nil, &binder{args: args})
	if err != nil {
		return Result{}, true, err
	}
	id, ok := asInt(idv)
	if !ok {
		return Result{RowsAffected: 0}, true, nil
	}
	data := s.Tables[t.Name]
	pos, ok := data.rowids[id]
	if !ok || data.Rows[pos].Deleted != 0 {
		return Result{RowsAffected: 0}, true, nil
	}
	asn := upd.Set[0]
	col, _, ok := t.Column(asn.Column.Unquoted)
	if !ok {
		return Result{}, true, fmt.Errorf("unknown column: %s", asn.Column.Unquoted)
	}
	if !canFastUpdateColumn(s, t, col.Name) {
		return Result{}, false, nil
	}
	nextValue, ok, err := fastUpdateValue(asn.Value, data.Rows[pos].Values)
	if err != nil || !ok {
		return Result{}, ok, err
	}
	cv, err := coerce(col, nextValue, t.Strict)
	if err != nil {
		return Result{}, true, err
	}
	data.Rows[pos].Values[col.Name] = cv
	return Result{RowsAffected: 1}, true, nil
}

func fastSimpleDML(s *persistedState, query string, args []any) (Result, bool, error) {
	plan, ok := simplePlan(query)
	if !ok || plan.params != len(args) {
		return Result{}, false, nil
	}
	switch plan.kind {
	case 'i':
		return fastInsertValues(s, plan.table, args)
	case 'u':
		return fastIncrementUpdate(s, simpleIncrementUpdate{
			table: plan.table,
			col:   plan.col,
			where: plan.where,
			inc:   plan.inc,
		}, args)
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
	data.Rows = append(data.Rows, rowVersion{RowID: rowid, Values: row, Created: s.NextTx})
	if rowid >= t.NextRowID {
		t.NextRowID = rowid + 1
	}
	data.addRuntime(t, len(data.Rows)-1)
	return Result{RowsAffected: 1, LastInsertID: rowid}, true, nil
}

type simpleIncrementUpdate struct {
	table string
	col   string
	where string
	inc   int64
}

func fastIncrementUpdate(s *persistedState, upd simpleIncrementUpdate, args []any) (Result, bool, error) {
	if len(args) != 1 {
		return Result{}, false, nil
	}
	t, ok := s.Catalog.Table(upd.table)
	if !ok {
		return Result{}, true, fmt.Errorf("table does not exist: %s", upd.table)
	}
	pk := integerPrimaryKeyColumn(t)
	if pk == "" || upd.where != pk {
		return Result{}, false, nil
	}
	col, _, ok := t.Column(upd.col)
	if !ok {
		return Result{}, true, fmt.Errorf("unknown column: %s", upd.col)
	}
	if !canFastUpdateColumn(s, t, col.Name) {
		return Result{}, false, nil
	}
	id, ok := asInt(args[0])
	if !ok {
		return Result{RowsAffected: 0}, true, nil
	}
	data := s.Tables[t.Name]
	pos, ok := data.rowids[id]
	if !ok || data.Rows[pos].Deleted != 0 {
		return Result{RowsAffected: 0}, true, nil
	}
	cur, ok := asInt(data.Rows[pos].Values[col.Name])
	if !ok {
		return Result{}, false, nil
	}
	cv, err := coerce(col, cur+upd.inc, t.Strict)
	if err != nil {
		return Result{}, true, err
	}
	data.Rows[pos].Values[col.Name] = cv
	return Result{RowsAffected: 1}, true, nil
}

func fastInsertDML(s *persistedState, st *ast.InsertStmt, args []any) (Result, bool, error) {
	if st.OnConflictDoNothing || st.Ignore || len(st.OnConflictUpdate) > 0 || len(st.Values) != 1 {
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
	data.Rows = append(data.Rows, rowVersion{RowID: rowid, Values: row, Created: s.NextTx})
	if rowid >= t.NextRowID {
		t.NextRowID = rowid + 1
	}
	data.addRuntime(t, len(data.Rows)-1)
	return Result{RowsAffected: 1, LastInsertID: rowid}, true, nil
}

func validateInsertFast(s *persistedState, t *catalog.Table, data *tableData, values map[string]any, rowid int64) error {
	for _, col := range t.Columns {
		v := values[col.Name]
		if col.NotNull && v == nil {
			return fmt.Errorf("column %s cannot be null", col.Name)
		}
		if col.PrimaryKey || col.Unique {
			if col.PrimaryKey && col.Name == integerPrimaryKeyColumn(t) {
				if data.rowids == nil {
					data.rebuildRuntime(t)
				}
				if pos, ok := data.rowids[rowid]; ok && pos >= 0 && pos < len(data.Rows) && data.Rows[pos].Deleted == 0 {
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
		if len(idx.Columns) == 0 {
			return validateRow(s, t, data, values, 0)
		}
		if data.indexes == nil {
			data.rebuildRuntime(t)
		}
		key := indexRuntimeKey(idx, values)
		for _, pos := range data.indexes[idx.Name][key] {
			if pos >= 0 && pos < len(data.Rows) && data.Rows[pos].Deleted == 0 {
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

func canFastUpdateColumn(s *persistedState, t *catalog.Table, col string) bool {
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

func queryRows(ctx context.Context, s *persistedState, query string, args ...any) (*Rows, error) {
	if rows, ok, err := queryPragma(s, query); ok || err != nil {
		return rows, err
	}
	if rows, ok := queryPlan(s, query); ok {
		return rows, nil
	}
	stmt, err := parseStatementCached(query)
	if err != nil {
		return nil, err
	}
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
}

func execCreateTable(s *persistedState, st *ast.CreateTableStmt, strict bool, rawSQL string) (Result, error) {
	name := qname(st.Table)
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
			Collation:  "BINARY",
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
			c.Check = "check"
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
		}
		if strict && !catalog.StrictTypeAllowed(c.Type) {
			return Result{}, fmt.Errorf("unsupported STRICT column type: %s", c.DeclType)
		}
		t.Columns = append(t.Columns, c)
	}
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

func execDropTable(s *persistedState, st *ast.DropTableStmt) (Result, error) {
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

func execCreateIndex(s *persistedState, st *ast.CreateIndexStmt, rawSQL string) (Result, error) {
	t, ok := s.Catalog.Table(qname(st.Table))
	if !ok {
		return Result{}, fmt.Errorf("table does not exist: %s", qname(st.Table))
	}
	cols := make([]string, 0, len(st.Columns))
	for _, c := range st.Columns {
		name := catalog.Normalize(c.Name.Unquoted)
		if _, _, ok := t.Column(name); !ok {
			return Result{}, fmt.Errorf("unknown index column: %s", name)
		}
		cols = append(cols, name)
	}
	idx := catalog.Index{Name: catalog.Normalize(st.Name.Unquoted), Columns: cols, Unique: st.Type == ast.UniqueConstraint, SQL: strings.TrimSpace(rawSQL)}
	t.Indexes = append(t.Indexes, idx)
	if data := s.Tables[t.Name]; data != nil {
		data.rebuildRuntime(t)
	}
	t.SchemaVers++
	s.Pragmas.SchemaVer++
	return Result{}, nil
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
	parts := strings.SplitN(rest, " on ", 2)
	if len(parts) != 2 {
		parts = strings.SplitN(rest, " ON ", 2)
	}
	if len(parts) != 2 {
		return Result{}, true, errors.New("invalid CREATE INDEX")
	}
	idxName := catalog.Normalize(parts[0])
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
	t.Indexes = append(t.Indexes, idx)
	if data := s.Tables[t.Name]; data != nil {
		data.rebuildRuntime(t)
	}
	t.SchemaVers++
	s.Pragmas.SchemaVer++
	return Result{}, true, nil
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
				s.Tables[t.Name].Rows[i].Values[col.Name] = col.Default
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
				delete(s.Tables[t.Name].Rows[i].Values, name)
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

func execInsert(s *persistedState, st *ast.InsertStmt, args []any) (Result, error) {
	t, data, err := tableForWrite(s, qname(st.Table))
	if err != nil {
		return Result{}, err
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
	var affected int64
	var last int64
	bind := binder{args: args}
	for _, values := range st.Values {
		if len(values) != len(cols) {
			return Result{}, errors.New("insert column/value count mismatch")
		}
		row := make(map[string]any, len(t.Columns))
		for _, col := range t.Columns {
			row[col.Name] = col.Default
		}
		for i, expr := range values {
			v, err := evalExpr(expr, nil, &bind)
			if err != nil {
				return Result{}, err
			}
			cv, err := coerce(cols[i], v, t.Strict)
			if err != nil {
				return Result{}, err
			}
			row[cols[i].Name] = cv
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
			if st.OnConflictDoNothing || st.Ignore {
				continue
			}
			if len(st.OnConflictUpdate) == 0 {
				return Result{}, err
			}
			if err := applyUpsert(s, t, data, row, st.OnConflictUpdate, args); err != nil {
				return Result{}, err
			}
			affected++
			continue
		}
		last = rowid
		data.Rows = append(data.Rows, rowVersion{RowID: rowid, Values: row, Created: s.NextTx})
		if rowid >= t.NextRowID {
			t.NextRowID = rowid + 1
		}
		data.addRuntime(t, len(data.Rows)-1)
		affected++
	}
	return Result{RowsAffected: affected, LastInsertID: last}, nil
}

func applyUpsert(s *persistedState, t *catalog.Table, data *tableData, incoming map[string]any, assigns []ast.Assignment, args []any) error {
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		conflict := false
		for _, col := range t.Columns {
			if (col.PrimaryKey || col.Unique) && reflect.DeepEqual(row.Values[col.Name], incoming[col.Name]) {
				conflict = true
				break
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
		row.Values = next
		data.rebuildRuntime(t)
		return nil
	}
	return errors.New("upsert conflict row not found")
}

func execUpdate(s *persistedState, st *ast.UpdateStmt, args []any) (Result, error) {
	name, err := tableNameFromRefs(st.Tables)
	if err != nil {
		return Result{}, err
	}
	t, data, err := tableForWrite(s, name)
	if err != nil {
		return Result{}, err
	}
	bind := binder{args: args}
	var affected int64
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		whereBind := bind
		ok, err := matchWhere(st.Where, row.Values, &whereBind)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			continue
		}
		next := cloneValues(row.Values)
		for _, asn := range st.Set {
			col, _, ok := t.Column(asn.Column.Unquoted)
			if !ok {
				return Result{}, fmt.Errorf("unknown column: %s", asn.Column.Unquoted)
			}
			v, err := evalExpr(asn.Value, next, &bind)
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
		row.Values = next
		data.rebuildRuntime(t)
		affected++
	}
	return Result{RowsAffected: affected}, nil
}

func execDelete(s *persistedState, st *ast.DeleteStmt, args []any) (Result, error) {
	name, err := tableNameFromRefs(st.From)
	if err != nil {
		return Result{}, err
	}
	_, data, err := tableForWrite(s, name)
	if err != nil {
		return Result{}, err
	}
	bind := binder{args: args}
	var affected int64
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		whereBind := bind
		ok, err := matchWhere(st.Where, row.Values, &whereBind)
		if err != nil {
			return Result{}, err
		}
		if ok {
			if s.Pragmas.ForeignKeys {
				if err := validateDeleteReferences(s, name, row.Values); err != nil {
					return Result{}, err
				}
			}
			row.Deleted = s.NextTx
			data.rebuildRuntime(s.Catalog.Tables[catalog.Normalize(name)])
			affected++
		}
	}
	return Result{RowsAffected: affected}, nil
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
					switch fk.OnDelete {
					case "cascade":
						row.Deleted = s.NextTx
					case "set null":
						for _, col := range fk.Columns {
							row.Values[col] = nil
						}
					case "set default":
						for _, col := range fk.Columns {
							if def, _, ok := child.Column(col); ok {
								row.Values[col] = def.Default
							}
						}
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
				switch fk.OnUpdate {
				case "cascade":
					for i, refCol := range refCols {
						row.Values[fk.Columns[i]] = next[refCol]
					}
				case "set null":
					for _, col := range fk.Columns {
						row.Values[col] = nil
					}
				case "set default":
					for _, col := range fk.Columns {
						if def, _, ok := child.Column(col); ok {
							row.Values[col] = def.Default
						}
					}
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
	if len(st.From) == 1 {
		if name := refSimpleName(st.From[0]); catalog.Normalize(name) == "sqlite_schema" {
			return selectVirtualSchema(s, st, args)
		}
	}
	if rows, ok, err := fastJoinAggregateSelect(s, st, args); ok || err != nil {
		return rows, err
	}
	if rows, ok, err := fastSingleTableSelect(s, st, args); ok || err != nil {
		return rows, err
	}
	tables, contexts, err := buildRowSetForSelect(s, st.From, st.Where, args)
	if err != nil {
		return nil, err
	}
	bind := binder{args: args}
	cols, exprs, err := selectList(tables, st.Columns)
	if err != nil {
		return nil, err
	}
	var filtered []rowContext
	for _, row := range contexts {
		whereBind := bind
		ok, err := matchWhere(st.Where, row.Values, &whereBind)
		if err != nil {
			return nil, err
		}
		if ok {
			filtered = append(filtered, row)
		}
	}
	var out [][]any
	if len(st.GroupBy) == 0 && !hasAggregate(exprs) && len(st.OrderBy) > 0 {
		sort.SliceStable(filtered, func(i, j int) bool {
			for _, item := range st.OrderBy {
				lv, _ := evalExpr(item.Expr, filtered[i].Values, &binder{args: args})
				rv, _ := evalExpr(item.Expr, filtered[j].Values, &binder{args: args})
				c, _ := compareValues(lv, rv, "BINARY")
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
	for _, row := range filtered {
		values := make([]any, 0, len(exprs))
		for _, expr := range exprs {
			v, err := evalExpr(expr, row.Values, &bind)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		out = append(out, values)
	}
	if len(st.GroupBy) > 0 || hasAggregate(exprs) {
		out, cols, err = aggregateRows(contexts, st, cols, exprs, args)
		if err != nil {
			return nil, err
		}
	}
	if (len(st.GroupBy) > 0 || hasAggregate(exprs)) && len(st.OrderBy) > 0 {
		sort.SliceStable(out, func(i, j int) bool {
			li := rowFromOutput(cols, out[i])
			lj := rowFromOutput(cols, out[j])
			for _, item := range st.OrderBy {
				lv, _ := evalExpr(item.Expr, li, &binder{args: args})
				rv, _ := evalExpr(item.Expr, lj, &binder{args: args})
				c, _ := compareValues(lv, rv, "BINARY")
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
	out, err = applyLimit(out, st.Limit, args)
	if err != nil {
		return nil, err
	}
	return isql.NewRows(cols, out), nil
}

func explain(st *ast.ExplainStmt) *Rows {
	return isql.NewRows([]string{"node", "detail"}, [][]any{{"parse", fmt.Sprintf("%T", st.Stmt)}, {"execute", "single-table v1 executor"}})
}

func fastSingleTableSelect(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, bool, error) {
	if len(st.From) != 1 || len(st.GroupBy) > 0 || st.Having != nil || st.SetOp != nil || hasAggregateSelect(st.Columns) {
		return nil, false, nil
	}
	simple, ok := st.From[0].(*ast.SimpleTable)
	if !ok {
		return nil, false, nil
	}
	name := qname(simple.Name)
	t, ok := s.Catalog.Table(name)
	if !ok {
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
	predicate, ok, err := fastPredicate(st.Where, args)
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
			if row.Deleted != 0 || !predicate(row) {
				continue
			}
			for _, p := range projectors {
				out = append(out, p(row))
			}
		}
	}
	out, err = applyFlatLimit(out, width, st.Limit, args)
	if err != nil {
		return nil, true, err
	}
	return isql.NewFlatRows(cols, out), true, nil
}

func fastJoinAggregateSelect(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, bool, error) {
	if len(args) != 0 || len(st.From) != 1 || st.Where != nil || st.Having != nil || st.Limit != nil || st.SetOp != nil {
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
	groupValueByJoin := make(map[any]any, len(groupData.Rows))
	for _, row := range groupData.Rows {
		if row.Deleted != 0 {
			continue
		}
		key, ok := comparableKey(row.Values[groupJoinCol])
		if !ok {
			return nil, false, nil
		}
		groupValueByJoin[key] = row.Values[groupCol]
	}
	type aggregate struct {
		key   any
		count int64
		sum   float64
	}
	byGroup := make(map[any]aggregate, len(groupValueByJoin))
	for _, row := range sumData.Rows {
		if row.Deleted != 0 {
			continue
		}
		joinKey, ok := comparableKey(row.Values[sumJoinCol])
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
		agg.key = groupValue
		agg.count++
		if score, ok := asFloat(row.Values[sumCol]); ok {
			agg.sum += score
		}
		byGroup[key] = agg
	}
	groups := make([]aggregate, 0, len(byGroup))
	for _, agg := range byGroup {
		groups = append(groups, agg)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		c, ok := compareValues(groups[i].key, groups[j].key, "BINARY")
		if !ok {
			return fmt.Sprint(groups[i].key) < fmt.Sprint(groups[j].key)
		}
		if len(st.OrderBy) == 1 && st.OrderBy[0].Desc {
			return c > 0
		}
		return c < 0
	})
	out := make([]any, 0, len(groups)*3)
	for _, agg := range groups {
		out = append(out, agg.key, agg.count, agg.sum)
	}
	return isql.NewFlatRows([]string{groupCol, "expr", "expr"}, out), true, nil
}

func comparableKey(v any) (any, bool) {
	switch x := v.(type) {
	case nil, int, int64, float64, string, bool:
		return x, true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
		if f, err := x.Float64(); err == nil {
			return f, true
		}
		return nil, false
	default:
		return nil, false
	}
}

func fastOrderCovered(order []fastOrderTerm, cols []string) bool {
	for _, ord := range order {
		found := false
		for _, col := range cols {
			if catalog.Normalize(col) == ord.col {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func fastProjectors(t *catalog.Table, cols []ast.SelectColumn) ([]string, []func(rowVersion) any, bool) {
	var names []string
	var projectors []func(rowVersion) any
	for _, sel := range cols {
		if sel.Star {
			for _, col := range t.Columns {
				c := col.Name
				names = append(names, c)
				projectors = append(projectors, func(row rowVersion) any { return row.Values[c] })
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
		if _, _, ok := t.Column(col); !ok {
			return nil, nil, false
		}
		c := col
		names = append(names, name)
		projectors = append(projectors, func(row rowVersion) any { return row.Values[c] })
	}
	return names, projectors, true
}

func fastPredicate(expr ast.Expr, args []any) (func(rowVersion) bool, bool, error) {
	if expr == nil {
		return func(rowVersion) bool { return true }, true, nil
	}
	be, ok := expr.(*ast.BinaryExpr)
	if !ok {
		return nil, false, nil
	}
	col := exprColumnName(be.Left)
	valueExpr := be.Right
	if col == "" {
		col = exprColumnName(be.Right)
		valueExpr = be.Left
	}
	if col == "" {
		return nil, false, nil
	}
	val, err := evalExpr(valueExpr, nil, &binder{args: args})
	if err != nil {
		return nil, true, err
	}
	return func(row rowVersion) bool {
		left := any(nil)
		if col == "rowid" || col == "_rowid_" || col == "oid" {
			left = row.RowID
		} else {
			left = row.Values[col]
		}
		c, ok := compareValues(left, val, "BINARY")
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
	col, val, ok, err := indexedEqualityPredicate(where, args)
	if err != nil || !ok {
		return nil
	}
	t, _ := s.Catalog.Table(table)
	if pk := integerPrimaryKeyColumn(t); pk == col || col == "rowid" || col == "_rowid_" || col == "oid" {
		id, ok := asInt(val)
		if !ok {
			return []int{}
		}
		pos, ok := data.rowids[id]
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
	return data.indexes[idxName][runtimeIndexLookupKey(idx, val)]
}

func runtimeIndexLookupKey(idx catalog.Index, val any) string {
	if len(idx.Columns) == 1 {
		return runtimeValueKey(val)
	}
	return indexRuntimeKey(idx, map[string]any{idx.Columns[0]: val})
}

type fastOrderTerm struct {
	col  string
	desc bool
}

func fastOrder(items []ast.OrderByItem) ([]fastOrderTerm, bool) {
	out := make([]fastOrderTerm, 0, len(items))
	for _, item := range items {
		col := exprColumnName(item.Expr)
		if col == "" {
			return nil, false
		}
		out = append(out, fastOrderTerm{col: col, desc: item.Desc})
	}
	return out, true
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
			if idx := usableIndexForWhere(s, name, sel.Where); idx != "" {
				detail = "SEARCH " + name + " USING INDEX " + idx
			}
			rows = append(rows, []any{int64(len(rows)), int64(0), int64(0), detail})
		case *ast.JoinTable:
			walk(r.Left)
			walk(r.Right)
		}
	}
	for _, ref := range sel.From {
		walk(ref)
	}
	if len(sel.OrderBy) > 0 {
		rows = append(rows, []any{int64(len(rows)), int64(0), int64(0), "USE TEMP B-TREE FOR ORDER BY"})
	}
	if len(sel.GroupBy) > 0 {
		rows = append(rows, []any{int64(len(rows)), int64(0), int64(0), "USE TEMP B-TREE FOR GROUP BY"})
	}
	return isql.NewRows([]string{"id", "parent", "notused", "detail"}, rows), true
}

func usableIndexForWhere(s *persistedState, table string, expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	if be, ok := expr.(*ast.BinaryExpr); ok {
		if col := exprColumnName(be.Left); col != "" {
			return usableIndexForColumn(s, table, col)
		}
		if col := exprColumnName(be.Right); col != "" {
			return usableIndexForColumn(s, table, col)
		}
	}
	return ""
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

func queryPragma(s *persistedState, query string) (*Rows, bool, error) {
	name, value, ok := parsePragma(query)
	if !ok || value != "" {
		return nil, ok, nil
	}
	switch name {
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

func pragmaForeignKeyList(s *persistedState, table string) *Rows {
	t, ok := s.Catalog.Table(table)
	cols := []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"}
	if !ok {
		return isql.NewRows(cols, nil)
	}
	var rows [][]any
	for i, fk := range t.ForeignKeys {
		refCols := resolveFKColumnsForPragma(fk.RefColumns)
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
	cols, exprs, err := selectList([]*catalog.Table{{Columns: []catalog.Column{{Name: "type"}, {Name: "name"}, {Name: "tbl_name"}, {Name: "rootpage"}, {Name: "sql"}}}}, st.Columns)
	if err != nil {
		return nil, err
	}
	var out [][]any
	for _, row := range contexts {
		ok, err := matchWhere(st.Where, row.Values, &binder{args: args})
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
			if col, val, ok, err := indexedEqualityPredicate(where, args); err != nil {
				return nil, nil, err
			} else if ok {
				if rows, used, err := buildIndexedRows(s, simple, col, val); used || err != nil {
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

func buildIndexedRows(s *persistedState, simple *ast.SimpleTable, col string, val any) ([]rowContext, bool, error) {
	name := qname(simple.Name)
	t, ok := s.Catalog.Table(name)
	if !ok {
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
		pos, ok := data.rowids[id]
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
	positions := data.indexes[idxName][runtimeIndexLookupKey(idx, val)]
	rows := make([]rowContext, 0, len(positions))
	for _, pos := range positions {
		if pos < 0 || pos >= len(data.Rows) || data.Rows[pos].Deleted != 0 {
			continue
		}
		rows = append(rows, rowToContext(name, simple, t, data.Rows[pos]))
	}
	return rows, true, nil
}

func indexedEqualityPredicate(expr ast.Expr, args []any) (string, any, bool, error) {
	be, ok := expr.(*ast.BinaryExpr)
	if !ok || be.Op != lexer.EQ {
		return "", nil, false, nil
	}
	if col := exprColumnName(be.Left); col != "" {
		v, err := evalExpr(be.Right, nil, &binder{args: args})
		return col, v, err == nil, err
	}
	if col := exprColumnName(be.Right); col != "" {
		v, err := evalExpr(be.Left, nil, &binder{args: args})
		return col, v, err == nil, err
	}
	return "", nil, false, nil
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
	t, ok := s.Catalog.Table(table)
	if !ok {
		return ""
	}
	for _, idx := range t.Indexes {
		if len(idx.Columns) > 0 && idx.Columns[0] == col {
			return idx.Name
		}
	}
	return ""
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
	t, ok := s.Catalog.Table(name)
	if !ok {
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

func rowToContext(name string, r *ast.SimpleTable, t *catalog.Table, row rowVersion) rowContext {
	alias := name
	if r.Alias != nil {
		alias = catalog.Normalize(r.Alias.Unquoted)
	}
	vals := make(map[string]any, len(t.Columns)*3+9)
	for _, col := range t.Columns {
		v := row.Values[col.Name]
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
	for _, col := range t.Columns {
		v := values[col.Name]
		if col.NotNull && v == nil {
			return fmt.Errorf("column %s cannot be null", col.Name)
		}
		if col.PrimaryKey || col.Unique {
			for _, row := range data.Rows {
				if row.Deleted == 0 && row.RowID != self && reflect.DeepEqual(row.Values[col.Name], v) {
					return fmt.Errorf("unique constraint failed: %s.%s", t.Name, col.Name)
				}
			}
		}
	}
	for _, idx := range t.Indexes {
		if !idx.Unique {
			continue
		}
		for _, row := range data.Rows {
			if row.Deleted != 0 || row.RowID == self {
				continue
			}
			same := true
			for _, col := range idx.Columns {
				same = same && reflect.DeepEqual(row.Values[col], values[col])
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

func validateForeignKey(s *persistedState, fk catalog.ForeignKey, values map[string]any) error {
	refTable, ok := s.Catalog.Table(fk.RefTable)
	if !ok {
		return fmt.Errorf("foreign key mismatch: missing table %s", fk.RefTable)
	}
	refData := s.Tables[refTable.Name]
	refCols := resolveFKColumns(refTable, fk.RefColumns)
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
	out := append([]string(nil), cols...)
	for i, col := range out {
		if col == primaryKeyNamePlaceholder {
			out[i] = primaryKeyColumn(t)
		}
	}
	return out
}

func primaryKeyColumn(t *catalog.Table) string {
	for _, col := range t.Columns {
		if col.PrimaryKey {
			return col.Name
		}
	}
	if len(t.Columns) > 0 {
		return t.Columns[0].Name
	}
	return ""
}

func integerPrimaryKeyColumn(t *catalog.Table) string {
	for _, col := range t.Columns {
		if col.PrimaryKey && col.Type == catalog.Integer {
			return col.Name
		}
	}
	return ""
}

func coerce(col catalog.Column, v any, strict bool) (any, error) {
	if v == nil {
		if col.NotNull {
			return nil, fmt.Errorf("column %s cannot be null", col.Name)
		}
		return nil, nil
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
		return fmt.Sprint(v), nil
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
		if i, ok := asInt(v); ok {
			return i
		}
	case catalog.Real:
		if f, ok := asFloat(v); ok {
			return f
		}
	case catalog.Text:
		switch x := v.(type) {
		case []byte:
			return string(x)
		default:
			return fmt.Sprint(v)
		}
	case catalog.Numeric:
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
	args []any
	pos  int
}

func (b *binder) next() (any, error) {
	if b.pos >= len(b.args) {
		return nil, errors.New("not enough bind parameters")
	}
	v := b.args[b.pos]
	b.pos++
	return v, nil
}

func evalExpr(expr ast.Expr, row map[string]any, b *binder) (any, error) {
	switch e := expr.(type) {
	case *ast.Literal:
		return literal(e)
	case *ast.NullLit:
		return nil, nil
	case *ast.Param:
		return b.next()
	case *ast.Ident:
		return row[catalog.Normalize(e.Unquoted)], nil
	case *ast.QualifiedIdent:
		if len(e.Parts) == 0 {
			return nil, errors.New("empty identifier")
		}
		var parts []string
		for _, p := range e.Parts {
			parts = append(parts, catalog.Normalize(p.Unquoted))
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
		ok := like(fmt.Sprint(v), fmt.Sprint(pat))
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
			switch x := v.(type) {
			case int64:
				return -x, nil
			case float64:
				return -x, nil
			}
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
		return arithmetic(left, right, op)
	case lexer.DBAR:
		return fmt.Sprint(left) + fmt.Sprint(right), nil
	}
	return nil, fmt.Errorf("unsupported binary operator: %s", op)
}

func compare(left, right any) (int, bool) {
	return compareValues(left, right, "BINARY")
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

func evalFunc(fn *ast.FuncCall, row map[string]any, b *binder) (any, error) {
	name := ""
	if fn.Name != nil && len(fn.Name.Parts) > 0 {
		name = strings.ToLower(fn.Name.Parts[len(fn.Name.Parts)-1].Unquoted)
	}
	args := make([]any, 0, len(fn.Args))
	for _, expr := range fn.Args {
		v, err := evalExpr(expr, row, b)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	switch name {
	case "lower":
		if len(args) != 1 {
			return nil, errors.New("lower expects 1 argument")
		}
		return strings.ToLower(fmt.Sprint(args[0])), nil
	case "upper":
		if len(args) != 1 {
			return nil, errors.New("upper expects 1 argument")
		}
		return strings.ToUpper(fmt.Sprint(args[0])), nil
	case "length":
		if len(args) != 1 {
			return nil, errors.New("length expects 1 argument")
		}
		if b, ok := args[0].([]byte); ok {
			return int64(len(b)), nil
		}
		return int64(len(fmt.Sprint(args[0]))), nil
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
	case "json_valid":
		if len(args) != 1 {
			return nil, errors.New("json_valid expects 1 argument")
		}
		var js any
		if err := json.Unmarshal([]byte(fmt.Sprint(args[0])), &js); err == nil {
			return int64(1), nil
		}
		return int64(0), nil
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
	case "abs":
		if len(args) != 1 {
			return nil, errors.New("abs expects 1 argument")
		}
		f, ok := asFloat(args[0])
		if !ok {
			return nil, errors.New("abs expects numeric argument")
		}
		if f < 0 {
			f = -f
		}
		return f, nil
	case "count", "sum", "avg", "min", "max":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported function: %s", name)
	}
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

func like(value, pattern string) bool {
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		if j == len(pattern) {
			return i == len(value)
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

func arithmetic(left, right any, op lexer.TokenType) (any, error) {
	lf, lok := asFloat(left)
	rf, rok := asFloat(right)
	if !lok || !rok {
		return nil, errors.New("arithmetic requires numeric values")
	}
	switch op {
	case lexer.PLUS:
		return lf + rf, nil
	case lexer.MINUS:
		return lf - rf, nil
	case lexer.STAR:
		return lf * rf, nil
	case lexer.SLASH:
		if rf == 0 {
			return nil, errors.New("division by zero")
		}
		return lf / rf, nil
	case lexer.PERCENT:
		li, lok := asInt(left)
		ri, rok := asInt(right)
		if !lok || !rok {
			return nil, errors.New("modulo requires integer values")
		}
		if ri == 0 {
			return nil, errors.New("division by zero")
		}
		return li % ri, nil
	default:
		return nil, errors.New("unknown arithmetic operator")
	}
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

func matchWhere(expr ast.Expr, row map[string]any, b *binder) (bool, error) {
	if expr == nil {
		return true, nil
	}
	v, err := evalExpr(expr, row, b)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

func hasAggregate(exprs []ast.Expr) bool {
	for _, expr := range exprs {
		if fn, ok := expr.(*ast.FuncCall); ok {
			name := ""
			if fn.Name != nil && len(fn.Name.Parts) > 0 {
				name = strings.ToLower(fn.Name.Parts[len(fn.Name.Parts)-1].Unquoted)
			}
			switch name {
			case "count", "sum", "avg", "min", "max":
				return true
			}
		}
	}
	return false
}

func aggregateRows(contexts []rowContext, st *ast.SelectStmt, cols []string, exprs []ast.Expr, args []any) ([][]any, []string, error) {
	groups := map[string][]rowContext{}
	order := []string{}
	for _, row := range contexts {
		b := binder{args: args}
		ok, err := matchWhere(st.Where, row.Values, &b)
		if err != nil {
			return nil, nil, err
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
					return nil, nil, err
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
	for _, key := range order {
		group := groups[key]
		var values []any
		for _, expr := range exprs {
			v, err := evalAggregateExpr(expr, group, args)
			if err != nil {
				return nil, nil, err
			}
			values = append(values, v)
		}
		if st.Having != nil {
			hrow := rowFromOutput(cols, values)
			hv, err := evalExpr(st.Having, hrow, &binder{args: args})
			if err != nil {
				return nil, nil, err
			}
			if !truthy(hv) {
				continue
			}
		}
		out = append(out, values)
	}
	return out, cols, nil
}

func evalAggregateExpr(expr ast.Expr, group []rowContext, args []any) (any, error) {
	fn, ok := expr.(*ast.FuncCall)
	if !ok {
		if len(group) == 0 {
			return nil, nil
		}
		return evalExpr(expr, group[0].Values, &binder{args: args})
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
	case "sum", "avg":
		var sum float64
		var n int64
		for _, row := range group {
			v, err := evalExpr(fn.Args[0], row.Values, &binder{args: args})
			if err != nil {
				return nil, err
			}
			if f, ok := asFloat(v); ok {
				sum += f
				n++
			}
		}
		if n == 0 {
			return nil, nil
		}
		if name == "avg" {
			return sum / float64(n), nil
		}
		return sum, nil
	case "min", "max":
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
	default:
		if len(group) == 0 {
			return nil, nil
		}
		return evalFunc(fn, group[0].Values, &binder{args: args})
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

func applyLimit(rows [][]any, lim *ast.LimitClause, args []any) ([][]any, error) {
	if lim == nil {
		return rows, nil
	}
	count := int64(len(rows))
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

func applyFlatLimit(rows []any, width int, lim *ast.LimitClause, args []any) ([]any, error) {
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
