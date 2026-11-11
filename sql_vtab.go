package cqlite

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	catalog "github.com/oarkflow/cqlite/internal/catalog"
	isql "github.com/oarkflow/cqlite/internal/sql"
	"github.com/oarkflow/cqlite/internal/sqlparser/ast"
	"github.com/oarkflow/cqlite/internal/sqlparser/lexer"
)

// usesFTSFeatures reports whether a select needs the virtual-table path: a
// MATCH constraint or a relevance column.
func usesFTSFeatures(st *ast.SelectStmt) bool {
	if _, ok := extractMatch(st.Where); ok {
		return true
	}
	for _, col := range st.Columns {
		if exprUsesRank(col.Expr) {
			return true
		}
	}
	return false
}

// exprUsesRank reports whether an expression reads the rank column or calls a
// relevance function.
func exprUsesRank(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		n := catalog.Normalize(x.Unquoted)
		return n == "rank" || n == "score"
	case *ast.QualifiedIdent:
		if len(x.Parts) > 0 {
			n := catalog.Normalize(x.Parts[len(x.Parts)-1].Unquoted)
			return n == "rank" || n == "score"
		}
	case *ast.FuncCall:
		switch funcCallName(x) {
		case "bm25", "highlight", "snippet":
			return true
		}
	}
	return false
}

// execCreateVirtualTableStmt handles CREATE VIRTUAL TABLE ... USING module.
func execCreateVirtualTableStmt(s *persistedState, st *ast.CreateVirtualTableStmt, rawSQL string) (Result, error) {
	if st == nil || st.Name == nil || st.Module == nil {
		return Result{}, fmt.Errorf("invalid CREATE VIRTUAL TABLE")
	}
	name := qname(st.Name)
	return execCreateVirtualTable(s, name, st.Module.Unquoted, string(st.Args), rawSQL, st.IfNotExists)
}

// vtabFromRef reports the virtual table a simple FROM item names, if any.
func vtabFromRef(s *persistedState, ref ast.TableRef) (*VirtualTable, *ast.SimpleTable, bool) {
	simple, ok := ref.(*ast.SimpleTable)
	if !ok {
		return nil, nil, false
	}
	vt, ok := hasVirtualTables(s, qname(simple.Name))
	if !ok {
		return nil, nil, false
	}
	return vt, simple, true
}

// vtabRowContext builds a row context for one virtual table row.
func vtabRowContext(vt *VirtualTable, simple *ast.SimpleTable, rowid int64, values []any) rowContext {
	tableName := qname(simple.Name)
	alias := tableName
	if simple.Alias != nil {
		alias = catalog.Normalize(simple.Alias.Unquoted)
	}
	vals := make(map[string]any, len(vt.Columns)*3+3)
	vals["rowid"] = rowid
	vals[tableName+".rowid"] = rowid
	vals[alias+".rowid"] = rowid
	for i, col := range vt.Columns {
		var v any
		if i < len(values) {
			v = values[i]
		}
		vals[col] = v
		vals[tableName+"."+col] = v
		vals[alias+"."+col] = v
	}
	return rowContext{Values: vals}
}

// vtabTableSchema renders a virtual table as a catalog table so ordinary row
// contexts and select-list projection can treat it uniformly.
func vtabTableSchema(vt *VirtualTable, name string) *catalog.Table {
	cols := make([]catalog.Column, 0, len(vt.Columns))
	for _, c := range vt.Columns {
		cols = append(cols, catalog.Column{Name: catalog.Normalize(c), Type: catalog.Text, DeclType: "TEXT"})
	}
	return &catalog.Table{Name: name, Columns: cols}
}

// isVTabTableRef reports whether a MATCH left-hand side names the virtual
// table itself rather than one of its columns.
func isVTabTableRef(name string, vt *VirtualTable, simple *ast.SimpleTable) bool {
	if name == "" {
		return true
	}
	tableName := catalog.Normalize(qname(simple.Name))
	if name == tableName {
		return true
	}
	if simple.Alias != nil && name == catalog.Normalize(simple.Alias.Unquoted) {
		return true
	}
	for _, c := range vt.Columns {
		if catalog.Normalize(c) == name {
			return false
		}
	}
	return true
}

// matchPredicate is a `x MATCH 'query'` constraint pulled out of a WHERE
// clause. It drives which virtual-table rows are visited.
type matchPredicate struct {
	// column names the left-hand side, empty for the table-wide form.
	column string
	query  string
}

