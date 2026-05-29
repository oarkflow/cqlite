package sql

type Result struct {
	RowsAffected int64
	LastInsertID int64
}

type Rows struct {
	Columns []string
	Values  [][]any
	Flat    []any
	Width   int
	pos     int
}

type ColumnType struct {
	Name             string
	DatabaseTypeName string
	Nullable         bool
}

func NewRows(cols []string, vals [][]any) *Rows {
	return &Rows{Columns: cols, Values: vals}
}

func NewFlatRows(cols []string, vals []any) *Rows {
	width := len(cols)
	return &Rows{Columns: cols, Flat: vals, Width: width}
}

func (r *Rows) Next() ([]any, bool) {
	if r == nil {
		return nil, false
	}
	if r.Width > 0 {
		start := r.pos * r.Width
		if start >= len(r.Flat) {
			return nil, false
		}
		r.pos++
		return r.Flat[start : start+r.Width], true
	}
	if r.pos >= len(r.Values) {
		return nil, false
	}
	row := r.Values[r.pos]
	r.pos++
	return row, true
}

func (r *Rows) ColumnTypes() []ColumnType {
	if r == nil {
		return nil
	}
	out := make([]ColumnType, len(r.Columns))
	for i, name := range r.Columns {
		out[i] = ColumnType{Name: name, DatabaseTypeName: "ANY", Nullable: true}
	}
	row, ok := r.firstRow()
	if !ok {
		return out
	}
	for i := range out {
		if i < len(row) {
			out[i].DatabaseTypeName = valueTypeName(row[i])
			out[i].Nullable = row[i] == nil
		}
	}
	return out
}

func (r *Rows) firstRow() ([]any, bool) {
	if r.Width > 0 {
		if len(r.Flat) < r.Width {
			return nil, false
		}
		return r.Flat[:r.Width], true
	}
	if len(r.Values) == 0 {
		return nil, false
	}
	return r.Values[0], true
}

func valueTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "ANY"
	case int, int64:
		return "INTEGER"
	case float32, float64:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	case bool:
		return "INTEGER"
	default:
		return "ANY"
	}
}
