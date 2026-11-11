package db

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/sujit/sqlite/internal/catalog"
	"github.com/sujit/sqlite/internal/sqlparser/ast"
	"github.com/sujit/sqlite/internal/sqlparser/lexer"
)

// windowCall is one window function invocation found in a select list, bound to
// its resolved window specification.
type windowCall struct {
	fn      *ast.FuncCall
	name    string
	spec    *ast.WindowSpec
	argIdxs []int
}

// isWindowCall reports whether a function call is a window function.
func isWindowCall(fn *ast.FuncCall) bool {
	return fn != nil && fn.Over != nil
}

// hasWindowSelect reports whether the select list contains a window function
// at any depth.
func hasWindowSelect(st *ast.SelectStmt) bool {
	if st == nil {
		return false
	}
	for _, col := range st.Columns {
		if col.Expr != nil && exprHasWindow(col.Expr) {
			return true
		}
	}
	return false
}

// exprHasWindow reports whether an expression contains a window function call.
// Subqueries are not descended into: their window functions belong to the
// subquery's own select.
func exprHasWindow(expr ast.Expr) bool {
	found := false
	walkExprForWindow(expr, func(*ast.FuncCall) { found = true })
	return found
}

// walkExprForWindow visits every window function call in an expression tree.
func walkExprForWindow(expr ast.Expr, fn func(*ast.FuncCall)) {
	switch e := expr.(type) {
	case nil:
		return
	case *ast.FuncCall:
		if isWindowCall(e) {
			fn(e)
			return
		}
		for _, a := range e.Args {
			walkExprForWindow(a, fn)
		}
	case *ast.BinaryExpr:
		walkExprForWindow(e.Left, fn)
		walkExprForWindow(e.Right, fn)
	case *ast.UnaryExpr:
		walkExprForWindow(e.Expr, fn)
	case *ast.CastExpr:
		walkExprForWindow(e.Expr, fn)
	case *ast.CaseExpr:
		if e.Else != nil {
			walkExprForWindow(e.Else, fn)
		}
		if e.Operand != nil {
			walkExprForWindow(e.Operand, fn)
		}
		for _, w := range e.Whens {
			walkExprForWindow(w.Cond, fn)
			walkExprForWindow(w.Result, fn)
		}
	case *ast.BetweenExpr:
		walkExprForWindow(e.Expr, fn)
		if e.Lo != nil {
			walkExprForWindow(e.Lo, fn)
		}
		if e.Hi != nil {
			walkExprForWindow(e.Hi, fn)
		}
	case *ast.IsNullExpr:
		walkExprForWindow(e.Expr, fn)
	case *ast.LikeExpr:
		if e.Expr != nil {
			walkExprForWindow(e.Expr, fn)
		}
		if e.Pattern != nil {
			walkExprForWindow(e.Pattern, fn)
		}
		if e.Escape != nil {
			walkExprForWindow(e.Escape, fn)
		}
	case *ast.InExpr:
		walkExprForWindow(e.Expr, fn)
		for _, item := range e.List {
			walkExprForWindow(item, fn)
		}
		// e.Subq is a subquery: its window calls are out of scope here.
	case *ast.IntervalExpr:
		if e.Expr != nil {
			walkExprForWindow(e.Expr, fn)
		}
	}
}

// resolveWindowSpec merges an inline OVER (...) specification with a named
// WINDOW clause entry. SQLite resolves `OVER name` to the named definition and
// allows the definition itself to name a base window.
func resolveWindowSpec(st *ast.SelectStmt, spec *ast.WindowSpec, depth int) (*ast.WindowSpec, error) {
	if spec == nil {
		return nil, nil
	}
	if depth > 16 {
		return nil, fmt.Errorf("window definition is too deeply nested")
	}
	named := map[string]*ast.WindowSpec{}
	for i := range st.Windows {
		w := &st.Windows[i]
		if w.Name != nil {
			named[strings.ToLower(w.Name.Unquoted)] = w.Spec
		}
	}
	cur := spec
	name := windowSpecName(cur)
	if name != "" {
		key := strings.ToLower(name)
		base, ok := named[key]
		if !ok {
			return nil, fmt.Errorf("no such window: %s", name)
		}
		merged := *cur
		merged.Name = nil
		rawNameOnly := len(cur.PartitionBy) == 0 && len(cur.OrderBy) == 0 && strings.TrimSpace(string(cur.Raw)) == name
		if len(merged.Raw) == 0 || rawNameOnly {
			merged.Raw = base.Raw
		} else if len(base.Raw) > 0 {
			return nil, fmt.Errorf("window frame cannot override a base window frame")
		}
		if len(merged.PartitionBy) == 0 {
			merged.PartitionBy = base.PartitionBy
		}
		if len(merged.OrderBy) == 0 {
			merged.OrderBy = base.OrderBy
		}
		if windowSpecName(base) != "" {
			return resolveWindowSpec(st, &merged, depth+1)
		}
		return &merged, nil
	}
	return cur, nil
}

