package db

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oarkflow/sqlparser/ast"
	"github.com/sujit/sqlite/internal/buffer"
	"github.com/sujit/sqlite/internal/catalog"
	"github.com/sujit/sqlite/internal/page"
	"github.com/sujit/sqlite/internal/pager"
	isql "github.com/sujit/sqlite/internal/sql"
	"github.com/sujit/sqlite/internal/txn"
	"github.com/sujit/sqlite/internal/wal"
)

type DB struct {
	mu      sync.RWMutex
	path    string
	opts    Options
	wal     *wal.Log
	pager   *pager.Pager
	buffer  *buffer.Pool
	txmgr   *txn.Manager
	state   persistedState
	opened  time.Time
	closed  atomic.Bool
	applied uint64
}

type logicalRecord struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
}

const simpleWALMagic byte = 0xA5

func Open(path string, opts Options) (*DB, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if opts.PageSize == 0 {
		opts.PageSize = page.DefaultSize
	}
	if opts.BufferPoolSize == 0 {
		opts.BufferPoolSize = 128
	}
	if opts.SyncPolicy == "" {
		opts.SyncPolicy = SyncFull
	}
	if opts.CacheSize == 0 {
		opts.CacheSize = opts.BufferPoolSize
	}
	if opts.JournalMode == "" {
		opts.JournalMode = "wal"
	}
	if opts.Synchronous == "" {
		opts.Synchronous = string(opts.SyncPolicy)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	log, err := wal.Open(filepath.Join(path, "db.wal"))
	if err != nil {
		return nil, err
	}
	pg, err := pager.Open(filepath.Join(path, "db.pages"), opts.PageSize)
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	d := &DB{
		path:   path,
		opts:   opts,
		wal:    log,
		pager:  pg,
		buffer: buffer.New(opts.BufferPoolSize),
		txmgr:  txn.NewManager(),
		state:  newState(),
		opened: time.Now(),
	}
	if err := d.loadSnapshot(); err != nil {
		_ = log.Close()
		_ = pg.Close()
		return nil, err
	}
	if err := d.replay(); err != nil {
		_ = log.Close()
		_ = pg.Close()
		return nil, err
	}
	if d.state.Pragmas.PageSize == 0 {
		d.state.Pragmas.PageSize = opts.PageSize
	}
	if d.state.Pragmas.CacheSize == 0 {
		d.state.Pragmas.CacheSize = opts.CacheSize
	}
	if d.state.Pragmas.JournalMode == "" {
		d.state.Pragmas.JournalMode = opts.JournalMode
	}
	if d.state.Pragmas.Synchronous == "" {
		d.state.Pragmas.Synchronous = strings.ToLower(opts.Synchronous)
	}
	return d, nil
}

func (d *DB) Begin(ctx context.Context, opts TxOptions) (*Tx, error) {
	if err := d.checkOpen(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return &Tx{db: d, tx: d.txmgr.Begin(opts.Isolation), writes: d.state.clone()}, nil
}

func (d *DB) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	if res, ok, err := d.execDirect(ctx, query, args...); ok || err != nil {
		return res, err
	}
	tx, err := d.Begin(ctx, TxOptions{})
	if err != nil {
		return Result{}, err
	}
	res, err := tx.Exec(ctx, query, args...)
	if err != nil {
		_ = tx.Rollback()
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return res, nil
}

func (d *DB) execDirect(ctx context.Context, query string, args ...any) (Result, bool, error) {
	if err := d.checkOpen(); err != nil {
		return Result{}, true, err
	}
	select {
	case <-ctx.Done():
		return Result{}, true, ctx.Err()
	default:
	}
	if res, ok, err := d.execSimpleDirect(query, args...); ok || err != nil {
		return res, ok, err
	}
	stmt, err := parseStatementCached(stripStrictOnly(query))
	if err != nil {
		return Result{}, false, nil
	}
	switch stmt.(type) {
	case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt:
	default:
		return Result{}, false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	next := d.applied + 1
	d.state.NextTx = next
	res, fast, err := fastDirectDML(&d.state, stmt, args)
	if err != nil {
		return Result{}, true, err
	}
	if !fast {
		res, err = execute(ctx, &d.state, query, args...)
		if err != nil {
			d.state.rebuildRuntime()
			return Result{}, true, err
		}
	}
	if err := d.appendLogical(next, query, args); err != nil {
		return Result{}, true, err
	}
	d.applied = next
	return res, true, nil
}

func (d *DB) execSimpleDirect(query string, args ...any) (Result, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := d.applied + 1
	d.state.NextTx = next
	res, fast, err := fastSimpleDML(&d.state, query, args)
	if err != nil {
		return Result{}, true, err
	}
	if !fast {
		return Result{}, false, nil
	}
	if err := d.appendLogical(next, query, args); err != nil {
		return Result{}, true, err
	}
	d.applied = next
	return res, true, nil
}

func (d *DB) appendLogical(txid uint64, query string, args []any) error {
	if data, ok := encodeSimpleWAL(query, args); ok {
		return d.wal.Append(wal.Record{Type: wal.RecordCommit, TxID: txid, Data: data}, d.opts.SyncPolicy == SyncFull)
	}
	data, err := json.Marshal(logicalRecord{SQL: query, Args: args})
	if err != nil {
		return err
	}
	return d.wal.Append(wal.Record{Type: wal.RecordCommit, TxID: txid, Data: data}, d.opts.SyncPolicy == SyncFull)
}

func encodeSimpleWAL(query string, args []any) ([]byte, bool) {
	plan, ok := simplePlan(query)
	if !ok || plan.params != len(args) || len(args) > math.MaxUint16 {
		return nil, false
	}
	data := make([]byte, 0, 64+len(args)*16)
	data = append(data, simpleWALMagic, plan.kind)
	data = appendString16(data, plan.table)
	if plan.kind == 'u' {
		data = appendString16(data, plan.col)
		data = appendString16(data, plan.where)
		data = binary.LittleEndian.AppendUint64(data, uint64(plan.inc))
	}
	data = binary.LittleEndian.AppendUint16(data, uint16(len(args)))
	for _, arg := range args {
		var ok bool
		data, ok = appendWALValue(data, arg)
		if !ok {
			return nil, false
		}
	}
	return data, true
}

func appendString16(dst []byte, s string) []byte {
	if len(s) > math.MaxUint16 {
		return nil
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(s)))
	return append(dst, s...)
}

func decodeSimpleWAL(data []byte) (string, []any, bool, error) {
	if len(data) < 2 || data[0] != simpleWALMagic {
		return "", nil, false, nil
	}
	kind := data[1]
	rest := data[2:]
	table, rest, err := readString16(rest)
	if err != nil {
		return "", nil, true, err
	}
	query := ""
	switch kind {
	case 'i':
	case 'u':
		var col, where string
		col, rest, err = readString16(rest)
		if err != nil {
			return "", nil, true, err
		}
		where, rest, err = readString16(rest)
		if err != nil {
			return "", nil, true, err
		}
		if len(rest) < 8 {
			return "", nil, true, errors.New("truncated simple WAL update increment")
		}
		inc := int64(binary.LittleEndian.Uint64(rest[:8]))
		rest = rest[8:]
		query = "UPDATE " + table + " SET " + col + " = " + col + " + " + strconv.FormatInt(inc, 10) + " WHERE " + where + " = ?"
	default:
		return "", nil, true, errors.New("unknown simple WAL record")
	}
	if len(rest) < 2 {
		return "", nil, true, errors.New("truncated simple WAL argument count")
	}
	n := int(binary.LittleEndian.Uint16(rest[:2]))
	rest = rest[2:]
	args := make([]any, 0, n)
	for i := 0; i < n; i++ {
		var v any
		v, rest, err = readWALValue(rest)
		if err != nil {
			return "", nil, true, err
		}
		args = append(args, v)
	}
	if len(rest) != 0 {
		return "", nil, true, errors.New("trailing simple WAL bytes")
	}
	if kind == 'i' {
		query = "INSERT INTO " + table + " VALUES (" + strings.TrimRight(strings.Repeat("?,", n), ",") + ")"
	}
	return query, args, true, nil
}

func readString16(data []byte) (string, []byte, error) {
	if len(data) < 2 {
		return "", nil, errors.New("truncated simple WAL string")
	}
	n := int(binary.LittleEndian.Uint16(data[:2]))
	data = data[2:]
	if len(data) < n {
		return "", nil, errors.New("truncated simple WAL string payload")
	}
	return string(data[:n]), data[n:], nil
}

func appendWALValue(dst []byte, v any) ([]byte, bool) {
	switch x := v.(type) {
	case nil:
		return append(dst, 0), true
	case int:
		dst = append(dst, 1)
		return binary.LittleEndian.AppendUint64(dst, uint64(int64(x))), true
	case int64:
		dst = append(dst, 1)
		return binary.LittleEndian.AppendUint64(dst, uint64(x)), true
	case float64:
		dst = append(dst, 2)
		return binary.LittleEndian.AppendUint64(dst, math.Float64bits(x)), true
	case string:
		if len(x) > math.MaxUint32 {
			return nil, false
		}
		dst = append(dst, 3)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(x)))
		return append(dst, x...), true
	case []byte:
		if len(x) > math.MaxUint32 {
			return nil, false
		}
		dst = append(dst, 4)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(x)))
		return append(dst, x...), true
	case bool:
		dst = append(dst, 5)
		if x {
			return append(dst, 1), true
		}
		return append(dst, 0), true
	default:
		return nil, false
	}
}

