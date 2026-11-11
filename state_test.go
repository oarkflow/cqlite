package cqlite

import (
	"testing"

	"github.com/oarkflow/cqlite/internal/catalog"
)

func TestPositionArenaAllocationsAreDisjointAndZeroed(t *testing.T) {
	var arena positionArena
	first := arena.alloc(3)
	first[0], first[1], first[2] = int64(1), "first", int64(3)
	second := arena.alloc(3)
	if cap(first) != len(first) || cap(second) != len(second) {
		t.Fatalf("arena slices must be capacity-limited: first cap=%d len=%d, second cap=%d len=%d", cap(first), len(first), cap(second), len(second))
	}
	if second[0] != nil || second[1] != nil || second[2] != nil {
		t.Fatalf("new arena slice was not zeroed: %#v", second)
	}
	second[0], second[1], second[2] = int64(2), "second", int64(4)
	if first[0] != int64(1) || first[1] != "first" || first[2] != int64(3) {
		t.Fatalf("allocating a later row changed the first row: %#v", first)
	}
}

func TestRowVersionMaintainsSchemaPositionedValues(t *testing.T) {
	schema := &catalog.Table{
		Name: "items",
		Columns: []catalog.Column{
			{Name: "id", Type: catalog.Integer},
			{Name: "name", Type: catalog.Text},
			{Name: "score", Type: catalog.Integer},
		},
	}
	row := newRowVersion(schema, 1, map[string]any{"id": int64(1), "name": "Ada", "score": int64(7)}, 2)
	pos := row.positionedValues()
	if len(pos) != 3 || pos[0] != int64(1) || pos[1] != "Ada" || pos[2] != int64(7) {
		t.Fatalf("positioned values = %#v", pos)
	}
	row.Values["score"] = int64(8)
	row.syncPositioned(schema)
	pos = row.positionedValues()
	if pos[2] != int64(8) {
		t.Fatalf("synced positioned score = %#v", pos)
	}
	row.Values = nil
	row.ensureValues(schema)
	if row.Values["name"] != "Ada" || row.Values["score"] != int64(8) {
		t.Fatalf("rebuilt values = %#v", row.Values)
	}
}

func TestTableDataPrimaryAndSecondaryRuntimeBTrees(t *testing.T) {
	schema := &catalog.Table{
		Name: "items",
		Columns: []catalog.Column{
			{Name: "id", Type: catalog.Integer, PrimaryKey: true},
			{Name: "name", Type: catalog.Text},
		},
		Indexes: []catalog.Index{{Name: "items_name_idx", Columns: []string{"name"}}},
	}
	data := &tableData{Rows: []rowVersion{
		newRowVersion(schema, 10, map[string]any{"id": int64(10), "name": "Ada"}, 1),
		newRowVersion(schema, 20, map[string]any{"id": int64(20), "name": "Grace"}, 1),
		newRowVersion(schema, 30, map[string]any{"id": int64(30), "name": "Ada"}, 1),
	}}
	data.rebuildRuntime(schema)
	if pos, ok := data.lookupRowID(20); !ok || pos != 1 {
		t.Fatalf("lookup rowid = %d %v, want 1 true", pos, ok)
	}
	positions := data.rangeRowIDs(ptrInt64(11), ptrInt64(31))
	if len(positions) != 2 || positions[0] != 1 || positions[1] != 2 {
		t.Fatalf("range positions = %#v", positions)
	}
	idx := catalog.Index{Name: "items_name_idx", Columns: []string{"name"}}
	positions = data.lookupIndexPositions("items_name_idx", runtimeIndexLookupKey(idx, "Ada"))
	if len(positions) != 2 || positions[0] != 0 || positions[1] != 2 {
		t.Fatalf("index positions = %#v", positions)
	}
}

func ptrInt64(v int64) *int64 { return &v }
