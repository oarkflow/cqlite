package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sujit/sqlite/internal/sqlparser/ast"
	catalog "github.com/sujit/sqlite/internal/catalog"
	isql "github.com/sujit/sqlite/internal/sql"
)

// cteMaterial is a fully materialized common table expression.
type cteMaterial struct {
	cols []string
	rows [][]any
}

// recursiveCTELimit bounds WITH RECURSIVE iteration so a non-terminating
// definition cannot wedge the process. SQLite itself has no hard limit, but a
// runaway recursion here would never return control to the caller.
const recursiveCTELimit = 1_000_000

// execSelectExt handles WITH clauses and compound (set-operation) selects.
// It returns ok=false when the statement is a plain SELECT the caller should
// execute normally.
func execSelectExt(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, bool, error) {
	if st.With != nil {
		restore, err := materializeCTEs(s, st.With, args)
		if err != nil {
			return nil, true, err
		}
		defer restore()
		body := shallowSelect(st)
		body.With = nil
		if body.SetOp != nil {
			rows, err := execCompoundSelect(s, body, args)
			return rows, true, err
		}
		rows, err := execSelect(s, body, args)
		return rows, true, err
	}
	if st.SetOp != nil {
		rows, err := execCompoundSelect(s, st, args)
		return rows, true, err
	}
	return nil, false, nil
}