// extractMatch finds a MATCH constraint in a WHERE expression.
func extractMatch(where ast.Expr) (matchPredicate, bool) {
	if where == nil {
		return matchPredicate{}, false
	}
	switch e := where.(type) {
	case *ast.BinaryExpr:
		if e.Op.String() == "MATCH" || isMatchOp(e.Op) {
			return matchPredicate{
				column: exprColumnName(e.Left),
				query:  exprLiteralString(e.Right),
			}, true
		}
		// A MATCH can be combined with other predicates by AND; the other side
		// is evaluated after the row set is built.
		if isAndOp(e.Op) {
			if mp, ok := extractMatch(e.Left); ok {
				return mp, true
			}
			return extractMatch(e.Right)
		}
	}
	return matchPredicate{}, false
}

// stripMatchPredicate returns the part of a WHERE clause that is not a MATCH
// constraint, so it can be evaluated as an ordinary filter once the virtual
// table has supplied its matching rows.
func stripMatchPredicate(where ast.Expr) ast.Expr {
	switch e := where.(type) {
	case nil:
		return nil
	case *ast.BinaryExpr:
		if isMatchOp(e.Op) {
			return nil
		}
		if isAndOp(e.Op) {
			l := stripMatchPredicate(e.Left)
			r := stripMatchPredicate(e.Right)
			switch {
			case l == nil:
				return r
			case r == nil:
				return l
			default:
				return &ast.BinaryExpr{Left: l, Right: r, Op: e.Op, TokPos: e.TokPos}
			}
		}
	}
	return where
}

// exprLiteralString renders a constant expression as text.
func exprLiteralString(e ast.Expr) string {
	if lit, ok := e.(*ast.Literal); ok {
		return unquoteSQL(string(lit.Raw))
	}
	return ""
}

// isMatchOp reports whether a token is the MATCH operator.
func isMatchOp(op lexer.TokenType) bool {
	return op == lexer.MATCH
}

func isAndOp(op lexer.TokenType) bool {
	return op == lexer.AND
}

// execVTabSelect runs a SELECT whose FROM names a virtual table. A MATCH
// constraint narrows the rows; rank and bm25() report relevance.
func execVTabSelect(s *persistedState, st *ast.SelectStmt, vt *VirtualTable, simple *ast.SimpleTable, args []any) (*Rows, error) {
	inst, err := openVTab(s, vt)
	if err != nil {
		return nil, err
	}
	match, hasMatch := extractMatch(st.Where)
	residual := stripMatchPredicate(st.Where)
	if hasMatch && match.column != "" && !isVTabTableRef(match.column, vt, simple) {
		// `col MATCH 'q'` is equivalent to `ft MATCH 'col:q'`. When the
		// left-hand side is the table itself the query is table-wide.
		match.query = match.column + ":" + match.query
	}
	var ids []int64
	var scores map[int64]float64
	if hasMatch {
		var idList []int64
		var scoreList []float64
		if fts, ok := inst.(*fts5Instance); ok && !selectNeedsFTSScores(st) {
			idList, scoreList, err = fts.QueryUnranked(match.query, nil)
		} else {
			idList, scoreList, err = inst.Query(match.query, nil)
		}
		if err != nil {
			return nil, err
		}
		ids = idList
		scores = make(map[int64]float64, len(idList))
		for i, id := range idList {
			scores[id] = scoreList[i]
		}
	} else {
		ids = inst.RowIDs()
		scores = map[int64]float64{}
	}

	table := vtabTableSchema(vt, qname(simple.Name))
	cols, exprs, err := selectList([]*catalog.Table{table}, st.Columns)
	if err != nil {
		return nil, err
	}
	contexts := make([]rowContext, 0, len(ids))
	type entry struct {
		ctx    rowContext
		rowid  int64
		score  float64
		values []any
	}
	entries := make([]entry, 0, len(ids))
	for _, id := range ids {
		values, ok := inst.Row(id)
		if !ok {
			continue
		}
		ctx := vtabRowContext(vt, simple, id, values)
		if residual != nil {
			b := binder{args: args}
			keep, err := matchWhereState(s, residual, ctx.Values, &b)
			if err != nil {
				return nil, err
			}
			if !keep {
				continue
			}
		}
		entries = append(entries, entry{ctx: ctx, rowid: id, score: scores[id], values: values})
		contexts = append(contexts, ctx)
	}

	// Project, substituting the rank and bm25() columns. Auxiliary functions
	// that need to know which terms matched get the query's term set.
	matched := map[string]bool{}
	if hasMatch {
		node, err := parseFTSQuery(match.query, vt.Columns)
		if err == nil && node != nil {
			for _, t := range collectFTSTerms(node) {
				matched[inst.Tokenizer().QueryToken(t.term)] = true
			}
		}
	}
	out := make([][]any, 0, len(entries))
	for _, e := range entries {
		row := make([]any, 0, len(exprs))
		for _, expr := range exprs {
			v := evalVTabExpr(s, expr, e.ctx.Values, e.score, vt, matched, inst, &binder{args: args})
			row = append(row, v)
		}
		out = append(out, row)
	}
	if len(st.OrderBy) > 0 && len(out) > 1 {
		sortProjectedRows(s, st, []*catalog.Table{table}, cols, out, contexts, nil, args)
	}
	if st.Distinct {
		out = distinctRowsKeepFirst(len(cols), out)
	}
	out, err = applyLimit(out, st.Limit, args)
	if err != nil {
		return nil, err
	}
	return isql.NewRows(cols, out), nil
}

