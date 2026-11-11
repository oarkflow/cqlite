package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	catalog "github.com/sujit/sqlite/internal/catalog"
)

func init() {
	registerVTabModule(fts5Module{})
}

// fts5Module implements the fts5 virtual table module. The inverted index is
// kept in shadow tables owned by the engine's ordinary storage, so checkpoint,
// WAL replay and crash recovery apply to it unchanged.
type fts5Module struct{}

func (fts5Module) Name() string { return "fts5" }

// fts5Args is the parsed argument list of CREATE VIRTUAL TABLE ... USING fts5.
type fts5Args struct {
	cols         []string
	tokenize     string
	contentTable string
	contentRowid string
	contentless  bool
	prefix       string
	unindexed    map[string]bool
}

func parseFTS5Args(args string) (*fts5Args, error) {
	out := &fts5Args{tokenize: "unicode61", unindexed: map[string]bool{}}
	for _, part := range splitFTS5Args(args) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		switch {
		case strings.HasPrefix(lower, "tokenize"):
			out.tokenize = unquoteArgValue(part, "tokenize")
		case strings.HasPrefix(lower, "content_rowid"):
			out.contentRowid = unquoteArgValue(part, "content_rowid")
		case strings.HasPrefix(lower, "content"):
			v := unquoteArgValue(part, "content")
			if v == "" || strings.EqualFold(v, "delete") {
				out.contentless = true
			} else {
				out.contentTable = v
			}
		case strings.HasPrefix(lower, "prefix"):
			out.prefix = unquoteArgValue(part, "prefix")
		case strings.HasPrefix(lower, "columnsize") || strings.HasPrefix(lower, "detail") ||
			strings.HasPrefix(lower, "optimize") || strings.HasPrefix(lower, "securedelete"):
			// Accepted and ignored; these tune storage rather than behaviour.
			continue
		default:
			// A column definition, optionally with UNINDEXED.
			name := part
			unindexed := false
			if i := strings.Index(strings.ToUpper(part), " UNINDEXED"); i >= 0 {
				name = part[:i]
				unindexed = true
			}
			name = strings.TrimSpace(name)
			name = strings.Trim(name, `"'`)
			if name == "" {
				continue
			}
			out.cols = append(out.cols, name)
			if unindexed {
				out.unindexed[strings.ToLower(name)] = true
			}
		}
	}
	return out, nil
}

// splitFTS5Args splits the argument list on top-level commas.
func splitFTS5Args(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// unquoteArgValue extracts the value of a "name = value" argument.
func unquoteArgValue(part, name string) string {
	i := strings.Index(part, "=")
	if i < 0 {
		return ""
	}
	v := strings.TrimSpace(part[i+1:])
	v = strings.Trim(v, `"'`)
	return v
}

func (m fts5Module) Declare(s *persistedState, vt *VirtualTable, args string) error {
	parsed, err := parseFTS5Args(args)
	if err != nil {
		return err
	}
	if len(parsed.cols) == 0 {
		return errors.New("fts5 table requires at least one column")
	}
	tok, err := tokenizerName(parsed.tokenize)
	if err != nil {
		return err
	}
	_ = tok
	vt.Columns = append([]string(nil), parsed.cols...)
	vt.ModuleKV["tokenize"] = parsed.tokenize
	vt.ModuleKV["content_table"] = parsed.contentTable
	vt.ModuleKV["content_rowid"] = parsed.contentRowid
	vt.ModuleKV["prefix"] = parsed.prefix
	if parsed.contentless {
		vt.ModuleKV["contentless"] = "1"
	}
	unindexed := make([]string, 0, len(parsed.unindexed))
	for k := range parsed.unindexed {
		unindexed = append(unindexed, k)
	}
	sort.Strings(unindexed)
	vt.ModuleKV["unindexed"] = strings.Join(unindexed, ",")

	contentTbl := vt.Name + "_content"
	termsTbl := vt.Name + "_terms"
	docsizeTbl := vt.Name + "_docsize"
	vt.Shadows = []string{termsTbl, docsizeTbl}
	if !parsed.contentless {
		vt.Shadows = append([]string{contentTbl}, vt.Shadows...)
	}

	// The content table stores the original column text so highlight() and
	// snippet() can rebuild it, and so UPDATE/DELETE can find the old text.
	colDefs := make([]string, 0, len(vt.Columns))
	for i, c := range vt.Columns {
		colDefs = append(colDefs, fmt.Sprintf("c%d TEXT", i))
		_ = c
	}
	if !parsed.contentless {
		if err := execShadowSQL(s, fmt.Sprintf(
			"CREATE TABLE %s (rowid INTEGER PRIMARY KEY, %s)", contentTbl, strings.Join(colDefs, ", "))); err != nil {
			return err
		}
	}
	if err := execShadowSQL(s, fmt.Sprintf(
		"CREATE TABLE %s (term TEXT, col INTEGER, docid INTEGER, poss TEXT)", termsTbl)); err != nil {
		return err
	}
	if err := execShadowSQL(s, fmt.Sprintf(
		"CREATE TABLE %s (docid INTEGER PRIMARY KEY, %s)", docsizeTbl, joinIntCols("l", len(vt.Columns)))); err != nil {
		return err
	}
	// An index on the term column makes prefix and equality lookups cheap.
	if err := execShadowSQL(s, fmt.Sprintf(
		"CREATE INDEX %s_term ON %s (term)", termsTbl, termsTbl)); err != nil {
		return err
	}
	if err := execShadowSQL(s, fmt.Sprintf(
		"CREATE INDEX %s_docid ON %s (docid)", termsTbl, termsTbl)); err != nil {
		return err
	}
	return nil
}

func joinIntCols(prefix string, n int) string {
	if n == 0 {
		return prefix + "0 INTEGER"
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("%s%d INTEGER", prefix, i))
	}
	return strings.Join(parts, ", ")
}