// windowSpecName extracts the referenced window name from a spec. The parser
// records `OVER w` as Name, but `OVER (w)` leaves the identifier untouched in
// Raw with no PartitionBy/OrderBy clauses.
func windowSpecName(spec *ast.WindowSpec) string {
	if spec == nil {
		return ""
	}
	if spec.Name != nil {
		return spec.Name.Unquoted
	}
	if len(spec.PartitionBy) > 0 || len(spec.OrderBy) > 0 {
		return ""
	}
	raw := strings.TrimSpace(string(spec.Raw))
	if raw == "" || strings.ContainsAny(raw, " \t\r\n()") {
		return ""
	}
	return raw
}

// collectNestedAggregates finds every bare aggregate call inside the arguments
// of the given window calls. Expressions such as sum(sum(n)) OVER () need the
// inner aggregate resolved against each GROUP BY group before the outer window
// aggregate can run.
func collectNestedAggregates(calls []windowCall) []ast.Expr {
	var out []ast.Expr
	seen := map[ast.Expr]struct{}{}
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case nil:
			return
		case *ast.FuncCall:
			if isWindowCall(x) {
				return
			}
			if isAggregateCall(x) {
				if _, ok := seen[ast.Expr(x)]; !ok {
					seen[ast.Expr(x)] = struct{}{}
					out = append(out, ast.Expr(x))
				}
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
			if x.Operand != nil {
				walk(x.Operand)
			}
			if x.Else != nil {
				walk(x.Else)
			}
			for _, w := range x.Whens {
				walk(w.Cond)
				walk(w.Result)
			}
		case *ast.BetweenExpr:
			walk(x.Expr)
			walk(x.Lo)
			walk(x.Hi)
		case *ast.IsNullExpr:
			walk(x.Expr)
		case *ast.LikeExpr:
			walk(x.Expr)
			walk(x.Pattern)
			walk(x.Escape)
		case *ast.InExpr:
			walk(x.Expr)
		}
	}
	for _, c := range calls {
		for _, a := range c.fn.Args {
			walk(a)
		}
		if c.spec != nil {
			for _, p := range c.spec.PartitionBy {
				walk(p)
			}
			for _, o := range c.spec.OrderBy {
				walk(o.Expr)
			}
		}
	}
	return out
}

// evalWindowArg evaluates a window-function argument, substituting nested
// aggregate results already computed for the row.
func evalWindowArg(s *persistedState, expr ast.Expr, row map[string]any, aux map[ast.Expr]any, args []any) (any, error) {
	if aux != nil {
		if v, ok := aux[expr]; ok {
			return v, nil
		}
	}
	switch e := expr.(type) {
	case *ast.BinaryExpr:
		l, err := evalWindowArg(s, e.Left, row, aux, args)
		if err != nil {
			return nil, err
		}
		r, err := evalWindowArg(s, e.Right, row, aux, args)
		if err != nil {
			return nil, err
		}
		return evalBinary(l, r, e.Op)
	case *ast.UnaryExpr:
		v, err := evalWindowArg(s, e.Expr, row, aux, args)
		if err != nil {
			return nil, err
		}
		if e.Op == lexer.MINUS {
			return negateNumeric(v)
		}
		if e.Op == lexer.NOT || strings.EqualFold(e.Op.String(), "UNKNOWN") {
			return !truthy(v), nil
		}
		return nil, fmt.Errorf("unsupported unary operator: %s", e.Op)
	case *ast.CastExpr:
		v, err := evalWindowArg(s, e.Expr, row, aux, args)
		if err != nil {
			return nil, err
		}
		typ, _ := catalog.ParseType(string(e.Type.Name))
		return applyAffinity(catalog.Column{Type: typ}, v), nil
	case *ast.FuncCall:
		if isWindowCall(e) {
			return nil, fmt.Errorf("nested window function is not supported: %s", funcCallName(e))
		}
		if isAggregateCall(e) {
			return nil, fmt.Errorf("aggregate %s must be computed before the window function", funcCallName(e))
		}
		fnArgs := make([]any, 0, len(e.Args))
		for _, a := range e.Args {
			v, err := evalWindowArg(s, a, row, aux, args)
			if err != nil {
				return nil, err
			}
			fnArgs = append(fnArgs, v)
		}
		return callScalarFunc(funcCallName(e), fnArgs)
	}
	return evalExprState(s, expr, row, &binder{args: args})
}

// binds it to its resolved window specification.
// collectWindowCalls gathers every window function in the select list and
// binds it to its resolved window specification.
func collectWindowCalls(st *ast.SelectStmt, exprs []ast.Expr) ([]ast.Expr, []windowCall, error) {
	var keys []ast.Expr
	var calls []windowCall
	for _, expr := range exprs {
		var found []*ast.FuncCall
		walkExprForWindow(expr, func(fn *ast.FuncCall) {
			found = append(found, fn)
		})
		for _, fn := range found {
			if fn.Name == nil || len(fn.Name.Parts) == 0 {
				return nil, nil, fmt.Errorf("window function is missing a name")
			}
			spec, err := resolveWindowSpec(st, fn.Over, 0)
			if err != nil {
				return nil, nil, err
			}
			keys = append(keys, ast.Expr(fn))
			calls = append(calls, windowCall{
				fn:      fn,
				name:    strings.ToLower(fn.Name.Parts[len(fn.Name.Parts)-1].Unquoted),
				spec:    spec,
				argIdxs: make([]int, len(fn.Args)),
			})
		}
	}
	return keys, calls, nil
}