func selectNeedsFTSScores(st *ast.SelectStmt) bool {
	if st == nil {
		return false
	}
	if exprUsesRank(st.Where) || exprUsesRank(st.Having) {
		return true
	}
	for _, col := range st.Columns {
		if exprUsesRank(col.Expr) {
			return true
		}
	}
	for _, expr := range st.GroupBy {
		if exprUsesRank(expr) {
			return true
		}
	}
	for _, item := range st.OrderBy {
		if exprUsesRank(item.Expr) {
			return true
		}
	}
	return false
}

// evalVTabExpr evaluates a select-list expression against a virtual table row,
// resolving rank and the bm25/highlight/snippet auxiliary functions.
func evalVTabExpr(s *persistedState, expr ast.Expr, row map[string]any, score float64, vt *VirtualTable, matched map[string]bool, inst vtabInstance, b *binder) any {
	switch e := expr.(type) {
	case *ast.Ident:
		name := catalog.Normalize(e.Unquoted)
		if name == "rank" || name == "score" {
			return score
		}
		return row[name]
	case *ast.QualifiedIdent:
		if len(e.Parts) > 0 {
			name := catalog.Normalize(e.Parts[len(e.Parts)-1].Unquoted)
			if name == "rank" || name == "score" {
				return score
			}
		}
	case *ast.FuncCall:
		name := funcCallName(e)
		switch name {
		case "bm25", "rank":
			return score
		case "highlight":
			return ftsAuxHighlight(e, row, vt, matched, inst)
		case "snippet":
			return ftsAuxSnippet(e, row, vt, matched, inst)
		}
	}
	v, err := evalExprState(s, expr, row, b)
	if err != nil {
		return nil
	}
	return v
}

// ftsAuxHighlight implements highlight(tbl, col, open, close): the column text
// with every matched term wrapped in the delimiters.
func ftsAuxHighlight(fn *ast.FuncCall, row map[string]any, vt *VirtualTable, matched map[string]bool, inst vtabInstance) any {
	if len(fn.Args) < 3 {
		return nil
	}
	colIdx := ftsColumnIndex(fn.Args[1], vt)
	if colIdx < 0 || colIdx >= len(vt.Columns) {
		return nil
	}
	text := stringifySQL(row[vt.Columns[colIdx]])
	open := exprLiteralString(fn.Args[2])
	closeTag := open
	if len(fn.Args) > 3 {
		closeTag = exprLiteralString(fn.Args[3])
	}
	return highlightText(text, matched, open, closeTag, inst)
}

// ftsAuxSnippet implements snippet(tbl, col, open, close, ellipsis, ntokens):
// the window of the column with the densest matches, with matched terms
// wrapped and elisions marking what was cut.
func ftsAuxSnippet(fn *ast.FuncCall, row map[string]any, vt *VirtualTable, matched map[string]bool, inst vtabInstance) any {
	if len(fn.Args) < 3 {
		return nil
	}
	colIdx := ftsColumnIndex(fn.Args[1], vt)
	if colIdx < 0 || colIdx >= len(vt.Columns) {
		return nil
	}
	text := stringifySQL(row[vt.Columns[colIdx]])
	open := exprLiteralString(fn.Args[2])
	closeTag := open
	if len(fn.Args) > 3 {
		closeTag = exprLiteralString(fn.Args[3])
	}
	ellipsis := "..."
	if len(fn.Args) > 4 {
		ellipsis = exprLiteralString(fn.Args[4])
	}
	ntokens := 15
	if len(fn.Args) > 5 {
		if n, err := strconv.Atoi(exprLiteralString(fn.Args[5])); err == nil && n > 0 {
			ntokens = n
		}
	}
	return snippetText(text, matched, open, closeTag, ellipsis, ntokens, inst)
}

