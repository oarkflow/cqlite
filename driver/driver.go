package driver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	db "github.com/oarkflow/sqlite"
)

const Name = "cqlite"

func init() {
	sql.Register(Name, &Driver{})
}

type Driver struct{}

func (d *Driver) Open(name string) (driver.Conn, error) {
	if isPrivateMemoryName(name) {
		inner, err := db.Open(name, db.Options{})
		if err != nil {
			return nil, err
		}
		return &Conn{db: inner, release: inner.Close}, nil
	}
	inner, release, err := acquireSharedDatabase(name)
	if err != nil {
		return nil, err
	}
	return &Conn{db: inner, release: release}, nil
}

func isPrivateMemoryName(name string) bool {
	p := strings.ToLower(strings.TrimSpace(name))
	if p == "" || p == ":memory:" {
		return true
	}
	if strings.HasPrefix(p, "file:") {
		p = strings.TrimPrefix(p, "file:")
		if i := strings.IndexAny(p, "?#"); i >= 0 {
			p = p[:i]
		}
		return p == ":memory:" || p == ""
	}
	return false
}

type sharedEngine struct {
	db      *db.DB
	refs    int
	opening bool
	closing bool
	done    chan struct{}
}

var sharedEngines = struct {
	sync.Mutex
	byPath map[string]*sharedEngine
}{byPath: make(map[string]*sharedEngine)}

func acquireSharedDatabase(name string) (*db.DB, func() error, error) {
	key, err := filepath.Abs(name)
	if err != nil {
		return nil, nil, err
	}
	key = filepath.Clean(key)
	for {
		sharedEngines.Lock()
		entry := sharedEngines.byPath[key]
		if entry != nil {
			if entry.opening || entry.closing {
				done := entry.done
				sharedEngines.Unlock()
				<-done
				continue
			}
			entry.refs++
			inner := entry.db
			sharedEngines.Unlock()
			return inner, func() error { return releaseSharedDatabase(key, entry) }, nil
		}
		entry = &sharedEngine{opening: true, done: make(chan struct{})}
		sharedEngines.byPath[key] = entry
		sharedEngines.Unlock()

		inner, openErr := db.Open(name, db.Options{})
		sharedEngines.Lock()
		if openErr != nil {
			delete(sharedEngines.byPath, key)
			close(entry.done)
			sharedEngines.Unlock()
			return nil, nil, openErr
		}
		entry.db = inner
		entry.refs = 1
		entry.opening = false
		close(entry.done)
		sharedEngines.Unlock()
		return inner, func() error { return releaseSharedDatabase(key, entry) }, nil
	}
}

func releaseSharedDatabase(key string, entry *sharedEngine) error {
	sharedEngines.Lock()
	if entry.refs > 0 {
		entry.refs--
	}
	if entry.refs > 0 {
		sharedEngines.Unlock()
		return nil
	}
	entry.closing = true
	entry.done = make(chan struct{})
	done := entry.done
	inner := entry.db
	sharedEngines.Unlock()

	err := inner.Close()
	sharedEngines.Lock()
	if sharedEngines.byPath[key] == entry {
		delete(sharedEngines.byPath, key)
	}
	close(done)
	sharedEngines.Unlock()
	return err
}

type Conn struct {
	db        *db.DB
	tx        *db.Tx
	release   func() error
	closeOnce sync.Once
	closeErr  error
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return &Stmt{conn: c, query: query}, nil
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		if c.release != nil {
			c.closeErr = c.release()
		} else if c.db != nil {
			c.closeErr = c.db.Close()
		}
	})
	return c.closeErr
}

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
	var argStack [8]any
	vals := namedValues(args, argStack[:0])
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
	var argStack [8]any
	vals := namedValues(args, argStack[:0])
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
	return r.Rows.Columns
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

func namedValues(args []driver.NamedValue, storage []any) []any {
	out := storage[:0]
	if cap(out) < len(args) {
		out = make([]any, len(args))
	} else {
		out = out[:len(args)]
	}
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
