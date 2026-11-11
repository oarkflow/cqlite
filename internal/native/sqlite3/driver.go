//go:build sqlite_native_benchmark && cgo

// Package sqlite3 provides a small database/sql adapter over the host SQLite
// library. It is used only by opt-in native comparison benchmarks.
package sqlite3

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>

static int bind_text_copy(sqlite3_stmt *stmt, int index, const char *value, int n) {
	return sqlite3_bind_text(stmt, index, value, n, SQLITE_TRANSIENT);
}

static int bind_blob_copy(sqlite3_stmt *stmt, int index, const void *value, int n) {
	return sqlite3_bind_blob(stmt, index, value, n, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"
)

const Name = "sqlite3-native-benchmark"

func init() { sql.Register(Name, Driver{}) }

type Driver struct{}

func (Driver) Open(name string) (driver.Conn, error) {
	path := C.CString(name)
	defer C.free(unsafe.Pointer(path))
	var handle *C.sqlite3
	if rc := C.sqlite3_open(path, &handle); rc != C.SQLITE_OK {
		err := sqliteError(handle, rc)
		if handle != nil {
			C.sqlite3_close(handle)
		}
		return nil, err
	}
	C.sqlite3_busy_timeout(handle, 5000)
	return &conn{db: handle}, nil
}

type conn struct {
	mu sync.Mutex
	db *C.sqlite3
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{conn: c, query: query}, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	rc := C.sqlite3_close(c.db)
	if rc != C.SQLITE_OK {
		return sqliteError(c.db, rc)
	}
	c.db = nil
	return nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly {
		return nil, errors.New("read-only transaction unsupported by benchmark adapter")
	}
	query := "BEGIN"
	if opts.Isolation == driver.IsolationLevel(sql.LevelSerializable) {
		query = "BEGIN IMMEDIATE"
	}
	if err := c.execSQL(ctx, query); err != nil {
		return nil, err
	}
	return &tx{conn: c}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	statement, err := c.prepareLocked(query)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(statement)
	if err := bindAll(c.db, statement, args); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		rc := C.sqlite3_step(statement)
		if rc == C.SQLITE_DONE {
			return result{affected: int64(C.sqlite3_changes(c.db)), lastID: int64(C.sqlite3_last_insert_rowid(c.db))}, nil
		}
		if rc != C.SQLITE_ROW {
			return nil, sqliteError(c.db, rc)
		}
	}
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	statement, err := c.prepareLocked(query)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err := bindAll(c.db, statement, args); err != nil {
		C.sqlite3_finalize(statement)
		c.mu.Unlock()
		return nil, err
	}
	columns := int(C.sqlite3_column_count(statement))
	names := make([]string, columns)
	for i := range names {
		names[i] = C.GoString(C.sqlite3_column_name(statement, C.int(i)))
	}
	return &rows{conn: c, stmt: statement, columns: names, ctx: ctx}, nil
}

func (c *conn) execSQL(ctx context.Context, query string) error {
	_, err := c.ExecContext(ctx, query, nil)
	return err
}

func (c *conn) prepareLocked(query string) (*C.sqlite3_stmt, error) {
	if c.db == nil {
		return nil, errors.New("sqlite connection is closed")
	}
	sql := C.CString(query)
	defer C.free(unsafe.Pointer(sql))
	var statement *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(c.db, sql, -1, &statement, nil); rc != C.SQLITE_OK {
		return nil, sqliteError(c.db, rc)
	}
	return statement, nil
}

type tx struct{ conn *conn }

func (t *tx) Commit() error   { return t.conn.execSQL(context.Background(), "COMMIT") }
func (t *tx) Rollback() error { return t.conn.execSQL(context.Background(), "ROLLBACK") }

type stmt struct {
	conn  *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, value := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}
	return s.conn.ExecContext(context.Background(), s.query, named)
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, value := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}
	return s.conn.QueryContext(context.Background(), s.query, named)
}