// highlightText wraps every token whose term is matched.
func highlightText(text string, matched map[string]bool, open, closeTag string, inst vtabInstance) string {
	toks := tokenPositions(text, inst)
	if len(toks) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, t := range toks {
		if !matched[t.Term] {
			continue
		}
		b.WriteString(text[last:t.Start])
		b.WriteString(open)
		b.WriteString(text[t.Start:t.End])
		b.WriteString(closeTag)
		last = t.End
	}
	b.WriteString(text[last:])
	return b.String()
}

// snippetText returns the densest window of ntokens around the matches.
func snippetText(text string, matched map[string]bool, open, closeTag, ellipsis string, ntokens int, inst vtabInstance) string {
	toks := tokenPositions(text, inst)
	if len(toks) == 0 {
		return text
	}
	bestStart, bestScore := 0, -1
	for i := 0; i < len(toks); i++ {
		score := 0
		for j := i; j < len(toks) && j < i+ntokens; j++ {
			if matched[toks[j].Term] {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestStart = i
		}
	}
	if bestScore <= 0 {
		bestStart = 0
	}
	end := bestStart + ntokens
	if end > len(toks) {
		end = len(toks)
	}
	from := toks[bestStart].Start
	to := toks[end-1].End
	var b strings.Builder
	if from > 0 {
		b.WriteString(ellipsis)
	}
	last := from
	for _, t := range toks[bestStart:end] {
		if !matched[t.Term] {
			continue
		}
		b.WriteString(text[last:t.Start])
		b.WriteString(open)
		b.WriteString(text[t.Start:t.End])
		b.WriteString(closeTag)
		last = t.End
	}
	b.WriteString(text[last:to])
	if to < len(text) {
		b.WriteString(ellipsis)
	}
	return b.String()
}

// tokenPositions tokenizes text and records the byte span of each token.
func tokenPositions(text string, inst vtabInstance) []Token {
	if inst != nil {
		if f, ok := inst.(*fts5Instance); ok {
			return f.tok.Tokenize(text)
		}
	}
	return unicode61Tokenizer{}.Tokenize(text)
}

// ftsColumnIndex resolves a highlight/snippet column argument.
func ftsColumnIndex(e ast.Expr, vt *VirtualTable) int {
	if lit, ok := e.(*ast.Literal); ok {
		raw := strings.TrimSpace(string(lit.Raw))
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return int(n)
		}
	}
	name := strings.ToLower(exprColumnName(e))
	for i, c := range vt.Columns {
		if strings.ToLower(c) == name {
			return i
		}
	}
	return -1
}

// execVTabInsert applies an INSERT to a virtual table.
func execVTabInsert(s *persistedState, st *ast.InsertStmt, vt *VirtualTable) (Result, error) {
	inst, err := openVTab(s, vt)
	if err != nil {
		return Result{}, err
	}
	if fts, ok := inst.(*fts5Instance); ok && len(st.Columns) == 1 &&
		strings.EqualFold(st.Columns[0].Unquoted, vt.Name) {
		if len(st.Values) != 1 || len(st.Values[0]) != 1 {
			return Result{}, errors.New("FTS5 control command requires one value")
		}
		value, err := evalExpr(st.Values[0][0], nil, &binder{})
		if err != nil {
			return Result{}, err
		}
		command := strings.ToLower(strings.TrimSpace(stringifySQL(value)))
		if command != "optimize" {
			return Result{}, fmt.Errorf("no such FTS5 special command: %s", command)
		}
		if err := fts.optimize(); err != nil {
			return Result{}, err
		}
		return Result{}, nil
	}
	cols := make([]int, 0, len(st.Columns))
	if len(st.Columns) == 0 {
		for i := range vt.Columns {
			cols = append(cols, i)
		}
	} else {
		for _, id := range st.Columns {
			idx := -1
			for i, c := range vt.Columns {
				if strings.EqualFold(c, id.Unquoted) {
					idx = i
					break
				}
			}
			if idx < 0 {
				return Result{}, fmt.Errorf("unknown column: %s", id.Unquoted)
			}
			cols = append(cols, idx)
		}
	}
	emit := func(values []any) (int64, error) {
		full := make([]any, len(vt.Columns))
		for i, ci := range cols {
			if i < len(values) {
				full[ci] = values[i]
			}
		}
		return inst.Insert(0, full)
	}
	var last int64
	var affected int64
	if st.Select != nil {
		rows, err := execSelect(s, st.Select, nil)
		if err != nil {
			return Result{}, err
		}
		for {
			vals, ok := rows.Next()
			if !ok {
				break
			}
			id, err := emit(vals)
			if err != nil {
				return Result{}, err
			}
			last = id
			affected++
		}
		return Result{RowsAffected: affected, LastInsertID: last}, nil
	}
	for _, valueExprs := range st.Values {
		values := make([]any, 0, len(valueExprs))
		b := binder{args: nil}
		for _, e := range valueExprs {
			v, err := evalExpr(e, nil, &b)
			if err != nil {
				return Result{}, err
			}
			values = append(values, v)
		}
		id, err := emit(values)
		if err != nil {
			return Result{}, err
		}
		last = id
		affected++
	}
	return Result{RowsAffected: affected, LastInsertID: last}, nil
}

