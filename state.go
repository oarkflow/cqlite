package db

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/sujit/sqlite/internal/btree"
	"github.com/sujit/sqlite/internal/catalog"
	"github.com/sujit/sqlite/internal/pager"
)

type rowVersion struct {
	RowID   int64          `json:"row_id"`
	Values  map[string]any `json:"values"`
	Pos     []any          `json:"pos,omitempty"`
	Created uint64         `json:"created"`
	Deleted uint64         `json:"deleted,omitempty"`
}

type tableData struct {
	Rows         []rowVersion           `json:"rows"`
	Pending      []rowVersion           `json:"-"`
	pendingRowID map[int64]struct{}     `json:"-"`
	pendingLast  int64                  `json:"-"`
	pendingAsc   bool                   `json:"-"`
	rowids       *btree.Tree            `json:"-"`
	indexes      map[string]*btree.Tree `json:"-"`
	baseRows     int                    `json:"-"`
	deferRuntime bool                   `json:"-"`
	rowsShared   bool                   `json:"-"`
	lazyRecords  []pager.Record         `json:"-"`
	lazyPath     string                 `json:"-"`
	lazyPageSize int                    `json:"-"`
	lazyCount    int                    `json:"-"`
}

type persistedState struct {
	Catalog catalog.Catalog       `json:"catalog"`
	Tables  map[string]*tableData `json:"tables"`
	NextTx  uint64                `json:"next_tx"`
	Pragmas pragmaState           `json:"pragmas"`
}

type checkpointMeta struct {
	Catalog    catalog.Catalog `json:"catalog"`
	NextTx     uint64          `json:"next_tx"`
	Pragmas    pragmaState     `json:"pragmas"`
	Applied    uint64          `json:"applied"`
	Generation uint64          `json:"generation,omitempty"`
}

type pragmaState struct {
	ForeignKeys bool   `json:"foreign_keys"`
	JournalMode string `json:"journal_mode"`
	Synchronous string `json:"synchronous"`
	UserVersion int64  `json:"user_version"`
	SchemaVer   int64  `json:"schema_version"`
	PageSize    int    `json:"page_size"`
	CacheSize   int    `json:"cache_size"`
}

func newState() persistedState {
	return persistedState{
		Catalog: catalog.New(),
		Tables:  make(map[string]*tableData),
		NextTx:  1,
		Pragmas: pragmaState{JournalMode: "wal", Synchronous: "full", PageSize: 4096, CacheSize: 128},
	}
}

func (s persistedState) clone() persistedState {
	out := persistedState{
		Catalog: catalog.New(),
		Tables:  make(map[string]*tableData, len(s.Tables)),
		NextTx:  s.NextTx,
		Pragmas: s.Pragmas,
	}
	for name, tbl := range s.Catalog.Tables {
		cp := *tbl
		cp.Columns = append([]catalog.Column(nil), tbl.Columns...)
		cp.Indexes = append([]catalog.Index(nil), tbl.Indexes...)
		out.Catalog.Tables[name] = &cp
	}
	for name, view := range s.Catalog.Views {
		cp := *view
		cp.Columns = append([]catalog.Column(nil), view.Columns...)
		out.Catalog.Views[name] = &cp
	}
	for name, data := range s.Tables {
		td := &tableData{Rows: data.Rows[:len(data.Rows):len(data.Rows)]}
		if len(data.Pending) > 0 {
			td.Pending = append([]rowVersion(nil), data.Pending...)
		}
		if len(data.lazyRecords) > 0 {
			td.lazyRecords = append([]pager.Record(nil), data.lazyRecords...)
		}
		td.lazyPath = data.lazyPath
		td.lazyPageSize = data.lazyPageSize
		td.lazyCount = data.lazyCount
		if len(data.pendingRowID) > 0 {
			td.pendingRowID = make(map[int64]struct{}, len(data.pendingRowID))
			for rowid := range data.pendingRowID {
				td.pendingRowID[rowid] = struct{}{}
			}
		}
		td.rowids = data.rowids
		td.indexes = data.indexes
		td.baseRows = len(data.Rows)
		td.deferRuntime = true
		td.rowsShared = true
		td.pendingLast = data.pendingLast
		td.pendingAsc = data.pendingAsc
		data.rowsShared = true
		out.Tables[name] = td
	}
	return out
}