type rows struct {
	conn    *conn
	stmt    *C.sqlite3_stmt
	columns []string
	ctx     context.Context
	closed  bool
}

func (r *rows) Columns() []string { return r.columns }

func (r *rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if rc := C.sqlite3_finalize(r.stmt); rc != C.SQLITE_OK {
		r.conn.mu.Unlock()
		return sqliteError(r.conn.db, rc)
	}
	r.stmt = nil
	r.conn.mu.Unlock()
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if r.closed {
		return io.EOF
	}
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	default:
	}
	rc := C.sqlite3_step(r.stmt)
	if rc == C.SQLITE_DONE {
		return io.EOF
	}
	if rc != C.SQLITE_ROW {
		return sqliteError(r.conn.db, rc)
	}
	for i := range dest {
		column := C.int(i)
		switch C.sqlite3_column_type(r.stmt, column) {
		case C.SQLITE_INTEGER:
			dest[i] = int64(C.sqlite3_column_int64(r.stmt, column))
		case C.SQLITE_FLOAT:
			dest[i] = float64(C.sqlite3_column_double(r.stmt, column))
		case C.SQLITE_TEXT:
			ptr := (*C.char)(unsafe.Pointer(C.sqlite3_column_text(r.stmt, column)))
			dest[i] = C.GoStringN(ptr, C.sqlite3_column_bytes(r.stmt, column))
		case C.SQLITE_BLOB:
			dest[i] = C.GoBytes(C.sqlite3_column_blob(r.stmt, column), C.sqlite3_column_bytes(r.stmt, column))
		default:
			dest[i] = nil
		}
	}
	return nil
}

type result struct {
	affected int64
	lastID   int64
}

func (r result) LastInsertId() (int64, error) { return r.lastID, nil }
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

func bindAll(db *C.sqlite3, statement *C.sqlite3_stmt, args []driver.NamedValue) error {
	for i, arg := range args {
		index := C.int(i + 1)
		var rc C.int
		switch value := arg.Value.(type) {
		case nil:
			rc = C.sqlite3_bind_null(statement, index)
		case int64:
			rc = C.sqlite3_bind_int64(statement, index, C.sqlite3_int64(value))
		case float64:
			rc = C.sqlite3_bind_double(statement, index, C.double(value))
		case bool:
			if value {
				rc = C.sqlite3_bind_int64(statement, index, 1)
			} else {
				rc = C.sqlite3_bind_int64(statement, index, 0)
			}
		case string:
			text := C.CString(value)
			rc = C.bind_text_copy(statement, index, text, C.int(len(value)))
			C.free(unsafe.Pointer(text))
		case []byte:
			if len(value) == 0 {
				rc = C.bind_blob_copy(statement, index, nil, 0)
			} else {
				rc = C.bind_blob_copy(statement, index, unsafe.Pointer(&value[0]), C.int(len(value)))
			}
		case time.Time:
			formatted := value.Format(time.RFC3339Nano)
			text := C.CString(formatted)
			rc = C.bind_text_copy(statement, index, text, C.int(len(formatted)))
			C.free(unsafe.Pointer(text))
		default:
			return fmt.Errorf("unsupported SQLite benchmark bind type %T", value)
		}
		if rc != C.SQLITE_OK {
			return sqliteError(db, rc)
		}
	}
	return nil
}

func sqliteError(db *C.sqlite3, rc C.int) error {
	if db == nil {
		return fmt.Errorf("sqlite3 error %d", int(rc))
	}
	return fmt.Errorf("sqlite3 error %d: %s", int(rc), C.GoString(C.sqlite3_errmsg(db)))
}

var _ driver.Driver = Driver{}
var _ driver.ConnBeginTx = (*conn)(nil)
var _ driver.ExecerContext = (*conn)(nil)
var _ driver.QueryerContext = (*conn)(nil)
var _ driver.Rows = (*rows)(nil)
