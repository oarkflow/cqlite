package db

import (
	"fmt"
	"strconv"

	"github.com/sujit/sqlite/internal/catalog"
)

type rowVersion struct {
	RowID   int64          `json:"row_id"`
	Values  map[string]any `json:"values"`
	Created uint64         `json:"created"`
	Deleted uint64         `json:"deleted,omitempty"`
}

type tableData struct {
	Rows    []rowVersion                `json:"rows"`
	rowids  map[int64]int               `json:"-"`
	indexes map[string]map[string][]int `json:"-"`
}

type persistedState struct {
	Catalog catalog.Catalog       `json:"catalog"`
	Tables  map[string]*tableData `json:"tables"`
	NextTx  uint64                `json:"next_tx"`
	Pragmas pragmaState           `json:"pragmas"`
}

type checkpointMeta struct {
	Catalog catalog.Catalog `json:"catalog"`
	NextTx  uint64          `json:"next_tx"`
	Pragmas pragmaState     `json:"pragmas"`
	Applied uint64          `json:"applied"`
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
	t.rowids = make(map[int64]int, len(t.Rows))
	t.indexes = make(map[string]map[string][]int, len(schema.Indexes))
	for _, idx := range schema.Indexes {
		if len(idx.Columns) > 0 {
			t.indexes[idx.Name] = make(map[string][]int)
		}
	}
	for i, row := range t.Rows {
		t.rowids[row.RowID] = i
		if row.Deleted != 0 {
			continue
		}
		for _, idx := range schema.Indexes {
			if len(idx.Columns) == 0 {
				continue
			}
			key := indexRuntimeKey(idx, row.Values)
			t.indexes[idx.Name][key] = append(t.indexes[idx.Name][key], i)
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
	t.rowids[row.RowID] = pos
	if row.Deleted != 0 {
		return
	}
	for _, idx := range schema.Indexes {
		if len(idx.Columns) == 0 {
			continue
		}
		if t.indexes[idx.Name] == nil {
			t.indexes[idx.Name] = make(map[string][]int)
		}
		key := indexRuntimeKey(idx, row.Values)
		t.indexes[idx.Name][key] = append(t.indexes[idx.Name][key], pos)
	}
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
		return "i:" + strconv.FormatInt(int64(x), 10)
	case int64:
		return "i:" + strconv.FormatInt(x, 10)
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