// expandRows materializes a *Rows into a row slice regardless of whether it is
// stored flat (fast paths) or row-major (generic path).
func expandRows(r *Rows) [][]any {
	if r == nil {
		return nil
	}
	if r.Width > 0 {
		out := make([][]any, 0, len(r.Flat)/maxInt(r.Width, 1))
		for i := 0; i+r.Width <= len(r.Flat); i += r.Width {
			row := make([]any, r.Width)
			copy(row, r.Flat[i:i+r.Width])
			out = append(out, row)
		}
		return out
	}
	return r.Values
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// shallowSelect copies a SelectStmt header so arms of a compound select can be
// evaluated without the parser's SetOp/OrderBy/Limit fields. The parser's AST
// is arena-owned and must not be mutated.
func shallowSelect(st *ast.SelectStmt) *ast.SelectStmt {
	cp := *st
	return &cp
}

// compoundArm is one operand of a compound SELECT. op is the set operator that
// joins this arm to the preceding arms; the first arm has no operator.
type compoundArm struct {
	op   ast.SetOp
	all  bool
	stmt *ast.SelectStmt
}

// flattenCompound breaks a compound SELECT chain into arms in source order.
// The parser nests SetOp right-leaning (a UNION (b EXCEPT c)), but SQL set
// operators are left-associative, so arms are collected in order and folded
// left below. ORDER BY / LIMIT on the rightmost arm govern the whole compound
// result and are lifted out. arms[0].op is unused.
func flattenCompound(st *ast.SelectStmt) ([]compoundArm, []ast.OrderByItem, *ast.LimitClause, error) {
	var arms []compoundArm
	var trailerBy []ast.OrderByItem
	var trailerLimit *ast.LimitClause
	var op ast.SetOp
	var all bool
	cur := st
	for {
		arms = append(arms, compoundArm{op: op, all: all, stmt: cur})
		if cur.SetOp == nil {
			trailerBy = cur.OrderBy
			trailerLimit = cur.Limit
			break
		}
		op, all = cur.SetOp.Op, cur.SetOp.All
		next := cur.SetOp.Right
		if next == nil {
			return nil, nil, nil, errors.New("compound SELECT is missing its right operand")
		}
		cur = next
	}
	return arms, trailerBy, trailerLimit, nil
}

// execCompoundSelect evaluates a compound SELECT (UNION, UNION ALL, INTERSECT,
// EXCEPT) left-associatively, matching SQLite semantics.
func execCompoundSelect(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, error) {
	arms, trailerBy, trailerLimit, err := flattenCompound(st)
	if err != nil {
		return nil, err
	}
	if len(arms) == 0 {
		return nil, errors.New("empty compound SELECT")
	}
	left, err := execCompoundArm(s, arms[0].stmt, args)
	if err != nil {
		return nil, err
	}
	cols := left.Columns
	out := expandRows(left)
	for _, arm := range arms[1:] {
		right, err := execCompoundArm(s, arm.stmt, args)
		if err != nil {
			return nil, err
		}
		out, err = combineSetOp(arm.op, arm.all, cols, out, expandRows(right))
		if err != nil {
			return nil, err
		}
	}
	if len(trailerBy) > 0 {
		sortOutputRows(out, cols, trailerBy, args)
	}
	if trailerLimit != nil {
		out, err = applyLimit(out, trailerLimit, args)
		if err != nil {
			return nil, err
		}
	}
	return isql.NewRows(cols, out), nil
}

// execCompoundArm evaluates one arm of a compound SELECT, including nested
// WITH and nested compound arms.
func execCompoundArm(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, error) {
	arm := shallowSelect(st)
	arm.SetOp = nil
	arm.OrderBy = nil
	arm.Limit = nil
	return execSelectArm(s, arm, args)
}

// execSelectArm evaluates a select that may itself carry a WITH clause.
func execSelectArm(s *persistedState, st *ast.SelectStmt, args []any) (*Rows, error) {
	rows, ext, err := execSelectExt(s, st, args)
	if ext || err != nil {
		return rows, err
	}
	return execSelect(s, st, args)
}

// combineSetOp merges two result sets under a set operator.
func combineSetOp(op ast.SetOp, all bool, cols []string, left, right [][]any) ([][]any, error) {
	width := len(cols)
	switch op {
	case ast.Union:
		if all {
			out := make([][]any, 0, len(left)+len(right))
			out = append(out, left...)
			out = append(out, right...)
			return out, nil
		}
		combined := make([][]any, 0, len(left)+len(right))
		combined = append(combined, left...)
		combined = append(combined, right...)
		return compoundDistinctRows(width, combined), nil
	case ast.Intersect:
		return intersectRows(width, left, right), nil
	case ast.Except:
		return exceptRows(width, left, right), nil
	default:
		return nil, fmt.Errorf("unsupported set operation: %d", op)
	}
}

// rowKey builds a comparable identity for a result row so set operations can
// deduplicate. NULLs compare equal to each other for DISTINCT purposes, and
// INTEGER 1 equals REAL 1.0, matching SQLite's compound-select semantics.
func rowKey(width int, row []any) string {
	var b strings.Builder
	for i := 0; i < width && i < len(row); i++ {
		v := row[i]
		switch x := v.(type) {
		case nil:
			b.WriteString("n;")
		case bool:
			if x {
				b.WriteString("i;1;")
			} else {
				b.WriteString("i;0;")
			}
		case int:
			b.WriteString("i;")
			b.WriteString(strconv.FormatInt(int64(x), 10))
			b.WriteString(";")
		case int64:
			b.WriteString("i;")
			b.WriteString(strconv.FormatInt(x, 10))
			b.WriteString(";")
		case float64:
			// A float that is exactly an integer identifies as that integer so
			// UNION of 1 and 1.0 yields one row, as SQLite does.
			if x == float64(int64(x)) {
				b.WriteString("i;")
				b.WriteString(strconv.FormatInt(int64(x), 10))
			} else {
				b.WriteString("f;")
				b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
			}
			b.WriteString(";")
		case string:
			b.WriteString("s;")
			b.WriteString(x)
			b.WriteString(";")
		case []byte:
			b.WriteString("b;")
			b.Write(x)
			b.WriteString(";")
		default:
			b.WriteString("x;")
			b.WriteString(fmt.Sprintf("%v", x))
			b.WriteString(";")
		}
	}
	return b.String()
}

// compareSQLValues orders two values the way SQLite's compound-select b-tree
// does: NULLs first, then numbers, then text, then blobs. Numbers compare
// numerically regardless of whether they are stored as INTEGER or REAL.
func compareSQLValues(a, b any) int {
	ca, cb := sqlValueClass(a), sqlValueClass(b)
	if ca != cb {
		if ca < cb {
			return -1
		}
		return 1
	}
	switch ca {
	case 0:
		return 0
	case 1:
		af, _ := asFloat(a)
		bf, _ := asFloat(b)
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	case 2:
		return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
	case 3:
		return bytes.Compare(toBytes(a), toBytes(b))
	}
	return 0
}

func sqlValueClass(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case bool, int, int64, float64, float32:
		return 1
	case string:
		return 2
	case []byte:
		return 3
	case json.Number:
		return 1
	default:
		_ = x
		return 2
	}
}