// execVTabDelete removes virtual table rows selected by a WHERE clause.
func execVTabDelete(s *persistedState, st *ast.DeleteStmt, vt *VirtualTable) (Result, error) {
	inst, err := openVTab(s, vt)
	if err != nil {
		return Result{}, err
	}
	ids := inst.RowIDs()
	var affected int64
	for _, id := range ids {
		values, ok := inst.Row(id)
		if !ok {
			continue
		}
		ctx := vtabRowContext(vt, &ast.SimpleTable{}, id, values)
		if residual := stripMatchPredicate(st.Where); residual != nil {
			b := binder{}
			keep, err := matchWhereState(s, residual, ctx.Values, &b)
			if err != nil {
				return Result{}, err
			}
			if !keep {
				continue
			}
		}
		if err := inst.Delete(id); err != nil {
			return Result{}, err
		}
		affected++
	}
	return Result{RowsAffected: affected}, nil
}

// dmlStatementTouchesVirtualTable reports whether a DML statement targets a
// virtual table, which must be dispatched to its module.
func dmlStatementTouchesVirtualTable(s *persistedState, stmt ast.Statement) bool {
	switch st := stmt.(type) {
	case *ast.InsertStmt:
		_, ok := hasVirtualTables(s, qname(st.Table))
		return ok
	case *ast.UpdateStmt:
		name, err := tableNameFromRefs(st.Tables)
		if err != nil {
			return false
		}
		_, ok := hasVirtualTables(s, name)
		return ok
	case *ast.DeleteStmt:
		name, err := tableNameFromRefs(st.From)
		if err != nil {
			return false
		}
		_, ok := hasVirtualTables(s, name)
		return ok
	}
	return false
}

// buildVirtualRows expands a virtual table into row contexts so joins, WHERE
// and projection can treat it like an ordinary table.
func buildVirtualRows(s *persistedState, vt *VirtualTable, r *ast.SimpleTable, keep func(rowVersion) bool) ([]*catalog.Table, []rowContext, error) {
	if keep != nil {
		return nil, nil, errors.New("virtual table rows cannot be filtered by row predicate")
	}
	inst, err := openVTab(s, vt)
	if err != nil {
		return nil, nil, err
	}
	table := vtabTableSchema(vt, qname(r.Name))
	contexts := make([]rowContext, 0, 16)
	for _, id := range inst.RowIDs() {
		values, ok := inst.Row(id)
		if !ok {
			continue
		}
		contexts = append(contexts, vtabRowContext(vt, r, id, values))
	}
	return []*catalog.Table{table}, contexts, nil
}

// execVTabUpdate applies an UPDATE to a virtual table by rewriting the
// affected rows through the module.
func execVTabUpdate(s *persistedState, st *ast.UpdateStmt, vt *VirtualTable) (Result, error) {
	inst, err := openVTab(s, vt)
	if err != nil {
		return Result{}, err
	}
	residual := stripMatchPredicate(st.Where)
	ids := inst.RowIDs()
	var affected int64
	for _, id := range ids {
		values, ok := inst.Row(id)
		if !ok {
			continue
		}
		ctx := vtabRowContext(vt, &ast.SimpleTable{}, id, values)
		if residual != nil {
			b := binder{}
			keep, err := matchWhereState(s, residual, ctx.Values, &b)
			if err != nil {
				return Result{}, err
			}
			if !keep {
				continue
			}
		}
		next := make([]any, len(vt.Columns))
		copy(next, values)
		setBind := binder{}
		for _, asn := range st.Set {
			idx := ftsColumnIndex(&ast.Ident{Unquoted: asn.Column.Unquoted}, vt)
			if idx < 0 || idx >= len(vt.Columns) {
				return Result{}, fmt.Errorf("unknown column: %s", asn.Column.Unquoted)
			}
			v, err := evalExpr(asn.Value, ctx.Values, &setBind)
			if err != nil {
				return Result{}, err
			}
			next[idx] = v
		}
		if err := inst.Update(id, next); err != nil {
			return Result{}, err
		}
		affected++
	}
	return Result{RowsAffected: affected}, nil
}