// execShadowSQL runs a statement against the state, used to build the shadow
// tables a virtual table owns.
func execShadowSQL(s *persistedState, sql string) error {
	_, err := execute(context.Background(), s, sql)
	return err
}

func (m fts5Module) Open(s *persistedState, vt *VirtualTable) (vtabInstance, error) {
	tok, err := tokenizerName(vt.ModuleKV["tokenize"])
	if err != nil {
		return nil, err
	}
	unindexed := map[string]bool{}
	if u := vt.ModuleKV["unindexed"]; u != "" {
		for _, name := range strings.Split(u, ",") {
			unindexed[name] = true
		}
	}
	return &fts5Instance{
		s:         s,
		vt:        vt,
		tok:       tok,
		unindexed: unindexed,
	}, nil
}

// fts5Instance is a live fts5 virtual table.
type fts5Instance struct {
	s         *persistedState
	vt        *VirtualTable
	tok       Tokenizer
	unindexed map[string]bool
}

func (f *fts5Instance) contentTable() string  { return f.vt.Name + "_content" }
func (f *fts5Instance) termsTable() string    { return f.vt.Name + "_terms" }
func (f *fts5Instance) docsizeTable() string  { return f.vt.Name + "_docsize" }
func (f *fts5Instance) ColumnNames() []string { return f.vt.Columns }

// Tokenizer exposes the tokenizer so auxiliary functions can normalize terms
// the same way the index does.
func (f *fts5Instance) Tokenizer() Tokenizer { return f.tok }

func (f *fts5Instance) Insert(rowid int64, values []any) (int64, error) {
	autoRowID := rowid == 0
	if rowid == 0 {
		tbl := f.shadowSchema(f.contentTable())
		if f.vt.ModuleKV["contentless"] == "1" {
			tbl = f.shadowSchema(f.docsizeTable())
		}
		if tbl != nil {
			rowid = tbl.NextRowID
			tbl.NextRowID++
		}
	}
	if !autoRowID {
		if err := f.removeDocument(rowid); err != nil {
			return 0, err
		}
	}
	cols := f.vt.Columns
	texts := make([]string, len(cols))
	for i := range cols {
		if i < len(values) && values[i] != nil {
			texts[i] = stringifySQL(values[i])
		}
	}
	textVals := make([]any, len(texts))
	for i := range texts {
		textVals[i] = texts[i]
	}
	if f.vt.ModuleKV["contentless"] != "1" {
		if err := f.shadowInsert(f.contentTable(), rowid, textVals); err != nil {
			return 0, err
		}
	}
	// Build the inverted index for this document.
	postings := map[string][]int{}
	lens := make([]int64, len(cols))
	for c, text := range texts {
		name := strings.ToLower(cols[c])
		toks := f.tok.Tokenize(text)
		lens[c] = int64(len(toks))
		if f.unindexed[name] {
			continue
		}
		for pos, t := range toks {
			key := t.Term + "\x00" + strconv.Itoa(c)
			postings[key] = append(postings[key], pos)
		}
	}
	keys := make([]string, 0, len(postings))
	for k := range postings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		term := parts[0]
		col, _ := strconv.Atoi(parts[1])
		poss := encodePositions(postings[key])
		if err := f.shadowInsert(f.termsTable(), 0, []any{term, int64(col), rowid, poss}); err != nil {
			return 0, err
		}
	}
	if err := f.shadowInsert(f.docsizeTable(), rowid, intsToAny(lens)); err != nil {
		return 0, err
	}
	return rowid, nil
}