func toBytes(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	}
	return []byte(fmt.Sprint(v))
}

// compareSQLRows orders two result rows column by column.
func compareSQLRows(a, b []any) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := compareSQLValues(a[i], b[i]); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

// sortSQLRows sorts rows in SQLite compound-select order.
func sortSQLRows(rows [][]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		return compareSQLRows(rows[i], rows[j]) < 0
	})
}

// distinctRowsKeepFirst implements SELECT DISTINCT: the first occurrence wins
// and rows keep their input order. SQLite resolves SELECT DISTINCT with a
// hash/temporary structure rather than a sorter, so the output is not sorted.
func distinctRowsKeepFirst(width int, rows [][]any) [][]any {
	seen := make(map[string]struct{}, len(rows))
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		k := rowKey(width, row)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, row)
	}
	return out
}

// compoundDistinctRows implements the distinct form of UNION: rows are
// deduplicated with the *last* arm's value winning, and the result is sorted
// the way SQLite's compound-select temporary b-tree emits it.
func compoundDistinctRows(width int, rows [][]any) [][]any {
	order := make([]string, 0, len(rows))
	byKey := make(map[string][]any, len(rows))
	for _, row := range rows {
		k := rowKey(width, row)
		if _, dup := byKey[k]; !dup {
			order = append(order, k)
		}
		byKey[k] = row
	}
	out := make([][]any, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	sortSQLRows(out)
	return out
}

// distinctRows is retained for callers that want first-wins, input-order
// deduplication.
func distinctRows(width int, sets ...[][]any) [][]any {
	var all [][]any
	for _, set := range sets {
		all = append(all, set...)
	}
	return distinctRowsKeepFirst(width, all)
}

// intersectRows returns rows present in both sets. INTERSECT is always
// distinct in SQLite, including INTERSECT ALL.
func intersectRows(width int, left, right [][]any) [][]any {
	present := make(map[string]struct{}, len(right))
	for _, row := range right {
		present[rowKey(width, row)] = struct{}{}
	}
	seen := make(map[string]struct{})
	var out [][]any
	for _, row := range left {
		k := rowKey(width, row)
		if _, ok := present[k]; !ok {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, row)
	}
	sortSQLRows(out)
	return out
}

// exceptRows returns rows present in left but not in right. EXCEPT is always
// distinct in SQLite, including EXCEPT ALL.
func exceptRows(width int, left, right [][]any) [][]any {
	dropped := make(map[string]struct{}, len(right))
	for _, row := range right {
		dropped[rowKey(width, row)] = struct{}{}
	}
	seen := make(map[string]struct{})
	var out [][]any
	for _, row := range left {
		k := rowKey(width, row)
		if _, ok := dropped[k]; ok {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, row)
	}
	sortSQLRows(out)
	return out
}

// sortOutputRows orders already-projected output rows by ORDER BY terms that
// may reference output column names, ordinals, or expressions.
func sortOutputRows(rows [][]any, cols []string, order []ast.OrderByItem, args []any) {
	if len(rows) < 2 {
		return
	}
	rank := map[string]int{}
	for i, c := range cols {
		rank[strings.ToLower(c)] = i + 1
	}
	evalTerm := func(item ast.OrderByItem, row []any) any {
		if lit, ok := item.Expr.(*ast.Literal); ok {
			raw := strings.TrimSpace(string(lit.Raw))
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 1 && int(n) <= len(cols) {
				return row[int(n)-1]
			}
		}
		name := ""
		switch e := item.Expr.(type) {
		case *ast.Ident:
			name = strings.ToLower(e.Unquoted)
		case *ast.QualifiedIdent:
			if len(e.Parts) > 0 {
				name = strings.ToLower(e.Parts[len(e.Parts)-1].Unquoted)
			}
		}
		if name != "" {
			if idx, ok := rank[name]; ok && idx <= len(row) {
				return row[idx-1]
			}
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			if i < len(row) {
				m[strings.ToLower(c)] = row[i]
			}
		}
		v, _ := evalExpr(item.Expr, m, &binder{args: args})
		return v
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, item := range order {
			lv := evalTerm(item, rows[i])
			rv := evalTerm(item, rows[j])
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

// materializeCTEs evaluates every CTE in a WITH clause and installs it in the
// query scope. The returned function restores the previous scope. Later CTEs
// in the same clause can reference earlier ones, and the outer statement sees
// all of them.
func materializeCTEs(s *persistedState, with *ast.WithClause, args []any) (func(), error) {
	prev := s.ctes
	restore := func() { s.ctes = prev }
	if with == nil || len(with.CTEs) == 0 {
		return restore, nil
	}
	scope := make(map[string]*cteMaterial, len(with.CTEs)+len(prev))
	for k, v := range prev {
		scope[k] = v
	}
	for i := range with.CTEs {
		cte := &with.CTEs[i]
		if cte.Name == nil {
			return restore, errors.New("CTE is missing a name")
		}
		name := catalog.Normalize(cte.Name.Unquoted)
		mat, err := materializeCTE(s, scope, with.Recursive, cte, args)
		if err != nil {
			return restore, err
		}
		if len(cte.Columns) > 0 {
			cols := make([]string, len(cte.Columns))
			for i, c := range cte.Columns {
				cols[i] = catalog.Normalize(c.Unquoted)
			}
			mat.cols = cols
		}
		scope[name] = mat
	}
	s.ctes = scope
	return restore, nil
}

// materializeCTE evaluates a single CTE definition into a row set.
func materializeCTE(s *persistedState, scope map[string]*cteMaterial, recursive bool, cte *ast.CTE, args []any) (*cteMaterial, error) {
	if cte.Subq == nil {
		return nil, errors.New("CTE body is empty")
	}
	name := ""
	if cte.Name != nil {
		name = catalog.Normalize(cte.Name.Unquoted)
	}
	selfRef := recursive && cteReferences(cte.Subq, name)
	if !selfRef {
		saved := s.ctes
		s.ctes = scope
		rows, err := execSelectArm(s, cte.Subq, args)
		s.ctes = saved
		if err != nil {
			return nil, err
		}
		return &cteMaterial{cols: rows.Columns, rows: expandRows(rows)}, nil
	}
	return materializeRecursiveCTE(s, scope, name, cte, args)
}

// materializeRecursiveCTE evaluates `anchor UNION [ALL] recursive` the way
// SQLite does: the recursive reference resolves to the *working table*, the
// rows produced by the immediately preceding iteration, not the whole
// accumulated result. The result table accumulates every working row, which is
// what keeps `UNION ALL` recursion linear rather than quadratic.
func materializeRecursiveCTE(s *persistedState, scope map[string]*cteMaterial, name string, cte *ast.CTE, args []any) (*cteMaterial, error) {
	sub := cte.Subq
	if sub.SetOp == nil {
		return nil, errors.New("recursive CTE must be an anchor UNION [ALL] recursive-select")
	}
	if sub.SetOp.Op != ast.Union {
		return nil, errors.New("recursive CTE must use UNION or UNION ALL between anchor and recursive part")
	}
	all := sub.SetOp.All
	anchor := shallowSelect(sub)
	anchor.SetOp = nil
	anchor.OrderBy = nil
	anchor.Limit = nil
	recursive := sub.SetOp.Right
	if recursive == nil {
		return nil, errors.New("recursive CTE is missing its recursive part")
	}

	saved := s.ctes
	s.ctes = scope
	defer func() { s.ctes = saved }()

	anchorRows, err := execSelectArm(s, anchor, args)
	if err != nil {
		return nil, fmt.Errorf("recursive CTE anchor: %w", err)
	}
	cols := anchorRows.Columns
	if len(cte.Columns) > 0 {
		cols = make([]string, len(cte.Columns))
		for i, c := range cte.Columns {
			cols[i] = catalog.Normalize(c.Unquoted)
		}
	}
	result := &cteMaterial{cols: cols, rows: expandRows(anchorRows)}
	if len(result.rows) == 0 {
		return result, nil
	}

	seen := make(map[string]struct{}, len(result.rows))
	if !all {
		for _, r := range result.rows {
			seen[rowKey(len(cols), r)] = struct{}{}
		}
	}
	// frontier is the working table: only the rows produced by the previous
	// step. It is what the recursive reference resolves to.
	frontier := &cteMaterial{cols: cols, rows: result.rows}
	scope[name] = frontier
	for {
		if len(result.rows) > recursiveCTELimit {
			return nil, fmt.Errorf("recursive CTE %q exceeded %d rows", name, recursiveCTELimit)
		}
		step, err := execSelectArm(s, recursive, args)
		if err != nil {
			return nil, fmt.Errorf("recursive CTE step: %w", err)
		}
		stepRows := expandRows(step)
		if len(stepRows) == 0 {
			break
		}
		next := make([][]any, 0, len(stepRows))
		for _, r := range stepRows {
			if !all {
				k := rowKey(len(cols), r)
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
			}
			result.rows = append(result.rows, r)
			next = append(next, r)
		}
		if len(next) == 0 {
			break
		}
		frontier = &cteMaterial{cols: cols, rows: next}
		scope[name] = frontier
	}
	return result, nil
}

// cteReferences reports whether a statement reads from the named CTE,
// including through a nested compound arm.
func cteReferences(st *ast.SelectStmt, name string) bool {
	if st == nil {
		return false
	}
	target := strings.ToLower(name)
	found := false
	walkTableRefs(st, func(ref ast.TableRef) {
		simple, ok := ref.(*ast.SimpleTable)
		if !ok {
			return
		}
		if strings.ToLower(qname(simple.Name)) == target {
			found = true
		}
	})
	if found {
		return true
	}
	if st.SetOp != nil && st.SetOp.Right != nil {
		return cteReferences(st.SetOp.Right, name)
	}
	return false
}

// walkTableRefs visits every FROM item of a select, including nested joins.
func walkTableRefs(st *ast.SelectStmt, fn func(ast.TableRef)) {
	if st == nil {
		return
	}
	for _, ref := range st.From {
		walkTableRef(ref, fn)
	}
}

func walkTableRef(ref ast.TableRef, fn func(ast.TableRef)) {
	if ref == nil {
		return
	}
	fn(ref)
	if j, ok := ref.(*ast.JoinTable); ok {
		walkTableRef(j.Left, fn)
		walkTableRef(j.Right, fn)
	}
}

// buildCTERows expands a materialized CTE into row contexts, matching the
// shape produced by buildSimpleRows so joins and WHERE work unchanged.
func buildCTERows(r *ast.SimpleTable, mat *cteMaterial) ([]*catalog.Table, []rowContext, error) {
	cols := make([]catalog.Column, len(mat.cols))
	for i, name := range mat.cols {
		cols[i] = catalog.Column{Name: name, Type: catalog.Any, DeclType: "ANY"}
	}
	tableName := qname(r.Name)
	if tableName == "" {
		tableName = "cte"
	}
	alias := tableName
	if r.Alias != nil {
		alias = catalog.Normalize(r.Alias.Unquoted)
	}
	t := &catalog.Table{Name: tableName, Columns: cols}
	contexts := make([]rowContext, 0, len(mat.rows))
	for _, row := range mat.rows {
		vals := make(map[string]any, len(cols)*3)
		for i, col := range cols {
			var v any
			if i < len(row) {
				v = row[i]
			}
			vals[col.Name] = v
			vals[tableName+"."+col.Name] = v
			vals[alias+"."+col.Name] = v
		}
		contexts = append(contexts, rowContext{Values: vals})
	}
	return []*catalog.Table{t}, contexts, nil
}
