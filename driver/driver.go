package driver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"

	db "github.com/sujit/sqlite"
)

const Name = "sujitsqlite"

func init() {
	sql.Register(Name, &Driver{})
}

type Driver struct{}

func (d *Driver) Open(name string) (driver.Conn, error) {
	inner, err := db.Open(name, db.Options{})
	if err != nil {
		return nil, err
	}
	return &Conn{db: inner}, nil
}

type Conn struct {
	db *db.DB
	tx *db.Tx
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return &Stmt{conn: c, query: query}, nil
}

func (c *Conn) Close() error { return c.db.Close() }

func (c *Conn) Begin() (driver.Tx, error) {
	tx, err := c.db.Begin(context.Background(), db.TxOptions{})
	if err != nil {
		return nil, err
	}
	c.tx = tx
	return &Tx{conn: c, tx: tx}, nil
}

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.db.Begin(ctx, db.TxOptions{})
	if err != nil {
		return nil, err
	}
	c.tx = tx
	return &Tx{conn: c, tx: tx}, nil
}

func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals := namedValues(args)
	var res db.Result
	var err error
	if c.tx != nil {
		res, err = c.tx.Exec(ctx, query, vals...)
	} else {
		res, err = c.db.Exec(ctx, query, vals...)
	}
	if err != nil {
		return nil, err
	}
	return Result{rowsAffected: res.RowsAffected, lastInsertID: res.LastInsertID}, nil
}

func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals := namedValues(args)
	var rows *db.Rows
	var err error
	if c.tx != nil {
		rows, err = c.tx.Query(ctx, query, vals...)
	} else {
		rows, err = c.db.Query(ctx, query, vals...)
	}
	if err != nil {
		return nil, err
	}
	return &rowsAdapter{Rows: rows}, nil
}

func (c *Conn) CheckNamedValue(v *driver.NamedValue) error { return nil }

type Stmt struct {
	conn  *Conn
	query string
}

func (s *Stmt) Close() error  { return nil }
func (s *Stmt) NumInput() int { return -1 }

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	vals := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		vals[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	return s.conn.ExecContext(context.Background(), s.query, vals)
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	vals := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		vals[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	return s.conn.QueryContext(context.Background(), s.query, vals)
}

type Tx struct {
	conn *Conn
	tx   *db.Tx
}

func (t *Tx) Commit() error {
	err := t.tx.Commit()
	t.conn.tx = nil
	return err
}

func (t *Tx) Rollback() error {
	err := t.tx.Rollback()
	t.conn.tx = nil
	return err
}

type Result struct {
	rowsAffected int64
	lastInsertID int64
}

func (r Result) LastInsertId() (int64, error) { return r.lastInsertID, nil }
func (r Result) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type rowsAdapter struct {
	*db.Rows
}

func (r *rowsAdapter) Columns() []string {
	return append([]string(nil), r.Rows.Columns...)
}

func (r *rowsAdapter) Close() error { return nil }

func (r *rowsAdapter) Next(dest []driver.Value) error {
	row, ok := r.Rows.Next()
	if !ok {
		return io.EOF
	}
	if len(dest) < len(row) {
		return errors.New("destination row too small")
	}
	for i, v := range row {
		dest[i] = driver.Value(v)
	}
	return nil
}

func namedValues(args []driver.NamedValue) []any {
	out := make([]any, len(args))
	for i, arg := range args {
		out[i] = arg.Value
	}
	return out
}

var _ driver.Driver = (*Driver)(nil)
var _ driver.Conn = (*Conn)(nil)
var _ driver.ConnBeginTx = (*Conn)(nil)
var _ driver.ExecerContext = (*Conn)(nil)
var _ driver.QueryerContext = (*Conn)(nil)