func readWALValue(data []byte) (any, []byte, error) {
	if len(data) < 1 {
		return nil, nil, errors.New("truncated simple WAL value")
	}
	typ := data[0]
	data = data[1:]
	switch typ {
	case 0:
		return nil, data, nil
	case 1:
		if len(data) < 8 {
			return nil, nil, errors.New("truncated integer WAL value")
		}
		return int64(binary.LittleEndian.Uint64(data[:8])), data[8:], nil
	case 2:
		if len(data) < 8 {
			return nil, nil, errors.New("truncated real WAL value")
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(data[:8])), data[8:], nil
	case 3:
		if len(data) < 4 {
			return nil, nil, errors.New("truncated text WAL value")
		}
		n := int(binary.LittleEndian.Uint32(data[:4]))
		data = data[4:]
		if len(data) < n {
			return nil, nil, errors.New("truncated text WAL payload")
		}
		return string(data[:n]), data[n:], nil
	case 4:
		if len(data) < 4 {
			return nil, nil, errors.New("truncated blob WAL value")
		}
		n := int(binary.LittleEndian.Uint32(data[:4]))
		data = data[4:]
		if len(data) < n {
			return nil, nil, errors.New("truncated blob WAL payload")
		}
		v := append([]byte(nil), data[:n]...)
		return v, data[n:], nil
	case 5:
		if len(data) < 1 {
			return nil, nil, errors.New("truncated bool WAL value")
		}
		return data[0] != 0, data[1:], nil
	default:
		return nil, nil, errors.New("unknown simple WAL value type")
	}
}

