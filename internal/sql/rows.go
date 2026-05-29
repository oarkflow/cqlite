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