func intsToAny(v []int64) []any {
	out := make([]any, len(v))
	for i := range v {
		out[i] = v[i]
	}
	return out
}

func encodePositions(pos []int) string {
	parts := make([]string, len(pos))
	for i, p := range pos {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

func decodePositions(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (f *fts5Instance) Update(rowid int64, values []any) error {
	if f.vt.ModuleKV["contentless"] == "1" {
		return fmt.Errorf("cannot UPDATE contentless fts5 table: %s", f.vt.Name)
	}
	if _, err := f.Insert(rowid, values); err != nil {
		return err
	}
	return nil
}

func (f *fts5Instance) Delete(rowid int64) error {
	if f.vt.ModuleKV["contentless"] == "1" {
		return fmt.Errorf("cannot DELETE from contentless fts5 table: %s", f.vt.Name)
	}
	return f.removeDocument(rowid)
}

func (f *fts5Instance) removeDocument(rowid int64) error {
	if f.vt.ModuleKV["contentless"] != "1" {
		if err := f.shadowDeleteWhere(f.contentTable(), rowid); err != nil {
			return err
		}
	}
	if err := f.shadowDeleteWhere(f.docsizeTable(), rowid); err != nil {
		return err
	}
	return f.shadowDeleteTerms(rowid)
}

func (f *fts5Instance) Row(rowid int64) ([]any, bool) {
	if f.vt.ModuleKV["contentless"] == "1" {
		if f.hasDoc(rowid) {
			return make([]any, len(f.vt.Columns)), true
		}
		return nil, false
	}
	if tableName := catalog.Normalize(f.vt.ModuleKV["content_table"]); tableName != "" {
		table, data := f.externalContent(tableName)
		if table == nil || data == nil {
			return nil, false
		}
		rowidName := catalog.Normalize(f.vt.ModuleKV["content_rowid"])
		if rowidName == "" {
			rowidName = "rowid"
		}
		for i := range data.Rows {
			row := &data.Rows[i]
			if row.Deleted != 0 {
				continue
			}
			var id int64
			if rowidName == "rowid" || rowidName == "_rowid_" || rowidName == "oid" {
				id = row.RowID
			} else {
				value, ok := asInt(row.value(table, rowidName))
				if !ok {
					continue
				}
				id = value
			}
			if id != rowid {
				continue
			}
			out := make([]any, len(f.vt.Columns))
			for colIndex, col := range f.vt.Columns {
				out[colIndex] = row.value(table, catalog.Normalize(col))
			}
			return out, true
		}
		return nil, false
	}
	tbl := f.shadowSchema(f.contentTable())
	if tbl == nil {
		return nil, false
	}
	data := f.s.Tables[catalog.Normalize(f.contentTable())]
	if data == nil {
		return nil, false
	}
	pos, ok := data.lookupRowID(rowid)
	if !ok || pos < 0 || pos >= len(data.Rows) || data.Rows[pos].Deleted != 0 {
		return nil, false
	}
	row := data.Rows[pos]
	row.ensureValues(tbl)
	out := make([]any, len(f.vt.Columns))
	for i := range f.vt.Columns {
		out[i] = row.Values[fmt.Sprintf("c%d", i)]
	}
	return out, true
}

func (f *fts5Instance) RowIDs() []int64 {
	if f.vt.ModuleKV["contentless"] == "1" {
		data := f.s.Tables[catalog.Normalize(f.docsizeTable())]
		if data == nil {
			return nil
		}
		var out []int64
		for i := range data.Rows {
			if data.Rows[i].Deleted == 0 {
				out = append(out, data.Rows[i].RowID)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	if tableName := catalog.Normalize(f.vt.ModuleKV["content_table"]); tableName != "" {
		table, data := f.externalContent(tableName)
		if table == nil || data == nil {
			return nil
		}
		rowidName := catalog.Normalize(f.vt.ModuleKV["content_rowid"])
		if rowidName == "" {
			rowidName = "rowid"
		}
		var out []int64
		for i := range data.Rows {
			row := &data.Rows[i]
			if row.Deleted != 0 {
				continue
			}
			if rowidName == "rowid" || rowidName == "_rowid_" || rowidName == "oid" {
				out = append(out, row.RowID)
				continue
			}
			if id, ok := asInt(row.value(table, rowidName)); ok {
				out = append(out, id)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	data := f.s.Tables[catalog.Normalize(f.contentTable())]
	if data == nil {
		return nil
	}
	tbl := f.shadowSchema(f.contentTable())
	var out []int64
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		row.ensureValues(tbl)
		out = append(out, row.RowID)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

func (f *fts5Instance) hasDoc(rowid int64) bool {
	data := f.s.Tables[catalog.Normalize(f.docsizeTable())]
	if data == nil {
		return false
	}
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.RowID == rowid && row.Deleted == 0 {
			return true
		}
	}
	return false
}

// optimize compacts FTS shadow tables by dropping tombstoned row versions.
// The engine stores one logical index rather than FTS5's multi-segment index,
// so merging segments is unnecessary; removing obsolete versions is the
// corresponding useful maintenance operation here.
func (f *fts5Instance) optimize() error {
	for _, name := range f.vt.Shadows {
		key := catalog.Normalize(name)
		data := f.s.Tables[key]
		table := f.s.Catalog.Tables[key]
		if data == nil || table == nil {
			continue
		}
		data.materializePending(table)
		live := make([]rowVersion, 0, len(data.Rows))
		for _, row := range data.Rows {
			if row.Deleted == 0 {
				live = append(live, row)
			}
		}
		data.Rows = live
		data.rowsShared = false
		data.baseRows = len(live)
		data.rebuildRuntime(table)
	}
	return nil
}

func (f *fts5Instance) externalContent(name string) (*catalog.Table, *tableData) {
	table, ok := f.s.Catalog.Table(name)
	if !ok {
		return nil, nil
	}
	return table, f.s.Tables[catalog.Normalize(name)]
}

// Query evaluates a MATCH expression and returns matching rowids ordered by
// descending relevance.
func (f *fts5Instance) Query(q string, cols []int) ([]int64, []float64, error) {
	return f.query(q, cols, true)
}

// QueryUnranked uses the term index to find matches without scanning document
// lengths or computing BM25 scores. SELECTs that do not consume rank can use
// this path and return deterministic rowid order.
func (f *fts5Instance) QueryUnranked(q string, cols []int) ([]int64, []float64, error) {
	return f.query(q, cols, false)
}

func (f *fts5Instance) query(q string, cols []int, ranked bool) ([]int64, []float64, error) {
	node, err := parseFTSQuery(q, f.vt.Columns)
	if err != nil {
		return nil, nil, err
	}
	if node == nil {
		ids := f.RowIDs()
		scores := make([]float64, len(ids))
		return ids, scores, nil
	}
	postings := f.loadPostings(node)
	var all []int64
	if ranked || ftsNodeHasNot(node) {
		all = f.RowIDs()
	}
	eval := ftsEval{f: f, postings: postings, all: all}
	matches := eval.eval(node)
	if !ranked {
		ids := make([]int64, 0, len(matches))
		for id := range matches {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return ids, make([]float64, len(ids)), nil
	}
	type scored struct {
		id    int64
		score float64
	}
	out := make([]scored, 0, len(matches))
	for id := range matches {
		out = append(out, scored{id: id, score: f.bm25For(node, id, postings, all)})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].score == out[b].score {
			return out[a].id < out[b].id
		}
		return out[a].score < out[b].score // bm25 is negative; lower is better
	})
	ids := make([]int64, len(out))
	scores := make([]float64, len(out))
	for i, s := range out {
		ids[i] = s.id
		scores[i] = s.score
	}
	_ = cols
	return ids, scores, nil
}

func ftsNodeHasNot(node ftsNode) bool {
	switch n := node.(type) {
	case ftsNot:
		return true
	case ftsAnd:
		return ftsNodeHasNot(n.l) || ftsNodeHasNot(n.r)
	case ftsOr:
		return ftsNodeHasNot(n.l) || ftsNodeHasNot(n.r)
	default:
		return false
	}
}

// ftsPostings maps a term to the documents and positions containing it.
type ftsPostings map[string]map[int64][]int // key: term + "\x00" + col

func (f *fts5Instance) loadPostings(node ftsNode) ftsPostings {
	out := ftsPostings{}
	data := f.s.Tables[catalog.Normalize(f.termsTable())]
	if data == nil {
		return out
	}
	tbl := f.shadowSchema(f.termsTable())
	var termIndex *catalog.Index
	for i := range tbl.Indexes {
		idx := &tbl.Indexes[i]
		if len(idx.Columns) == 1 && catalog.Normalize(idx.Columns[0]) == "term" {
			termIndex = idx
			break
		}
	}
	if termIndex == nil || data.indexes == nil || data.indexes[termIndex.Name] == nil {
		return f.loadAllPostings(data, tbl)
	}
	terms := ftsQueryTerms(node)
	if _, isTri := f.tok.(trigramTokenizer); isTri {
		var expanded []ftsTerm
		for _, term := range terms {
			if term.prefix {
				expanded = append(expanded, term)
				continue
			}
			for _, token := range f.tok.Tokenize(term.term) {
				expanded = append(expanded, ftsTerm{term: token.Term, cols: term.cols})
			}
		}
		terms = expanded
	}
	seenRows := make(map[int]struct{})
	for _, term := range terms {
		termValue := f.tok.QueryToken(term.term)
		var positions []int
		if term.prefix {
			start := []byte(runtimeValueKey(termValue))
			end := append(append([]byte(nil), start...), 0xff)
			positions = data.rangeIndexPositions(termIndex.Name, start, end)
		} else {
			positions = data.lookupIndexPositions(termIndex.Name, runtimeIndexLookupKey(*termIndex, termValue))
		}
		for _, pos := range positions {
			if pos < 0 || pos >= len(data.Rows) {
				continue
			}
			if _, seen := seenRows[pos]; seen {
				continue
			}
			seenRows[pos] = struct{}{}
			row := &data.Rows[pos]
			if row.Deleted != 0 {
				continue
			}
			row.ensureValues(tbl)
			indexedTerm, _ := row.Values["term"].(string)
			col, _ := asInt(row.Values["col"])
			docid, _ := asInt(row.Values["docid"])
			poss, _ := row.Values["poss"].(string)
			key := indexedTerm + "\x00" + strconv.Itoa(int(col))
			if out[key] == nil {
				out[key] = map[int64][]int{}
			}
			out[key][docid] = append(out[key][docid], decodePositions(poss)...)
		}
	}
	return out
}

func (f *fts5Instance) loadAllPostings(data *tableData, tbl *catalog.Table) ftsPostings {
	out := ftsPostings{}
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		row.ensureValues(tbl)
		term, _ := row.Values["term"].(string)
		col, _ := asInt(row.Values["col"])
		docid, _ := asInt(row.Values["docid"])
		poss, _ := row.Values["poss"].(string)
		key := term + "\x00" + strconv.Itoa(int(col))
		if out[key] == nil {
			out[key] = map[int64][]int{}
		}
		out[key][docid] = append(out[key][docid], decodePositions(poss)...)
	}
	return out
}

func ftsQueryTerms(node ftsNode) []ftsTerm {
	var out []ftsTerm
	var walk func(ftsNode)
	walk = func(n ftsNode) {
		switch v := n.(type) {
		case ftsAnd:
			walk(v.l)
			walk(v.r)
		case ftsOr:
			walk(v.l)
			walk(v.r)
		case ftsNot:
			walk(v.e)
		case ftsTerm:
			out = append(out, v)
		case ftsPhrase:
			for i, term := range v.terms {
				out = append(out, ftsTerm{term: term, prefix: v.prefix && i == len(v.terms)-1, cols: v.cols})
			}
		case ftsNear:
			for _, term := range v.terms {
				out = append(out, ftsTerm{term: term, cols: v.cols})
			}
		}
	}
	walk(node)
	return out
}

// shadowSchema resolves the catalog entry of a shadow table.
func (f *fts5Instance) shadowSchema(name string) *catalog.Table {
	if f.s == nil {
		return nil
	}
	return f.s.Catalog.Tables[catalog.Normalize(name)]
}

// shadowInsert appends a row to a shadow table.
func (f *fts5Instance) shadowInsert(table string, rowid int64, values []any) error {
	tbl := f.shadowSchema(table)
	if tbl == nil {
		return fmt.Errorf("missing shadow table %s", table)
	}
	data := f.s.Tables[catalog.Normalize(table)]
	if data == nil {
		data = &tableData{}
		f.s.Tables[catalog.Normalize(table)] = data
	}
	if rowid == 0 {
		rowid = tbl.NextRowID
		tbl.NextRowID++
	} else if rowid >= tbl.NextRowID {
		tbl.NextRowID = rowid + 1
	}
	// The first column is the shadow table's rowid, which is supplied
	// separately, so payload values start at the second column.
	row := make(map[string]any, len(tbl.Columns))
	for i, col := range tbl.Columns {
		if col.PrimaryKey || col.Name == "rowid" {
			continue
		}
		vi := i
		if len(tbl.Columns) > 0 && (tbl.Columns[0].PrimaryKey || tbl.Columns[0].Name == "rowid") {
			vi = i - 1
		}
		if vi >= 0 && vi < len(values) {
			row[col.Name] = values[vi]
		} else {
			row[col.Name] = nil
		}
	}
	_ = data.stageRow(tbl, newRowVersion(tbl, rowid, row, f.s.NextTx))
	return nil
}

// shadowDeleteWhere removes a shadow row by rowid.
func (f *fts5Instance) shadowDeleteWhere(table string, rowid int64) error {
	data := f.s.Tables[catalog.Normalize(table)]
	if data == nil {
		return nil
	}
	if data.rowids != nil {
		if pos, ok := data.lookupRowID(rowid); ok && pos >= 0 && pos < len(data.Rows) {
			if data.Rows[pos].Deleted == 0 {
				data.copyRow(pos)
				data.Rows[pos].Deleted = f.s.NextTx
			}
		}
	} else {
		for i := range data.Rows {
			if data.Rows[i].RowID == rowid && data.Rows[i].Deleted == 0 {
				data.copyRow(i)
				data.Rows[i].Deleted = f.s.NextTx
			}
		}
	}
	for i := range data.Pending {
		if data.Pending[i].RowID == rowid && data.Pending[i].Deleted == 0 {
			data.Pending[i].Deleted = f.s.NextTx
		}
	}
	return nil
}

// shadowDeleteTerms removes every posting for a document.
func (f *fts5Instance) shadowDeleteTerms(docid int64) error {
	data := f.s.Tables[catalog.Normalize(f.termsTable())]
	if data == nil {
		return nil
	}
	table := f.shadowSchema(f.termsTable())
	for _, idx := range table.Indexes {
		if len(idx.Columns) != 1 || catalog.Normalize(idx.Columns[0]) != "docid" {
			continue
		}
		if data.indexes != nil && data.indexes[idx.Name] != nil {
			positions := data.lookupIndexPositions(idx.Name, runtimeIndexLookupKey(idx, docid))
			for _, pos := range positions {
				if pos < 0 || pos >= len(data.Rows) || data.Rows[pos].Deleted != 0 {
					continue
				}
				data.copyRow(pos)
				data.Rows[pos].Deleted = f.s.NextTx
			}
			for i := range data.Pending {
				if data.Pending[i].Deleted != 0 {
					continue
				}
				data.Pending[i].ensureValues(table)
				id, _ := asInt(data.Pending[i].Values["docid"])
				if id == docid {
					data.Pending[i].Deleted = f.s.NextTx
				}
			}
			return nil
		}
	}
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		row.ensureValues(f.shadowSchema(f.termsTable()))
		id, _ := asInt(row.Values["docid"])
		if id == docid {
			data.copyRow(i)
			data.Rows[i].Deleted = f.s.NextTx
		}
	}
	return nil
}

// ftsEval evaluates a parsed FTS expression to a set of matching rowids.
type ftsEval struct {
	f        *fts5Instance
	postings ftsPostings
	all      []int64
}

func (e ftsEval) eval(n ftsNode) map[int64]struct{} {
	switch x := n.(type) {
	case ftsAnd:
		l := e.eval(x.l)
		r := e.eval(x.r)
		out := map[int64]struct{}{}
		for id := range l {
			if _, ok := r[id]; ok {
				out[id] = struct{}{}
			}
		}
		return out
	case ftsOr:
		l := e.eval(x.l)
		r := e.eval(x.r)
		out := map[int64]struct{}{}
		for id := range l {
			out[id] = struct{}{}
		}
		for id := range r {
			out[id] = struct{}{}
		}
		return out
	case ftsNot:
		sub := e.eval(x.e)
		out := map[int64]struct{}{}
		for _, id := range e.all {
			if _, ok := sub[id]; !ok {
				out[id] = struct{}{}
			}
		}
		return out
	case ftsTerm:
		return e.matchTerm(x)
	case ftsPhrase:
		return e.matchPhrase(x)
	case ftsNear:
		return e.matchNear(x)
	}
	return map[int64]struct{}{}
}

// termDocs returns the documents and positions matching a term, honouring an
// optional prefix match and column restriction.
func (e ftsEval) termDocs(term string, prefix bool, cols []int) map[int64][]int {
	out := map[int64][]int{}
	for key, docs := range e.postings {
		parts := strings.SplitN(key, "\x00", 2)
		t, colStr := parts[0], parts[1]
		col, _ := strconv.Atoi(colStr)
		if cols != nil && !containsInt(cols, col) {
			continue
		}
		match := false
		if prefix {
			match = strings.HasPrefix(t, term)
		} else {
			match = t == term
		}
		if !match {
			continue
		}
		for docid, poss := range docs {
			out[docid] = append(out[docid], poss...)
		}
	}
	return out
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (e ftsEval) matchTerm(t ftsTerm) map[int64]struct{} {
	// The trigram tokenizer indexes fixed-width character runs, so a query is
	// matched as a substring: every trigram of the query must be present.
	if _, isTri := e.f.tok.(trigramTokenizer); isTri && !t.prefix {
		grams := e.f.tok.Tokenize(t.term)
		if len(grams) == 0 {
			return map[int64]struct{}{}
		}
		var acc map[int64]struct{}
		for i, g := range grams {
			docs := e.termDocs(g.Term, false, t.cols)
			cur := map[int64]struct{}{}
			for id := range docs {
				cur[id] = struct{}{}
			}
			if i == 0 {
				acc = cur
				continue
			}
			next := map[int64]struct{}{}
			for id := range acc {
				if _, ok := cur[id]; ok {
					next[id] = struct{}{}
				}
			}
			acc = next
		}
		if acc == nil {
			return map[int64]struct{}{}
		}
		return acc
	}
	term := e.f.tok.QueryToken(t.term)
	docs := e.termDocs(term, t.prefix, t.cols)
	out := map[int64]struct{}{}
	for id := range docs {
		out[id] = struct{}{}
	}
	return out
}

// matchPhrase requires the terms to appear in consecutive positions in the
// same column.
func (e ftsEval) matchPhrase(p ftsPhrase) map[int64]struct{} {
	if len(p.terms) == 1 {
		return e.matchTerm(ftsTerm{term: p.terms[0], prefix: p.prefix, cols: p.cols})
	}
	normalized := make([]string, len(p.terms))
	for i, t := range p.terms {
		normalized[i] = e.f.tok.QueryToken(t)
	}
	var cursor map[int64][]int
	for i, term := range normalized {
		prefix := p.prefix && i == len(normalized)-1
		docs := e.termDocs(term, prefix, p.cols)
		if i == 0 {
			cursor = docs
			continue
		}
		next := map[int64][]int{}
		for docid, starts := range cursor {
			cands, ok := docs[docid]
			if !ok {
				continue
			}
			for _, s := range starts {
				for _, c := range cands {
					if c == s+i {
						next[docid] = append(next[docid], s)
						break
					}
				}
			}
		}
		cursor = next
	}
	out := map[int64]struct{}{}
	for id := range cursor {
		out[id] = struct{}{}
	}
	return out
}

// matchNear requires the terms to appear within a token distance of each other.
func (e ftsEval) matchNear(n ftsNear) map[int64]struct{} {
	normalized := make([]string, len(n.terms))
	for i, t := range n.terms {
		normalized[i] = e.f.tok.QueryToken(t)
	}
	var cursor map[int64][]int
	for i, term := range normalized {
		docs := e.termDocs(term, false, n.cols)
		if i == 0 {
			cursor = docs
			continue
		}
		next := map[int64][]int{}
		for docid, left := range cursor {
			right, ok := docs[docid]
			if !ok {
				continue
			}
			for _, a := range left {
				for _, b := range right {
					d := a - b
					if d < 0 {
						d = -d
					}
					if d <= n.dist {
						next[docid] = append(next[docid], a, b)
						break
					}
				}
			}
		}
		cursor = next
	}
	out := map[int64]struct{}{}
	for id := range cursor {
		out[id] = struct{}{}
	}
	return out
}

// bm25For scores one document for the terms a query actually references.
func (f *fts5Instance) bm25For(node ftsNode, docid int64, postings ftsPostings, all []int64) float64 {
	terms := collectFTSTerms(node)
	if len(terms) == 0 {
		return 0
	}
	nDocs := float64(len(all))
	if nDocs == 0 {
		return 0
	}
	avgLen := f.averageDocLength()
	const k1 = 1.2
	const b = 0.75
	score := 0.0
	for _, t := range terms {
		term := f.tok.QueryToken(t.term)
		docs := postings[term+"\x00"+strconv.Itoa(0)]
		// Count across all columns when the term is unrestricted.
		n := 0
		freq := 0
		docLen := 0
		for key, m := range postings {
			parts := strings.SplitN(key, "\x00", 2)
			if parts[0] != term {
				continue
			}
			if len(m) > 0 {
				n += len(m)
			}
			if poss, ok := m[docid]; ok {
				freq += len(poss)
			}
		}
		_ = docs
		if freq == 0 {
			continue
		}
		docLen = f.docLength(docid)
		if docLen == 0 {
			docLen = 1
		}
		idf := math.Log(1 + (nDocs-float64(n)+0.5)/(float64(n)+0.5))
		score += idf * (float64(freq) * (k1 + 1)) / (float64(freq) + k1*(1-b+b*float64(docLen)/avgLen))
	}
	return -score
}

// averageDocLength is the mean token count per document across all columns.
func (f *fts5Instance) averageDocLength() float64 {
	data := f.s.Tables[catalog.Normalize(f.docsizeTable())]
	if data == nil || len(data.Rows) == 0 {
		return 1
	}
	tbl := f.shadowSchema(f.docsizeTable())
	total := 0.0
	n := 0
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 {
			continue
		}
		row.ensureValues(tbl)
		for j := range f.vt.Columns {
			v, _ := asInt(row.Values[fmt.Sprintf("l%d", j)])
			total += float64(v)
		}
		n++
	}
	if n == 0 {
		return 1
	}
	return total / float64(n)
}

// docLength is the token count of one document across all columns.
func (f *fts5Instance) docLength(docid int64) int {
	data := f.s.Tables[catalog.Normalize(f.docsizeTable())]
	if data == nil {
		return 0
	}
	tbl := f.shadowSchema(f.docsizeTable())
	for i := range data.Rows {
		row := &data.Rows[i]
		if row.Deleted != 0 || row.RowID != docid {
			continue
		}
		row.ensureValues(tbl)
		total := 0
		for j := range f.vt.Columns {
			v, _ := asInt(row.Values[fmt.Sprintf("l%d", j)])
			total += int(v)
		}
		return total
	}
	return 0
}

// collectFTSTerms gathers every term a query references, for scoring.
func collectFTSTerms(n ftsNode) []ftsTerm {
	var out []ftsTerm
	var walk func(ftsNode)
	walk = func(x ftsNode) {
		switch v := x.(type) {
		case ftsAnd:
			walk(v.l)
			walk(v.r)
		case ftsOr:
			walk(v.l)
			walk(v.r)
		case ftsNot:
		case ftsTerm:
			out = append(out, v)
		case ftsPhrase:
			for i, t := range v.terms {
				out = append(out, ftsTerm{term: t, prefix: v.prefix && i == len(v.terms)-1, cols: v.cols})
			}
		case ftsNear:
			for _, t := range v.terms {
				out = append(out, ftsTerm{term: t, cols: v.cols})
			}
		}
	}
	walk(n)
	return out
}