func stripStrictOnly(query string) string {
	q, _ := stripSQLiteStrict(query)
	return q
}

func (d *DB) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if err := d.checkOpen(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return queryRows(ctx, &d.state, query, args...)
}

func (d *DB) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	d.mu.Lock()
	if err := d.writeCheckpointPages(d.state, d.applied); err != nil {
		d.mu.Unlock()
		_ = d.wal.Close()
		_ = d.pager.Close()
		return err
	}
	d.mu.Unlock()
	err := d.wal.Close()
	if perr := d.pager.Close(); err == nil {
		err = perr
	}
	return err
}

func (d *DB) Stats() Stats {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return Stats{Tables: len(d.state.Catalog.Tables), Transactions: d.applied, OpenedAt: d.opened}
}

func (d *DB) loadSnapshot() error {
	data, err := d.pager.ReadBlob()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			data, err = os.ReadFile(filepath.Join(d.path, "db.wal.snapshot"))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if err := d.pager.WriteBlob(data); err != nil {
				return err
			}
			_ = os.Remove(filepath.Join(d.path, "db.wal.snapshot"))
			return json.Unmarshal(data, &d.state)
		}
		return err
	}
	var meta checkpointMeta
	if err := json.Unmarshal(data, &meta); err == nil && meta.Catalog.Tables != nil {
		d.state.Catalog = meta.Catalog
		d.state.NextTx = meta.NextTx
		d.state.Pragmas = meta.Pragmas
		d.applied = meta.Applied
		d.state.Tables = make(map[string]*tableData)
		for name := range d.state.Catalog.Tables {
			td, err := d.loadTablePages(name)
			if err != nil {
				return err
			}
			d.state.Tables[name] = td
		}
		d.state.rebuildRuntime()
		return nil
	}
	if err := json.Unmarshal(data, &d.state); err != nil {
		return err
	}
	d.state.rebuildRuntime()
	return nil
}

