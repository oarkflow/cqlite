package db

import (
	"bytes"
	"testing"

	"github.com/sujit/sqlite/internal/catalog"
)

func TestRowCodecRoundTrip(t *testing.T) {
	schema := rowCodecTestSchema()
	row := rowVersion{
		RowID:   7,
		Created: 11,
		Deleted: 13,
		Values: map[string]any{
			"id":      int64(7),
			"score":   3.5,
			"name":    "Ada",
			"payload": []byte("bytes"),
			"empty":   nil,
		},
	}
	data, err := encodeRowVersion(schema, row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRowVersion(schema, data)
	if err != nil {
		t.Fatal(err)
	}
	if got.RowID != row.RowID || got.Created != row.Created || got.Deleted != row.Deleted {
		t.Fatalf("metadata = %+v, want %+v", got, row)
	}
	if got.Values["id"] != int64(7) || got.Values["score"] != 3.5 || got.Values["name"] != "Ada" || got.Values["empty"] != nil {
		t.Fatalf("values = %#v", got.Values)
	}
	if len(got.Pos) != len(schema.Columns) || got.Pos[0] != int64(7) || got.Pos[1] != 3.5 || got.Pos[2] != "Ada" {
		t.Fatalf("positioned values = %#v", got.Pos)
	}
	if !bytes.Equal(got.Values["payload"].([]byte), []byte("bytes")) {
		t.Fatalf("payload = %#v", got.Values["payload"])
	}
}

func TestRowCodecPrefersSchemaPositionedValues(t *testing.T) {
	schema := rowCodecTestSchema()
	row := rowVersion{
		RowID:  9,
		Values: map[string]any{"id": int64(1), "score": 1.5, "name": "map"},
		Pos:    []any{int64(9), 4.5, "pos", []byte("payload"), nil},
	}
	data, err := encodeRowVersion(schema, row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRowVersion(schema, data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Values["id"] != int64(9) || got.Values["score"] != 4.5 || got.Values["name"] != "pos" {
		t.Fatalf("decoded values did not come from positioned storage: %#v", got.Values)
	}
}

func TestRowCodecRejectsCorruptInput(t *testing.T) {
	schema := rowCodecTestSchema()
	row := rowVersion{RowID: 1, Values: map[string]any{"id": int64(1)}}
	data, err := encodeRowVersion(schema, row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRowVersion(schema, data[:len(data)-1]); err == nil {
		t.Fatal("expected truncated row to fail")
	}
	data = append(data, 0)
	if _, err := decodeRowVersion(schema, data); err == nil {
		t.Fatal("expected trailing row bytes to fail")
	}
}

func FuzzRowCodecDecode(f *testing.F) {
	schema := rowCodecTestSchema()
	row := rowVersion{
		RowID:   1,
		Created: 2,
		Values:  map[string]any{"id": int64(1), "score": 2.5, "name": "seed", "payload": []byte("x")},
	}
	data, err := encodeRowVersion(schema, row)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte{})
	f.Add([]byte("not-a-row"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			data = data[:8192]
		}
		_, _ = decodeRowVersion(schema, data)
	})
}

func rowCodecTestSchema() *catalog.Table {
	return &catalog.Table{
		Name: "rows",
		Columns: []catalog.Column{
			{Name: "id", Type: catalog.Integer},
			{Name: "score", Type: catalog.Real},
			{Name: "name", Type: catalog.Text},
			{Name: "payload", Type: catalog.Blob},
			{Name: "empty", Type: catalog.Any},
		},
	}
}