// evalWindowValues computes each window call's value for every input row and
// returns them keyed by the originating AST node. rows are the rows the window
// functions run over: the filtered input for a plain select, or the grouped
// output when GROUP BY/aggregates are present.
func evalWindowValues(s *persistedState, calls []windowCall, rows []map[string]any, args []any, aux []map[ast.Expr]any) (map[ast.Expr][]any, error) {
	out := make(map[ast.Expr][]any, len(calls))
	if len(calls) == 0 {
		return out, nil
	}
	for i := range calls {
		vals, err := evalOneWindow(s, calls[i], rows, args, aux)
		if err != nil {
			return nil, err
		}
		out[ast.Expr(calls[i].fn)] = vals
	}
	return out, nil
}

// evalOneWindow evaluates a single window function over all rows.
func evalOneWindow(s *persistedState, call windowCall, rows []map[string]any, args []any, aux []map[ast.Expr]any) ([]any, error) {
	n := len(rows)
	vals := make([]any, n)
	if n == 0 {
		return vals, nil
	}

	// Evaluate the function's own arguments row by row. These are ordinary
	// scalar expressions evaluated before framing.
	argVals := make([][]any, len(call.fn.Args))
	for a, argExpr := range call.fn.Args {
		col := make([]any, n)
		for i, row := range rows {
			var rowAux map[ast.Expr]any
			if i < len(aux) {
				rowAux = aux[i]
			}
			v, err := evalWindowArg(s, argExpr, row, rowAux, args)
			if err != nil {
				return nil, err
			}
			col[i] = v
		}
		argVals[a] = col
	}

	partition, order := windowFrames(call.spec)
	// group rows into partitions, preserving original indices.
	type idxRow struct {
		idx  int
		vals map[string]any
	}
	var orderKeys [][]any
	if len(order) > 0 {
		orderKeys = make([][]any, n)
		for i, row := range rows {
			keys := make([]any, len(order))
			var rowAux map[ast.Expr]any
			if i < len(aux) {
				rowAux = aux[i]
			}
			for k, item := range order {
				v, err := evalWindowArg(s, item.Expr, row, rowAux, args)
				if err != nil {
					return nil, err
				}
				keys[k] = v
			}
			orderKeys[i] = keys
		}
	}

	partitionKeys := make([]string, n)
	for i, row := range rows {
		var b strings.Builder
		var rowAux map[ast.Expr]any
		if i < len(aux) {
			rowAux = aux[i]
		}
		for _, pexpr := range partition {
			v, err := evalWindowArg(s, pexpr, row, rowAux, args)
			if err != nil {
				return nil, err
			}
			b.WriteString(rowKey(1, []any{v}))
		}
		partitionKeys[i] = b.String()
	}

	groups := map[string][]int{}
	var groupOrder []string
	for i := 0; i < n; i++ {
		k := partitionKeys[i]
		if _, ok := groups[k]; !ok {
			groupOrder = append(groupOrder, k)
		}
		groups[k] = append(groups[k], i)
	}

	for _, gk := range groupOrder {
		idxs := groups[gk]
		// Sort the partition by the window ORDER BY.
		if len(order) > 0 {
			sort.SliceStable(idxs, func(x, y int) bool {
				a, b := idxs[x], idxs[y]
				for k, item := range order {
					// NULLs sort first for ASC, last for DESC: SQLite orders
					// NULLs as smaller than any other value.
					lv, rv := orderKeys[a][k], orderKeys[b][k]
					if lv == nil && rv == nil {
						continue
					}
					if lv == nil {
						return !item.Desc
					}
					if rv == nil {
						return item.Desc
					}
					c, _ := compareValues(lv, rv, collationForExpr(nil, item.Expr))
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
		if err := fillWindowGroup(s, call, idxs, rows, orderKeys, argVals, vals, aux, args); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// windowFrames extracts partition and ordering terms from a window spec.
func windowFrames(spec *ast.WindowSpec) (partition []ast.Expr, order []ast.OrderByItem) {
	if spec == nil {
		return nil, nil
	}
	return spec.PartitionBy, spec.OrderBy
}

// fillWindowGroup computes one window function's values for a single ordered
// partition.
func fillWindowGroup(s *persistedState, call windowCall, idxs []int, rows []map[string]any, orderKeys [][]any, argVals [][]any, vals []any, aux []map[ast.Expr]any, args []any) error {
	n := len(idxs)
	if n == 0 {
		return nil
	}
	// Peer groups: rows sharing an ORDER BY key are peers. The default frame
	// (RANGE UNBOUNDED PRECEDING AND CURRENT ROW) covers all peers of the
	// current row; with no ORDER BY the frame is the whole partition.
	peerEnd := make([]int, n) // exclusive end of each row's peer group
	peerRank := make([]int, n)
	{
		i := 0
		for i < n {
			j := i + 1
			for j < n && sameOrderKeys(orderKeys, idxs[i], idxs[j]) {
				j++
			}
			for k := i; k < j; k++ {
				peerEnd[k] = j
				peerRank[k] = i
			}
			i = j
		}
	}

	frameStart, frameEnd, exclude, err := windowFrameBounds(call.spec, n, idxs, orderKeys, peerRank, peerEnd)
	if err != nil {
		return err
	}

	switch call.name {
	case "row_number":
		for k := 0; k < n; k++ {
			vals[idxs[k]] = int64(k + 1)
		}
		return nil
	case "rank":
		for k := 0; k < n; k++ {
			vals[idxs[k]] = int64(peerRank[k] + 1)
		}
		return nil
	case "dense_rank":
		dr := make([]int64, n)
		var cur int64 = 1
		for k := 0; k < n; k++ {
			if k > 0 && !sameOrderKeys(orderKeys, idxs[k-1], idxs[k]) {
				cur++
			}
			dr[k] = cur
		}
		for k := 0; k < n; k++ {
			vals[idxs[k]] = dr[k]
		}
		return nil
	case "percent_rank":
		for k := 0; k < n; k++ {
			if n == 1 {
				vals[idxs[k]] = float64(0)
			} else {
				vals[idxs[k]] = float64(peerRank[k]) / float64(n-1)
			}
		}
		return nil
	case "cume_dist":
		for k := 0; k < n; k++ {
			vals[idxs[k]] = float64(peerEnd[k]) / float64(n)
		}
		return nil
	case "ntile":
		buckets, err := windowIntArg(call, 0, rows[idxs[0]], argVals, 1, idxs[0])
		if err != nil {
			return err
		}
		if buckets < 1 {
			buckets = 1
		}
		// SQLite distributes rows so the first (n % buckets) buckets get one
		// extra row each.
		base := n / buckets
		extra := n % buckets
		var bucket int64 = 1
		count := 0
		size := base
		if extra > 0 {
			size++
		}
		for k := 0; k < n; k++ {
			vals[idxs[k]] = bucket
			count++
			if count == size && bucket < int64(buckets) {
				bucket++
				count = 0
				size = base
				if bucket <= int64(extra) {
					size++
				}
			}
		}
		return nil
	case "lag", "lead":
		isLead := call.name == "lead"
		for k := 0; k < n; k++ {
			offset, err := windowIntArg(call, 1, rows[idxs[k]], argVals, 1, idxs[k])
			if err != nil {
				return err
			}
			if offset < 0 {
				offset = 0
			}
			var src int
			if isLead {
				src = k + offset
			} else {
				src = k - offset
			}
			if src < 0 || src >= n {
				if len(call.fn.Args) >= 3 {
					var rowAux map[ast.Expr]any
					if idxs[k] < len(aux) {
						rowAux = aux[idxs[k]]
					}
					dv, err := evalWindowArg(s, call.fn.Args[2], rows[idxs[k]], rowAux, args)
					if err != nil {
						return err
					}
					vals[idxs[k]] = dv
				} else {
					vals[idxs[k]] = nil
				}
				continue
			}
			if len(argVals) > 0 && len(argVals[0]) > 0 {
				vals[idxs[k]] = argVals[0][idxs[src]]
			} else {
				vals[idxs[k]] = nil
			}
		}
		return nil
	case "first_value", "last_value", "nth_value":
		return fillValueFrame(call, idxs, argVals, vals, frameStart, frameEnd, peerRank, peerEnd, exclude)
	}

	// Remaining window functions are aggregate functions over the frame.
	return fillAggregateWindow(call, idxs, argVals, vals, frameStart, frameEnd, peerRank, peerEnd, exclude)
}

type rowsFrameBound struct {
	kind   string
	offset float64
}

func windowFrameBounds(spec *ast.WindowSpec, n int, idxs []int, orderKeys [][]any, peerRank, peerEnd []int) ([]int, []int, string, error) {
	if spec == nil || len(spec.Raw) == 0 || strings.TrimSpace(string(spec.Raw)) == "" {
		return nil, peerEnd, "no others", nil
	}
	starts := make([]int, n)
	ends := make([]int, n)
	frameType, start, end, exclude, err := parseWindowFrame(string(spec.Raw))
	if err != nil {
		return nil, nil, "", err
	}
	if frameType == "groups" && len(spec.OrderBy) == 0 {
		return nil, nil, "", fmt.Errorf("GROUPS frame requires ORDER BY")
	}
	rangeOffsets := frameType == "range" && (start.kind == "preceding" || start.kind == "following" || end.kind == "preceding" || end.kind == "following")
	if rangeOffsets && len(spec.OrderBy) != 1 {
		return nil, nil, "", fmt.Errorf("RANGE offset frames require exactly one ORDER BY expression")
	}
	var groupStarts []int
	if frameType == "groups" {
		for row := 0; row < n; row = peerEnd[row] {
			groupStarts = append(groupStarts, row)
		}
	}
	for i := 0; i < n; i++ {
		switch frameType {
		case "rows":
			starts[i] = resolveRowsBound(start, i, n, false)
			ends[i] = resolveRowsBound(end, i, n, true)
		case "groups":
			starts[i] = resolveGroupsBound(start, i, n, groupStarts, peerRank, peerEnd, false)
			ends[i] = resolveGroupsBound(end, i, n, groupStarts, peerRank, peerEnd, true)
		case "range":
			if rangeOffsets && (start.kind == "preceding" || start.kind == "following") {
				starts[i] = resolveRangeOffsetBound(start, i, n, idxs, orderKeys, peerRank, peerEnd, spec.OrderBy[0].Desc, false)
			} else {
				starts[i] = resolveRangeBound(start, i, n, peerRank, peerEnd, false)
			}
			if rangeOffsets && (end.kind == "preceding" || end.kind == "following") {
				ends[i] = resolveRangeOffsetBound(end, i, n, idxs, orderKeys, peerRank, peerEnd, spec.OrderBy[0].Desc, true)
			} else {
				ends[i] = resolveRangeBound(end, i, n, peerRank, peerEnd, true)
			}
		}
		if ends[i] < starts[i] {
			ends[i] = starts[i]
		}
	}
	return starts, ends, exclude, nil
}

func parseWindowFrame(raw string) (string, rowsFrameBound, rowsFrameBound, string, error) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(raw)))
	if len(fields) == 0 || (fields[0] != "rows" && fields[0] != "groups" && fields[0] != "range") {
		return "", rowsFrameBound{}, rowsFrameBound{}, "", fmt.Errorf("unsupported window frame: %s", strings.TrimSpace(raw))
	}
	frameType := fields[0]
	idx := 1
	var start, end rowsFrameBound
	exclude := "no others"
	if idx < len(fields) && fields[idx] == "between" {
		idx++
		var err error
		start, idx, err = parseRowsFrameBound(fields, idx, frameType)
		if err != nil {
			return frameType, start, end, exclude, err
		}
		if idx >= len(fields) || fields[idx] != "and" {
			return frameType, start, end, exclude, fmt.Errorf("invalid %s frame: expected AND", strings.ToUpper(frameType))
		}
		end, idx, err = parseRowsFrameBound(fields, idx+1, frameType)
		if err != nil {
			return frameType, start, end, exclude, err
		}
	} else {
		var err error
		start, idx, err = parseRowsFrameBound(fields, idx, frameType)
		if err != nil {
			return frameType, start, end, exclude, err
		}
		end = rowsFrameBound{kind: "current row"}
	}
	if idx < len(fields) && fields[idx] == "exclude" {
		idx++
		if idx >= len(fields) {
			return frameType, start, end, exclude, fmt.Errorf("invalid EXCLUDE clause")
		}
		switch fields[idx] {
		case "no":
			if idx+1 >= len(fields) || fields[idx+1] != "others" {
				return frameType, start, end, exclude, fmt.Errorf("invalid EXCLUDE NO OTHERS clause")
			}
			exclude, idx = "no others", idx+2
		case "current":
			if idx+1 >= len(fields) || fields[idx+1] != "row" {
				return frameType, start, end, exclude, fmt.Errorf("invalid EXCLUDE CURRENT ROW clause")
			}
			exclude, idx = "current row", idx+2
		case "group", "ties":
			exclude, idx = fields[idx], idx+1
		default:
			return frameType, start, end, exclude, fmt.Errorf("unsupported EXCLUDE clause: %s", fields[idx])
		}
	}
	if idx != len(fields) {
		return frameType, start, end, exclude, fmt.Errorf("unsupported %s frame clause: %s", strings.ToUpper(frameType), strings.Join(fields[idx:], " "))
	}
	if start.kind == "unbounded following" || end.kind == "unbounded preceding" {
		return frameType, start, end, exclude, fmt.Errorf("invalid %s frame boundary", strings.ToUpper(frameType))
	}
	return frameType, start, end, exclude, nil
}

func parseRowsFrameBound(fields []string, idx int, frameType string) (rowsFrameBound, int, error) {
	if idx >= len(fields) {
		return rowsFrameBound{}, idx, fmt.Errorf("invalid ROWS frame boundary")
	}
	if fields[idx] == "unbounded" {
		if idx+1 >= len(fields) || (fields[idx+1] != "preceding" && fields[idx+1] != "following") {
			return rowsFrameBound{}, idx, fmt.Errorf("invalid UNBOUNDED ROWS frame boundary")
		}
		return rowsFrameBound{kind: "unbounded " + fields[idx+1]}, idx + 2, nil
	}
	if fields[idx] == "current" {
		if idx+1 >= len(fields) || fields[idx+1] != "row" {
			return rowsFrameBound{}, idx, fmt.Errorf("invalid CURRENT ROW frame boundary")
		}
		return rowsFrameBound{kind: "current row"}, idx + 2, nil
	}
	offset, err := strconv.ParseFloat(fields[idx], 64)
	if err != nil || offset < 0 || math.IsInf(offset, 0) || math.IsNaN(offset) {
		return rowsFrameBound{}, idx, fmt.Errorf("invalid %s frame offset: %s", strings.ToUpper(frameType), fields[idx])
	}
	if frameType != "range" && (math.Trunc(offset) != offset || offset > float64(int(^uint(0)>>1))) {
		return rowsFrameBound{}, idx, fmt.Errorf("invalid %s frame offset: %s", strings.ToUpper(frameType), fields[idx])
	}
	if idx+1 >= len(fields) || (fields[idx+1] != "preceding" && fields[idx+1] != "following") {
		return rowsFrameBound{}, idx, fmt.Errorf("invalid ROWS frame direction")
	}
	return rowsFrameBound{kind: fields[idx+1], offset: offset}, idx + 2, nil
}

func resolveRowsBound(bound rowsFrameBound, row, n int, exclusiveEnd bool) int {
	var pos int
	switch bound.kind {
	case "unbounded preceding":
		pos = 0
	case "unbounded following":
		pos = n
	case "current row":
		pos = row
	case "preceding":
		pos = row - int(bound.offset)
	case "following":
		pos = row + int(bound.offset)
	}
	if exclusiveEnd {
		if bound.kind == "current row" || bound.kind == "preceding" || bound.kind == "following" {
			pos++
		}
	}
	if pos < 0 {
		pos = 0
	}
	if pos > n {
		pos = n
	}
	return pos
}

func resolveGroupsBound(bound rowsFrameBound, row, n int, groupStarts, peerRank, peerEnd []int, exclusiveEnd bool) int {
	switch bound.kind {
	case "unbounded preceding":
		return 0
	case "unbounded following":
		return n
	case "current row":
		if exclusiveEnd {
			return peerEnd[row]
		}
		return peerRank[row]
	}
	group := sort.SearchInts(groupStarts, peerRank[row])
	target := group - int(bound.offset)
	if bound.kind == "following" {
		target = group + int(bound.offset)
	}
	if target < 0 {
		return 0
	}
	if target >= len(groupStarts) {
		return n
	}
	if exclusiveEnd {
		return peerEnd[groupStarts[target]]
	}
	return groupStarts[target]
}

func resolveRangeBound(bound rowsFrameBound, row, n int, peerRank, peerEnd []int, exclusiveEnd bool) int {
	switch bound.kind {
	case "unbounded preceding":
		return 0
	case "unbounded following":
		return n
	case "current row":
		if exclusiveEnd {
			return peerEnd[row]
		}
		return peerRank[row]
	default:
		return 0
	}
}

func resolveRangeOffsetBound(bound rowsFrameBound, row, n int, idxs []int, orderKeys [][]any, peerRank, peerEnd []int, desc, exclusiveEnd bool) int {
	if len(orderKeys) == 0 || len(orderKeys[idxs[row]]) == 0 {
		if exclusiveEnd {
			return peerEnd[row]
		}
		return peerRank[row]
	}
	currentValue := orderKeys[idxs[row]][0]
	switch currentValue.(type) {
	case string, []byte:
		// Text/blob values use peer equality for RANGE offsets, even if text
		// happens to contain digits.
		if exclusiveEnd {
			return peerEnd[row]
		}
		return peerRank[row]
	}
	current, ok := asFloat(currentValue)
	if !ok {
		// SQLite treats RANGE offsets on non-numeric order values as peers.
		if exclusiveEnd {
			return peerEnd[row]
		}
		return peerRank[row]
	}
	target := current
	switch bound.kind {
	case "preceding":
		if desc {
			target += bound.offset
		} else {
			target -= bound.offset
		}
	case "following":
		if desc {
			target -= bound.offset
		} else {
			target += bound.offset
		}
	}
	return sort.Search(n, func(i int) bool {
		value := orderKeys[idxs[i]][0]
		cmp, comparable := compareRangeOrderValue(value, target)
		if !comparable {
			return false
		}
		if desc {
			cmp = -cmp
		}
		if exclusiveEnd {
			return cmp > 0
		}
		return cmp >= 0
	})
}

func compareRangeOrderValue(value any, target float64) (int, bool) {
	if value == nil {
		return -1, true
	}
	// SQLite does not coerce text order keys to numbers for RANGE offsets.
	// Text and blobs sort after numeric values in ascending order.
	switch value.(type) {
	case string, []byte:
		return 1, true
	}
	valueNumber, ok := asFloat(value)
	if !ok {
		return 1, true
	}
	switch {
	case valueNumber < target:
		return -1, true
	case valueNumber > target:
		return 1, true
	default:
		return 0, true
	}
}

// sameOrderKeys reports whether two rows are peers under the window ORDER BY.
func sameOrderKeys(orderKeys [][]any, a, b int) bool {
	if len(orderKeys) == 0 {
		return true
	}
	if len(orderKeys[a]) != len(orderKeys[b]) {
		return false
	}
	for i := range orderKeys[a] {
		lv, rv := orderKeys[a][i], orderKeys[b][i]
		if lv == nil && rv == nil {
			continue
		}
		if lv == nil || rv == nil {
			return false
		}
		c, ok := compareValues(lv, rv, "BINARY")
		if !ok || c != 0 {
			return false
		}
	}
	return true
}

// windowIntArg reads an optional integer argument of a window function. Offset
// and bucket-count arguments are ordinary expressions evaluated per row; when
// the argument is absent or NULL the documented default applies.
func windowIntArg(call windowCall, pos int, row map[string]any, argVals [][]any, def int64, rowIdx int) (int, error) {
	if pos < len(argVals) && rowIdx < len(argVals[pos]) {
		if v := argVals[pos][rowIdx]; v != nil {
			if f, ok := asFloat(v); ok {
				return int(f), nil
			}
		}
	}
	return int(def), nil
}

// fillValueFrame implements first_value, last_value and nth_value over the
// default window frame.
func fillValueFrame(call windowCall, idxs []int, argVals [][]any, vals []any, frameStart, frameEnd, peerRank, peerEnd []int, exclude string) error {
	n := len(idxs)
	if len(argVals) == 0 || len(argVals[0]) == 0 {
		return fmt.Errorf("%s expects at least 1 argument", call.name)
	}
	col := argVals[0]
	nth := 1
	if call.name == "nth_value" {
		if len(argVals) > 1 && len(argVals[1]) > 0 {
			if f, ok := asFloat(argVals[1][0]); ok {
				nth = int(f)
			}
		}
		if nth < 1 {
			for k := 0; k < n; k++ {
				vals[idxs[k]] = nil
			}
			return nil
		}
	}
	for k := 0; k < n; k++ {
		start := frameStartAt(frameStart, k)
		end := frameEnd[k]
		target := 1
		if call.name == "nth_value" {
			target = nth
		}
		var value any
		seen := 0
		for pos := start; pos < end; pos++ {
			if windowFrameExcluded(pos, k, peerRank, peerEnd, exclude) {
				continue
			}
			seen++
			value = col[idxs[pos]]
			if (call.name == "first_value" || call.name == "nth_value") && seen == target {
				break
			}
		}
		if seen < target {
			value = nil
		}
		vals[idxs[k]] = value
	}
	return nil
}

// fillAggregateWindow computes aggregate window functions (sum, avg, count,
// min, max, total, group_concat) over the default window frame.
func fillAggregateWindow(call windowCall, idxs []int, argVals [][]any, vals []any, frameStart, frameEnd, peerRank, peerEnd []int, exclude string) error {
	n := len(idxs)
	var col []any
	if len(argVals) > 0 {
		col = argVals[0]
	}
	sep := ","
	if call.name == "group_concat" && len(argVals) > 1 {
		if s, ok := argVals[1][0].(string); ok {
			sep = s
		}
	}
	for k := 0; k < n; k++ {
		start, end := frameStartAt(frameStart, k), frameEnd[k]
		switch call.name {
		case "count":
			c := int64(0)
			if col == nil && exclude == "no others" {
				c = int64(end - start)
				vals[idxs[k]] = c
				continue
			}
			for i := start; i < end; i++ {
				if windowFrameExcluded(i, k, peerRank, peerEnd, exclude) {
					continue
				}
				if col == nil || col[idxs[i]] != nil {
					c++
				}
			}
			vals[idxs[k]] = c
		case "sum", "total":
			var sumF float64
			var sumI int64
			allInt := true
			seen := false
			for i := start; i < end; i++ {
				if windowFrameExcluded(i, k, peerRank, peerEnd, exclude) {
					continue
				}
				if col == nil {
					continue
				}
				v := col[idxs[i]]
				if v == nil {
					continue
				}
				f, ok := asFloat(v)
				if !ok {
					continue
				}
				seen = true
				if !isIntegerValue(v) {
					allInt = false
				} else if iv, iok := asInt(v); iok {
					sumI += iv
				}
				sumF += f
			}
			if call.name == "total" {
				vals[idxs[k]] = sumF
			} else if !seen {
				vals[idxs[k]] = nil
			} else if allInt {
				vals[idxs[k]] = sumI
			} else {
				vals[idxs[k]] = sumF
			}
		case "avg":
			var sum float64
			var c int64
			for i := start; i < end; i++ {
				if windowFrameExcluded(i, k, peerRank, peerEnd, exclude) {
					continue
				}
				if col == nil {
					continue
				}
				if f, ok := asFloat(col[idxs[i]]); ok {
					sum += f
					c++
				}
			}
			if c == 0 {
				vals[idxs[k]] = nil
			} else {
				vals[idxs[k]] = sum / float64(c)
			}
		case "min", "max":
			var best any
			for i := start; i < end; i++ {
				if windowFrameExcluded(i, k, peerRank, peerEnd, exclude) {
					continue
				}
				if col == nil {
					continue
				}
				v := col[idxs[i]]
				if v == nil {
					continue
				}
				if best == nil {
					best = v
					continue
				}
				if c, ok := compareValues(v, best, "BINARY"); ok {
					if (call.name == "min" && c < 0) || (call.name == "max" && c > 0) {
						best = v
					}
				}
			}
			vals[idxs[k]] = best
		case "group_concat", "string_agg":
			var parts []string
			for i := start; i < end; i++ {
				if windowFrameExcluded(i, k, peerRank, peerEnd, exclude) {
					continue
				}
				if col == nil {
					continue
				}
				if col[idxs[i]] == nil {
					continue
				}
				parts = append(parts, stringifySQL(col[idxs[i]]))
			}
			if len(parts) == 0 {
				vals[idxs[k]] = nil
			} else {
				vals[idxs[k]] = strings.Join(parts, sep)
			}
		default:
			return fmt.Errorf("unsupported window function: %s", call.name)
		}
	}
	return nil
}

func windowFrameExcluded(pos, row int, peerRank, peerEnd []int, exclude string) bool {
	switch exclude {
	case "current row":
		return pos == row
	case "group":
		return pos >= peerRank[row] && pos < peerEnd[row]
	case "ties":
		return pos != row && pos >= peerRank[row] && pos < peerEnd[row]
	default:
		return false
	}
}

func frameStartAt(starts []int, row int) int {
	if len(starts) == 0 {
		return 0
	}
	return starts[row]
}

// evalExprWithWindows evaluates a select-list expression, substituting the
// precomputed value of any window function it contains. A window call may be
// nested inside arithmetic or a scalar function, so lookup alone is not enough.
func evalExprWithWindows(s *persistedState, expr ast.Expr, row map[string]any, winVals map[ast.Expr][]any, rowIdx int, b *binder) (any, error) {
	if winVals != nil {
		if vals, ok := winVals[expr]; ok {
			if rowIdx < len(vals) {
				return vals[rowIdx], nil
			}
			return nil, nil
		}
	}
	switch e := expr.(type) {
	case *ast.BinaryExpr:
		l, err := evalExprWithWindows(s, e.Left, row, winVals, rowIdx, b)
		if err != nil {
			return nil, err
		}
		r, err := evalExprWithWindows(s, e.Right, row, winVals, rowIdx, b)
		if err != nil {
			return nil, err
		}
		return evalBinary(l, r, e.Op)
	case *ast.UnaryExpr:
		v, err := evalExprWithWindows(s, e.Expr, row, winVals, rowIdx, b)
		if err != nil {
			return nil, err
		}
		if e.Op == lexer.MINUS {
			return negateNumeric(v)
		}
		if e.Op == lexer.NOT || strings.EqualFold(e.Op.String(), "UNKNOWN") {
			return !truthy(v), nil
		}
		return nil, fmt.Errorf("unsupported unary operator: %s", e.Op)
	case *ast.CastExpr:
		v, err := evalExprWithWindows(s, e.Expr, row, winVals, rowIdx, b)
		if err != nil {
			return nil, err
		}
		typ, _ := catalog.ParseType(string(e.Type.Name))
		return applyAffinity(catalog.Column{Type: typ}, v), nil
	case *ast.CaseExpr:
		if e.Operand != nil {
			ov, err := evalExprWithWindows(s, e.Operand, row, winVals, rowIdx, b)
			if err != nil {
				return nil, err
			}
			for _, w := range e.Whens {
				cv, err := evalExprWithWindows(s, w.Cond, row, winVals, rowIdx, b)
				if err != nil {
					return nil, err
				}
				if c, ok := compareValues(ov, cv, "BINARY"); ok && c == 0 {
					return evalExprWithWindows(s, w.Result, row, winVals, rowIdx, b)
				}
			}
		} else {
			for _, w := range e.Whens {
				cv, err := evalExprWithWindows(s, w.Cond, row, winVals, rowIdx, b)
				if err != nil {
					return nil, err
				}
				if truthy(cv) {
					return evalExprWithWindows(s, w.Result, row, winVals, rowIdx, b)
				}
			}
		}
		if e.Else != nil {
			return evalExprWithWindows(s, e.Else, row, winVals, rowIdx, b)
		}
		return nil, nil
	case *ast.FuncCall:
		if isWindowCall(e) {
			return nil, fmt.Errorf("window function %s is not bound to a result", funcCallName(e))
		}
		if isAggregateCall(e) {
			return nil, fmt.Errorf("aggregate %s cannot appear here", funcCallName(e))
		}
		fnArgs := make([]any, 0, len(e.Args))
		for _, a := range e.Args {
			v, err := evalExprWithWindows(s, a, row, winVals, rowIdx, b)
			if err != nil {
				return nil, err
			}
			fnArgs = append(fnArgs, v)
		}
		return callScalarFunc(funcCallName(e), fnArgs)
	}
	return evalExprState(s, expr, row, b)
}