func (d *DB) replay() error {
	return d.wal.Replay(func(rec wal.Record) error {
		if rec.Type == wal.RecordSnapshot {
			var s persistedState
			if err := json.Unmarshal(rec.Data, &s); err != nil {
				return err
			}
			if rec.TxID >= d.applied {
				d.state = s
				d.state.rebuildRuntime()
				d.applied = rec.TxID
			}
			return nil
		}
		if rec.Type == wal.RecordCommit && rec.TxID > d.applied {
			if query, args, ok, err := decodeSimpleWAL(rec.Data); err != nil {
				return err
			} else if ok {
				d.state.NextTx = rec.TxID
				if _, fast, err := fastSimpleDML(&d.state, query, args); err != nil {
					return err
				} else if !fast {
					return errors.New("could not replay simple WAL record")
				}
				d.applied = rec.TxID
				return nil
			}
			var logical logicalRecord
			if err := json.Unmarshal(rec.Data, &logical); err != nil {
				return err
			}
			d.state.NextTx = rec.TxID
			if _, err := execute(context.Background(), &d.state, logical.SQL, logical.Args...); err != nil {
				return err
			}
			d.state.rebuildRuntime()
			d.applied = rec.TxID
		}
		return nil
	})
}

func (d *DB) commit(tx *Tx) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(); err != nil {
		return err
	}
	tx.writes.NextTx++
	data, err := json.Marshal(tx.writes)
	if err != nil {
		return err
	}
	if err := d.wal.Append(wal.Record{Type: wal.RecordSnapshot, TxID: tx.tx.ID, Data: data}, d.opts.SyncPolicy == SyncFull); err != nil {
		return err
	}
	d.state = tx.writes.clone()
	d.state.rebuildRuntime()
	d.applied = tx.tx.ID
	return nil
}

func (d *DB) writeCheckpointPages(state persistedState, applied uint64) error {
	meta := checkpointMeta{
		Catalog: state.Catalog,
		NextTx:  state.NextTx,
		Pragmas: state.Pragmas,
		Applied: applied,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := d.pager.WriteBlob(data); err != nil {
		return err
	}
	for name, table := range state.Tables {
		if err := d.writeTablePages(name, table); err != nil {
			return err
		}
		if err := d.writeIndexPages(name, state.Catalog.Tables[name], table); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) tablePager(name string) (*pager.Pager, error) {
	return pager.Open(filepath.Join(d.path, "table_"+safeName(name)+".pages"), d.opts.PageSize)
}

func (d *DB) indexPager(table, index string) (*pager.Pager, error) {
	return pager.Open(filepath.Join(d.path, "index_"+safeName(table)+"_"+safeName(index)+".pages"), d.opts.PageSize)
}

func (d *DB) writeTablePages(name string, table *tableData) error {
	pg, err := d.tablePager(name)
	if err != nil {
		return err
	}
	defer pg.Close()
	records := make([]pager.Record, 0, len(table.Rows))
	for _, row := range table.Rows {
		data, err := json.Marshal(row)
		if err != nil {
			return err
		}
		records = append(records, pager.Record{Key: row.RowID, Values: data})
	}
	return pg.WriteRecords(records)
}

func (d *DB) loadTablePages(name string) (*tableData, error) {
	pg, err := d.tablePager(name)
	if err != nil {
		return nil, err
	}
	defer pg.Close()
	data, err := pg.ReadBlob()
	if err != nil {
		records, rerr := pg.ReadRecords()
		if rerr != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &tableData{}, nil
			}
			return nil, err
		}
		table := &tableData{Rows: make([]rowVersion, 0, len(records))}
		for _, rec := range records {
			var row rowVersion
			if err := json.Unmarshal(rec.Values, &row); err != nil {
				return nil, err
			}
			table.Rows = append(table.Rows, row)
		}
		return table, nil
	}
	var table tableData
	if err := json.Unmarshal(data, &table); err != nil {
		return nil, err
	}
	return &table, nil
}

