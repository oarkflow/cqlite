package db

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/sujit/sqlite/internal/btree"
	"github.com/sujit/sqlite/internal/catalog"
)

type rowVersion struct {
	RowID   int64          `json:"row_id"`
	Values  map[string]any `json:"values"`
	Created uint64         `json:"created"`
	Deleted uint64         `json:"deleted,omitempty"`
}

type tableData struct {
	Rows    []rowVersion           `json:"rows"`
	rowids  *btree.Tree            `json:"-"`
	indexes map[string]*btree.Tree `json:"-"`
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
	for name, data := range s.Tables {
		td := &tableData{Rows: make([]rowVersion, len(data.Rows))}
		for i, row := range data.Rows {
			td.Rows[i] = rowVersion{
				RowID:   row.RowID,
				Values:  cloneValues(row.Values),
				Created: row.Created,
				Deleted: row.Deleted,
			}
		}
		if tbl := out.Catalog.Tables[name]; tbl != nil {
			td.rebuildRuntime(tbl)
		}
		out.Tables[name] = td
	}
	return out
}

func cloneValues(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *persistedState) rebuildRuntime() {
	for name, data := range s.Tables {
		if tbl := s.Catalog.Tables[name]; tbl != nil {
			data.rebuildRuntime(tbl)
		}
	}
}

func (t *tableData) rebuildRuntime(schema *catalog.Table) {
	t.rowids = btree.New()
	t.indexes = make(map[string]*btree.Tree, len(schema.Indexes))
	for _, idx := range schema.Indexes {
		if len(idx.Columns) > 0 {
			t.indexes[idx.Name] = btree.New()
		}
	}
	for i, row := range t.Rows {
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

func (t *tableData) addRuntime(schema *catalog.Table, pos int) {
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
	raw, ok := t.rowids.Get(encodeInt64Key(rowid))
	if !ok {
		return 0, false
	}
	return decodeIntPosition(raw)
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
	t.rowids.Scan(startKey, endKey, func(_, raw []byte) bool {
		if pos, ok := decodeIntPosition(raw); ok {
			out = append(out, pos)
		}
		return true
	})
	return out
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
	raw, ok := t.indexes[indexName].Get([]byte(key))
	if !ok {
		return nil
	}
	return decodeIndexPositions(raw)
}

func (t *tableData) eachIndexPosition(indexName, key string, fn func(int) bool) {
	if t == nil || t.indexes == nil || t.indexes[indexName] == nil {
		return
	}
	t.indexes[indexName].View([]byte(key), func(raw []byte) {
		eachEncodedIndexPosition(raw, fn)
	})
}

func (t *tableData) rangeIndexPositions(indexName string, start, end []byte) []int {
	if t == nil || t.indexes == nil || t.indexes[indexName] == nil {
		return nil
	}
	var out []int
	t.indexes[indexName].Scan(start, end, func(_, raw []byte) bool {
		eachEncodedIndexPosition(raw, func(pos int) bool {
			out = append(out, pos)
			return true
		})
		return true
	})
	return out
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