func (t *tableData) copyRow(pos int) {
	if t == nil || pos < 0 || pos >= len(t.Rows) {
		return
	}
	if t.rowsShared {
		rows := make([]rowVersion, len(t.Rows))
		copy(rows, t.Rows)
		t.Rows = rows
		t.rowsShared = false
	}
	t.Rows[pos].Values = cloneValues(t.Rows[pos].Values)
	t.Rows[pos].Pos = clonePositionedValues(t.Rows[pos].Pos)
}

func (t *tableData) copyRowForColumnUpdate(pos int, colIdx int, colName string, value any) {
	if t == nil || pos < 0 || pos >= len(t.Rows) {
		return
	}
	cloneRowValues := t.rowsShared
	if t.rowsShared {
		rows := make([]rowVersion, len(t.Rows))
		copy(rows, t.Rows)
		t.Rows = rows
		t.rowsShared = false
	}
	row := &t.Rows[pos]
	if cloneRowValues {
		row.Pos = clonePositionedValues(row.Pos)
	}
	if colIdx >= 0 && colIdx < len(row.Pos) {
		row.Pos[colIdx] = value
	}
	if row.Values != nil {
		if cloneRowValues {
			row.Values = cloneValues(row.Values)
		}
		row.Values[colName] = value
	}
}

func (t *tableData) markRowDeleted(pos int, txid uint64) {
	if t == nil || pos < 0 || pos >= len(t.Rows) {
		return
	}
	if t.rowsShared {
		rows := make([]rowVersion, len(t.Rows))
		copy(rows, t.Rows)
		t.Rows = rows
		t.rowsShared = false
	}
	t.Rows[pos].Deleted = txid
}

func (t *tableData) runtimeDeferred() bool {
	return t != nil && (t.deferRuntime || len(t.lazyRecords) > 0 || t.lazyPath != "")
}

