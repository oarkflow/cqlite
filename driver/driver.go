package driver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"

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
	if c.tx != nil {
		return nil, db.ErrTransactionActive
	}
	tx, err := c.db.Begin(context.Background(), db.TxOptions{})
	if err != nil {
		return nil, err
	}
	c.tx = tx
	return &Tx{conn: c, tx: tx}, nil
}

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.tx != nil {
		return nil, db.ErrTransactionActive
	}
	if opts.ReadOnly {
		return nil, db.ErrReadOnlyUnsupported
	}
	txOpts, err := txOptions(opts)
	if err != nil {
		return nil, err
	}
	tx, err := c.db.Begin(ctx, txOpts)
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

func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	vals := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		vals[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	return s.conn.QueryContext(context.Background(), s.query, vals)
}

func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
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

func (r *rowsAdapter) ColumnTypeDatabaseTypeName(index int) string {
	types := r.Rows.ColumnTypes()
	if index < 0 || index >= len(types) {
		return ""
	}
	return types[index].DatabaseTypeName
}

func (r *rowsAdapter) ColumnTypeNullable(index int) (bool, bool) {
	types := r.Rows.ColumnTypes()
	if index < 0 || index >= len(types) {
		return true, false
	}
	return types[index].Nullable, true
}

func (r *rowsAdapter) ColumnTypeScanType(index int) reflect.Type {
	return reflect.TypeOf((*any)(nil)).Elem()
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
		if arg.Name != "" {
			out[i] = db.Named(arg.Name, arg.Value)
		} else {
			out[i] = arg.Value
		}
	}
	return out
}

func txOptions(opts driver.TxOptions) (db.TxOptions, error) {
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault, sql.LevelReadCommitted, sql.LevelRepeatableRead, sql.LevelSnapshot:
		return db.TxOptions{}, nil
	case sql.LevelSerializable:
		return db.TxOptions{Isolation: "SERIALIZABLE"}, nil
	default:
		return db.TxOptions{}, db.ErrIsolationUnsupported
	}
}

var _ driver.Driver = (*Driver)(nil)
var _ driver.Conn = (*Conn)(nil)
var _ driver.ConnBeginTx = (*Conn)(nil)
var _ driver.ExecerContext = (*Conn)(nil)
var _ driver.QueryerContext = (*Conn)(nil)
var _ driver.StmtExecContext = (*Stmt)(nil)
var _ driver.StmtQueryContext = (*Stmt)(nil)
var _ driver.RowsColumnTypeDatabaseTypeName = (*rowsAdapter)(nil)
var _ driver.RowsColumnTypeNullable = (*rowsAdapter)(nil)
var _ driver.RowsColumnTypeScanType = (*rowsAdapter)(nil)