func (d *DB) writeIndexPages(name string, table *catalog.Table, data *tableData) error {
	if table == nil {
		return nil
	}
	for _, idx := range table.Indexes {
		pg, err := d.indexPager(name, idx.Name)
		if err != nil {
			return err
		}
		records := make([]pager.Record, 0, len(data.Rows))
		for _, row := range data.Rows {
			if row.Deleted != 0 {
				continue
			}
			entry := map[string]any{"rowid": row.RowID, "key": indexKey(idx, row.Values)}
			payload, err := json.Marshal(entry)
			if err != nil {
				_ = pg.Close()
				return err
			}
			records = append(records, pager.Record{Key: row.RowID, Values: payload})
		}
		if err := pg.WriteRecords(records); err != nil {
			_ = pg.Close()
			return err
		}
		if err := pg.Close(); err != nil {
			return err
		}
	}
	return nil
}

func indexKey(idx catalog.Index, values map[string]any) []any {
	if len(idx.Columns) == 0 {
		return nil
	}
	key := make([]any, 0, len(idx.Columns))
	for _, col := range idx.Columns {
		key = append(key, values[col])
	}
	return key
}

func safeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "table"
	}
	return b.String()
}

func (d *DB) checkOpen() error {
	if d.closed.Load() {
		return errors.New("database is closed")
	}
	return nil
}

type Tx struct {
	db         *DB
	tx         *txn.Tx
	writes     persistedState
	savepoints []savepoint
	closed     bool
}

type savepoint struct {
	name  string
	state persistedState
}

func (t *Tx) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	if err := t.check(); err != nil {
		return Result{}, err
	}
	if ok, err := t.execTransactionControl(query); ok || err != nil {
		return Result{}, err
	}
	if res, fast, err := fastSimpleDML(&t.writes, query, args); fast || err != nil {
		return res, err
	}
	if stmt, err := parseStatementCached(stripStrictOnly(query)); err == nil {
		if res, fast, err := fastDirectDML(&t.writes, stmt, args); fast || err != nil {
			return res, err
		}
	}
	return execute(ctx, &t.writes, query, args...)
}

func (t *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	return queryRows(ctx, &t.writes, query, args...)
}

func (t *Tx) Commit() error {
	if err := t.check(); err != nil {
		return err
	}
	t.closed = true
	t.tx.Close()
	return t.db.commit(t)
}

func (t *Tx) Rollback() error {
	if t.closed {
		return nil
	}
	t.closed = true
	t.tx.Close()
	return nil
}

func (t *Tx) execTransactionControl(query string) (bool, error) {
	stmt, err := parseStatementCached(query)
	if err != nil {
		return false, nil
	}
	txstmt, ok := stmt.(*ast.TransactionStmt)
	if !ok {
		return false, nil
	}
	action := strings.ToLower(strings.TrimSpace(string(txstmt.Action)))
	name := ""
	if txstmt.Savepoint != nil {
		name = strings.ToLower(txstmt.Savepoint.Unquoted)
	}
	switch action {
	case "begin", "start", "start_transaction", "commit":
		return true, nil
	case "rollback":
		if name == "" {
			t.writes = t.db.state.clone()
			t.savepoints = nil
			return true, nil
		}
		for i := len(t.savepoints) - 1; i >= 0; i-- {
			if t.savepoints[i].name == name {
				t.writes = t.savepoints[i].state.clone()
				t.savepoints = t.savepoints[:i+1]
				return true, nil
			}
		}
		return true, errors.New("no such savepoint: " + name)
	case "savepoint":
		if name == "" {
			return true, errors.New("savepoint name is required")
		}
		t.savepoints = append(t.savepoints, savepoint{name: name, state: t.writes.clone()})
		return true, nil
	case "release", "release_savepoint":
		if name == "" {
			return true, errors.New("savepoint name is required")
		}
		for i := len(t.savepoints) - 1; i >= 0; i-- {
			if t.savepoints[i].name == name {
				t.savepoints = t.savepoints[:i]
				return true, nil
			}
		}
		return true, errors.New("no such savepoint: " + name)
	default:
		return true, nil
	}
}

func (t *Tx) check() error {
	if t == nil || t.closed {
		return errors.New("transaction is closed")
	}
	return t.db.checkOpen()
}

var _ = isql.Result{}