func cloneValues(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func clonePositionedValues(in []any) []any {
	if in == nil {
		return nil
	}
	return append([]any(nil), in...)
}

func newRowVersion(schema *catalog.Table, rowid int64, values map[string]any, created uint64) rowVersion {
	return rowVersion{
		RowID:   rowid,
		Values:  values,
		Pos:     valuesToPositioned(schema, values),
		Created: created,
	}
}

func newPositionedRowVersion(rowid int64, values []any, created uint64) rowVersion {
	return rowVersion{
		RowID:   rowid,
		Pos:     values,
		Created: created,
	}
}

func valuesToPositioned(schema *catalog.Table, values map[string]any) []any {
	if schema == nil {
		return nil
	}
	pos := make([]any, len(schema.Columns))
	for i, col := range schema.Columns {
		pos[i] = values[col.Name]
	}
	return pos
}

func positionedToValues(schema *catalog.Table, pos []any) map[string]any {
	values := make(map[string]any, len(schema.Columns))
	for i, col := range schema.Columns {
		if i < len(pos) {
			values[col.Name] = pos[i]
		} else {
			values[col.Name] = nil
		}
	}
	return values
}

func (r *rowVersion) ensurePositioned(schema *catalog.Table) {
	if schema == nil {
		return
	}
	if len(r.Pos) == len(schema.Columns) {
		return
	}
	r.Pos = valuesToPositioned(schema, r.Values)
}

func (r *rowVersion) ensureValues(schema *catalog.Table) {
	if schema == nil {
		return
	}
	if r.Values != nil && len(r.Values) >= len(schema.Columns) {
		return
	}
	r.Values = positionedToValues(schema, r.positionedValues())
}

func (r rowVersion) value(schema *catalog.Table, col string) any {
	if col == "rowid" || col == "_rowid_" || col == "oid" {
		return r.RowID
	}
	if schema != nil {
		if _, idx, ok := schema.Column(col); ok && idx >= 0 && idx < len(r.Pos) {
			return r.Pos[idx]
		}
	}
	if r.Values == nil {
		return nil
	}
	return r.Values[col]
}

func (r rowVersion) positionalAt(idx int) (any, bool) {
	if idx >= 0 && idx < len(r.Pos) {
		return r.Pos[idx], true
	}
	return nil, false
}

func (r *rowVersion) syncPositioned(schema *catalog.Table) {
	if schema == nil {
		return
	}
	r.Pos = valuesToPositioned(schema, r.Values)
}

func (r *rowVersion) positionedValues() []any {
	return r.Pos
}

func (s *persistedState) rebuildRuntime() {
	s.materializePending()
	for name, data := range s.Tables {
		if tbl := s.Catalog.Tables[name]; tbl != nil {
			data.materializeLazy(tbl)
			data.rebuildRuntime(tbl)
		}
	}
}

func (s *persistedState) deferRuntime() {
	for _, data := range s.Tables {
		if data == nil {
			continue
		}
		data.rowids = nil
		data.indexes = nil
		data.baseRows = len(data.Rows)
		data.deferRuntime = true
		data.rowsShared = false
	}
}

func (s *persistedState) hasDeferredRuntime() bool {
	for _, data := range s.Tables {
		if data != nil && data.deferRuntime {
			return true
		}
	}
	return false
}

func (s *persistedState) ensureRuntime() {
	if s.hasDeferredRuntime() {
		s.rebuildRuntime()
	}
}

func (s *persistedState) materializePending() {
	for name, data := range s.Tables {
		if tbl := s.Catalog.Tables[name]; tbl != nil {
			data.materializePending(tbl)
		}
	}
}

func (t *tableData) stageRow(schema *catalog.Table, row rowVersion) int {
	if t.deferRuntime {
		t.Pending = append(t.Pending, row)
		t.trackPendingRowID(row.RowID)
		return len(t.Rows) + len(t.Pending) - 1
	}
	t.Rows = append(t.Rows, row)
	pos := len(t.Rows) - 1
	t.addRuntime(schema, pos)
	return pos
}

func (t *tableData) materializePending(schema *catalog.Table) {
	if t == nil || len(t.Pending) == 0 {
		return
	}
	if t.rowsShared {
		rows := make([]rowVersion, len(t.Rows), len(t.Rows)+len(t.Pending))
		copy(rows, t.Rows)
		t.Rows = rows
		t.rowsShared = false
	}
	start := len(t.Rows)
	t.Rows = append(t.Rows, t.Pending...)
	t.Pending = nil
	t.pendingRowID = nil
	t.pendingLast = 0
	t.pendingAsc = false
	for i := start; i < len(t.Rows); i++ {
		t.addRuntime(schema, i)
	}
}

func (t *tableData) trackPendingRowID(rowid int64) {
	if len(t.Pending) == 1 {
		t.pendingLast = rowid
		t.pendingAsc = true
		return
	}
	if t.pendingAsc && rowid > t.pendingLast {
		t.pendingLast = rowid
		return
	}
	t.ensurePendingRowIDMap()
	t.pendingRowID[rowid] = struct{}{}
	t.pendingAsc = false
}

func (t *tableData) ensurePendingRowIDMap() {
	if t.pendingRowID != nil {
		return
	}
	t.pendingRowID = make(map[int64]struct{}, len(t.Pending))
	for _, row := range t.Pending {
		t.pendingRowID[row.RowID] = struct{}{}
	}
}

func (t *tableData) pendingRowIDConflict(rowid int64) bool {
	if len(t.Pending) == 0 {
		return false
	}
	if t.pendingAsc && rowid > t.pendingLast {
		return false
	}
	if t.pendingRowID != nil {
		_, ok := t.pendingRowID[rowid]
		return ok
	}
	for _, row := range t.Pending {
		if row.Deleted == 0 && row.RowID == rowid {
			return true
		}
	}
	return false
}

func (t *tableData) rebuildRuntime(schema *catalog.Table) {
	t.materializeLazy(schema)
	t.rowids = btree.New()
	t.indexes = make(map[string]*btree.Tree, len(schema.Indexes))
	t.baseRows = len(t.Rows)
	t.deferRuntime = false
	t.rowsShared = false
	for _, idx := range schema.Indexes {
		if len(idx.Columns) > 0 {
			t.indexes[idx.Name] = btree.New()
		}
	}
	for i, row := range t.Rows {
		t.Rows[i].ensureValues(schema)
		t.Rows[i].ensurePositioned(schema)
		row = t.Rows[i]
		t.addRowIDPosition(row.RowID, i)
		if row.Deleted != 0 {
			continue
		}
		for _, idx := range schema.Indexes {
			if len(idx.Columns) == 0 {
				continue
			}
			t.addIndexPosition(idx, i)
		}
	}
}

func (t *tableData) materializeLazy(schema *catalog.Table) error {
	if t == nil || (len(t.lazyRecords) == 0 && t.lazyPath == "") {
		return nil
	}
	records := t.lazyRecords
	if len(records) == 0 {
		pg, err := pager.Open(t.lazyPath, t.lazyPageSize)
		if err != nil {
			return err
		}
		records, err = pg.ReadRecords()
		closeErr := pg.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	rows := make([]rowVersion, 0, len(records))
	for _, rec := range records {
		row, err := decodeRowVersionLazy(schema, rec.Values)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	t.Rows = rows
	t.lazyRecords = nil
	t.lazyPath = ""
	t.lazyPageSize = 0
	t.lazyCount = 0
	return nil
}

func (t *tableData) lazyRowByID(schema *catalog.Table, rowid int64) (rowVersion, bool, error) {
	if t == nil || (len(t.lazyRecords) == 0 && t.lazyPath == "") {
		return rowVersion{}, false, nil
	}
	for _, rec := range t.lazyRecords {
		if rec.Key != rowid {
			continue
		}
		row, err := decodeRowVersionLazy(schema, rec.Values)
		if err != nil {
			return rowVersion{}, false, err
		}
		return row, true, nil
	}
	if t.lazyPath != "" && rowid > 0 && rowid <= int64(t.lazyCount) {
		pg, err := pager.Open(t.lazyPath, t.lazyPageSize)
		if err != nil {
			return rowVersion{}, false, err
		}
		rec, ok, err := pg.ReadRecordAtOrdinal(int(rowid - 1))
		closeErr := pg.Close()
		if err != nil {
			return rowVersion{}, false, err
		}
		if closeErr != nil {
			return rowVersion{}, false, closeErr
		}
		if ok && rec.Key == rowid {
			row, err := decodeRowVersionLazy(schema, rec.Values)
			if err != nil {
				return rowVersion{}, false, err
			}
			return row, true, nil
		}
	}
	return rowVersion{}, false, nil
}

func (t *tableData) addRuntime(schema *catalog.Table, pos int) {
	if t.deferRuntime {
		return
	}
	if t.rowids == nil || t.indexes == nil {
		t.rebuildRuntime(schema)
		return
	}
	if pos < 0 || pos >= len(t.Rows) {
		return
	}
	row := t.Rows[pos]
	t.addRowIDPosition(row.RowID, pos)
	if row.Deleted != 0 {
		return
	}
	for _, idx := range schema.Indexes {
		if len(idx.Columns) == 0 {
			continue
		}
		t.addIndexPosition(idx, pos)
	}
}

func (t *tableData) addRowIDPosition(rowid int64, pos int) {
	if t.rowids == nil {
		t.rowids = btree.New()
	}
	t.rowids.Put(encodeInt64Key(rowid), encodeIntPosition(pos))
}

func (t *tableData) lookupRowID(rowid int64) (int, bool) {
	if t == nil || t.rowids == nil {
		return 0, false
	}
	var (
		pos int
		ok  bool
	)
	if !t.rowids.View(encodeInt64Key(rowid), func(raw []byte) {
		pos, ok = decodeIntPosition(raw)
	}) {
		return 0, false
	}
	return pos, ok
}

func (t *tableData) rangeRowIDs(start *int64, end *int64) []int {
	if t == nil || t.rowids == nil {
		return nil
	}
	var startKey, endKey []byte
	if start != nil {
		startKey = encodeInt64Key(*start)
	}
	if end != nil {
		endKey = encodeInt64Key(*end)
	}
	var out []int
	t.rowids.ViewScan(startKey, endKey, func(_, raw []byte) bool {
		if pos, ok := decodeIntPosition(raw); ok {
			out = append(out, pos)
		}
		return true
	})
	return out
}

func (t *tableData) eachRangeRowID(start *int64, end *int64, fn func(int) bool) bool {
	if t == nil || t.rowids == nil {
		return false
	}
	var startKey, endKey []byte
	if start != nil {
		startKey = encodeInt64Key(*start)
	}
	if end != nil {
		endKey = encodeInt64Key(*end)
	}
	t.rowids.ViewScan(startKey, endKey, func(_, raw []byte) bool {
		pos, ok := decodeIntPosition(raw)
		if !ok {
			return true
		}
		return fn(pos)
	})
	return true
}

func (t *tableData) addIndexPosition(idx catalog.Index, pos int) {
	if pos < 0 || pos >= len(t.Rows) {
		return
	}
	tr := t.indexes[idx.Name]
	if tr == nil {
		tr = btree.New()
		t.indexes[idx.Name] = tr
	}
	key := []byte(indexRuntimeKey(idx, t.Rows[pos].Values))
	positions, _ := tr.Get(key)
	next := decodeIndexPositions(positions)
	next = append(next, pos)
	tr.Put(key, encodeIndexPositions(next))
}

func (t *tableData) lookupIndexPositions(indexName, key string) []int {
	if t == nil || t.indexes == nil || t.indexes[indexName] == nil {
		return nil
	}
	var out []int
	if !t.indexes[indexName].View([]byte(key), func(raw []byte) {
		out = decodeIndexPositions(raw)
	}) {
		return nil
	}
	return out
}

func (t *tableData) eachIndexPosition(indexName, key string, fn func(int) bool) {
	t.eachIndexPositionBytes(indexName, []byte(key), fn)
}

func (t *tableData) eachIndexPositionBytes(indexName string, key []byte, fn func(int) bool) {
	if t == nil || t.indexes == nil || t.indexes[indexName] == nil {
		return
	}
	t.indexes[indexName].View(key, func(raw []byte) {
		eachEncodedIndexPosition(raw, fn)
	})
}

func (t *tableData) rangeIndexPositions(indexName string, start, end []byte) []int {
	if t == nil || t.indexes == nil || t.indexes[indexName] == nil {
		return nil
	}
	var out []int
	t.indexes[indexName].ViewScan(start, end, func(_, raw []byte) bool {
		eachEncodedIndexPosition(raw, func(pos int) bool {
			out = append(out, pos)
			return true
		})
		return true
	})
	return out
}

func (t *tableData) eachRangeIndexPosition(indexName string, start, end []byte, fn func(int) bool) bool {
	if t == nil || t.indexes == nil || t.indexes[indexName] == nil {
		return false
	}
	t.indexes[indexName].ViewScan(start, end, func(_, raw []byte) bool {
		keepGoing := true
		eachEncodedIndexPosition(raw, func(pos int) bool {
			keepGoing = fn(pos)
			return keepGoing
		})
		return keepGoing
	})
	return true
}

func indexRuntimeKey(idx catalog.Index, values map[string]any) string {
	if len(idx.Columns) == 1 {
		return runtimeValueKey(values[idx.Columns[0]])
	}
	key := make([]any, 0, len(idx.Columns))
	for _, col := range idx.Columns {
		key = append(key, values[col])
	}
	return fmt.Sprintf("%#v", key)
}

func runtimeValueKey(v any) string {
	switch x := v.(type) {
	case nil:
		return "n:"
	case int:
		return "i:" + string(encodeInt64Key(int64(x)))
	case int64:
		return "i:" + string(encodeInt64Key(x))
	case float64:
		return "f:" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "s:" + x
	case bool:
		if x {
			return "b:1"
		}
		return "b:0"
	case []byte:
		return "x:" + string(x)
	default:
		return fmt.Sprintf("%#v", v)
	}
}

func encodeIndexPositions(positions []int) []byte {
	out := make([]byte, 4+len(positions)*8)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(positions)))
	for i, pos := range positions {
		binary.LittleEndian.PutUint64(out[4+i*8:12+i*8], uint64(pos))
	}
	return out
}

func encodeInt64Key(v int64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], uint64(v)^(uint64(1)<<63))
	return out[:]
}

func encodeIntPosition(pos int) []byte {
	var out [8]byte
	binary.LittleEndian.PutUint64(out[:], uint64(pos))
	return out[:]
}

func decodeIntPosition(data []byte) (int, bool) {
	if len(data) != 8 {
		return 0, false
	}
	return int(binary.LittleEndian.Uint64(data)), true
}

func decodeIndexPositions(data []byte) []int {
	if len(data) < 4 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(data[0:4]))
	if n < 0 || len(data) < 4+n*8 {
		return nil
	}
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = int(binary.LittleEndian.Uint64(data[4+i*8 : 12+i*8]))
	}
	return out
}

func eachEncodedIndexPosition(data []byte, fn func(int) bool) {
	if len(data) < 4 {
		return
	}
	n := int(binary.LittleEndian.Uint32(data[0:4]))
	if n < 0 || len(data) < 4+n*8 {
		return
	}
	for i := 0; i < n; i++ {
		if !fn(int(binary.LittleEndian.Uint64(data[4+i*8 : 12+i*8]))) {
			return
		}
	}
}
