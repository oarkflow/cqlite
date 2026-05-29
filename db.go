package db

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
	mu           sync.RWMutex
	path         string
	opts         Options
	wal          *wal.Log
	pager        *pager.Pager
	lock         *databaseLock
	buffer       *buffer.Pool
	txmgr        *txn.Manager
	state        persistedState
	opened       time.Time
	closed       atomic.Bool
	explicitMu   sync.Mutex
	explicitTx   *Tx
	applied      uint64
	checkpointed uint64
}

type logicalRecord struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
}

const simpleWALMagic byte = 0xA5
const txWALMagic byte = 0xA6

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
	var lock *databaseLock
	if !opts.DisableFileLock {
		var err error
		lock, err = acquireDatabaseLock(path)
		if err != nil {
			return nil, err
		}
	}
	log, err := wal.Open(filepath.Join(path, "db.wal"))
	if err != nil {
		_ = releaseDatabaseLock(lock)
		return nil, err
	}
	pg, err := pager.Open(filepath.Join(path, "db.pages"), opts.PageSize)
	if err != nil {
		_ = log.Close()
		_ = releaseDatabaseLock(lock)
		return nil, err
	}
	d := &DB{
		path:   path,
		opts:   opts,
		wal:    log,
		pager:  pg,
		lock:   lock,
		buffer: buffer.New(opts.BufferPoolSize),
		txmgr:  txn.NewManager(),
		state:  newState(),
		opened: time.Now(),
	}
	if err := d.replayPhysicalPages(); err != nil {
		_ = log.Close()
		_ = pg.Close()
		_ = releaseDatabaseLock(lock)
		return nil, err
	}
	if err := d.loadSnapshot(); err != nil {
		if walHasRecords(filepath.Join(path, "db.wal")) {
			d.state = newState()
			d.applied = 0
			d.checkpointed = 0
		} else {
			_ = log.Close()
			_ = pg.Close()
			_ = releaseDatabaseLock(lock)
			return nil, err
		}
	}
	if err := d.replay(); err != nil {
		_ = log.Close()
		_ = pg.Close()
		_ = releaseDatabaseLock(lock)
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
	if res, ok, err := d.execExplicitTransaction(ctx, query, args...); ok || err != nil {
		return res, err
	}
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

type txCommand struct {
	kind string
	name string
}

func classifyTxCommand(query string) (txCommand, bool) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	if q == "" {
		return txCommand{}, false
	}
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return txCommand{}, false
	}
	first := strings.ToLower(fields[0])
	switch first {
	case "begin":
		if len(fields) == 1 {
			return txCommand{kind: "begin"}, true
		}
		second := strings.ToLower(fields[1])
		switch second {
		case "transaction", "deferred", "immediate", "exclusive":
			return txCommand{kind: "begin"}, true
		default:
			return txCommand{}, false
		}
	case "start":
		if len(fields) >= 2 && strings.EqualFold(fields[1], "transaction") {
			return txCommand{kind: "begin"}, true
		}
	case "commit", "end":
		return txCommand{kind: "commit"}, true
	case "rollback":
		if len(fields) >= 3 && strings.EqualFold(fields[1], "to") {
			return txCommand{kind: "rollback_to", name: cleanTxName(fields[len(fields)-1])}, true
		}
		return txCommand{kind: "rollback"}, true
	case "savepoint":
		if len(fields) >= 2 {
			return txCommand{kind: "savepoint", name: cleanTxName(fields[1])}, true
		}
		return txCommand{kind: "savepoint"}, true
	case "release":
		if len(fields) >= 3 && strings.EqualFold(fields[1], "savepoint") {
			return txCommand{kind: "release", name: cleanTxName(fields[2])}, true
		}
		if len(fields) >= 2 {
			return txCommand{kind: "release", name: cleanTxName(fields[1])}, true
		}
		return txCommand{kind: "release"}, true
	}
	return txCommand{}, false
}

func cleanTxName(name string) string {
	return strings.ToLower(strings.Trim(name, `"'[]`))
}

func (d *DB) execExplicitTransaction(ctx context.Context, query string, args ...any) (Result, bool, error) {
	cmd, isTxCommand := classifyTxCommand(query)
	if !isTxCommand {
		d.explicitMu.Lock()
		tx := d.explicitTx
		if tx == nil {
			d.explicitMu.Unlock()
			return Result{}, false, nil
		}
		defer d.explicitMu.Unlock()
		res, err := tx.Exec(ctx, query, args...)
		return res, true, err
	}
	if len(args) != 0 {
		return Result{}, true, errors.New("transaction control statements do not accept parameters")
	}
	d.explicitMu.Lock()
	defer d.explicitMu.Unlock()
	switch cmd.kind {
	case "begin":
		if d.explicitTx != nil {
			return Result{}, true, ErrTransactionActive
		}
		tx, err := d.Begin(ctx, TxOptions{})
		if err != nil {
			return Result{}, true, err
		}
		d.explicitTx = tx
		return Result{}, true, nil
	case "commit":
		if d.explicitTx == nil {
			return Result{}, true, errors.New("no active transaction")
		}
		tx := d.explicitTx
		if err := tx.Commit(); err != nil {
			return Result{}, true, err
		}
		d.explicitTx = nil
		return Result{}, true, nil
	case "rollback":
		if d.explicitTx == nil {
			return Result{}, true, errors.New("no active transaction")
		}
		err := d.explicitTx.Rollback()
		d.explicitTx = nil
		return Result{}, true, err
	case "savepoint", "rollback_to", "release":
		if d.explicitTx == nil {
			return Result{}, true, errors.New("no active transaction")
		}
		res, err := d.explicitTx.execTransactionCommand(cmd)
		return res, true, err
	default:
		return Result{}, true, nil
	}
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
	parseMu.Lock()
	defer parseMu.Unlock()
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
	if err := acquireDatabaseWriteLock(d.lock); err != nil {
		return Result{}, true, err
	}
	defer releaseDatabaseWriteLock(d.lock)
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
	if err := d.maybeCheckpointLocked(ctx); err != nil {
		return Result{}, true, err
	}
	return res, true, nil
}

func (d *DB) execSimpleDirect(query string, args ...any) (Result, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := acquireDatabaseWriteLock(d.lock); err != nil {
		return Result{}, true, err
	}
	defer releaseDatabaseWriteLock(d.lock)
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
	if err := d.maybeCheckpointLocked(context.Background()); err != nil {
		return Result{}, true, err
	}
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
	if plan.kind == 'd' {
		data = appendString16(data, plan.where)
	}
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
	case 'd':
		var where string
		where, rest, err = readString16(rest)
		if err != nil {
			return "", nil, true, err
		}
		query = "DELETE FROM " + table + " WHERE " + where + " = ?"
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

func encodeTransactionWAL(ops []logicalRecord) ([]byte, error) {
	data, err := json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(data))
	out = append(out, txWALMagic)
	out = append(out, data...)
	return out, nil
}

func decodeTransactionWAL(data []byte) ([]logicalRecord, bool, error) {
	if len(data) == 0 || data[0] != txWALMagic {
		return nil, false, nil
	}
	var ops []logicalRecord
	if err := json.Unmarshal(data[1:], &ops); err != nil {
		return nil, true, err
	}
	return ops, true, nil
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

func walHasRecords(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() >= 21
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
	d.explicitMu.Lock()
	if tx := d.explicitTx; tx != nil {
		defer d.explicitMu.Unlock()
		return tx.Query(ctx, query, args...)
	}
	d.explicitMu.Unlock()
	parseMu.Lock()
	defer parseMu.Unlock()
	d.mu.RLock()
	defer d.mu.RUnlock()
	return queryRows(ctx, &d.state, query, args...)
}

func (d *DB) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	d.explicitMu.Lock()
	if d.explicitTx != nil {
		_ = d.explicitTx.Rollback()
		d.explicitTx = nil
	}
	d.explicitMu.Unlock()
	d.mu.Lock()
	if d.applied != d.checkpointed {
		if err := acquireDatabaseWriteLock(d.lock); err != nil {
			d.mu.Unlock()
			_ = d.wal.Close()
			_ = d.pager.Close()
			_ = releaseDatabaseLock(d.lock)
			return err
		}
		if err := d.writeCheckpointPages(d.state, d.applied); err != nil {
			_ = releaseDatabaseWriteLock(d.lock)
			d.mu.Unlock()
			_ = d.wal.Close()
			_ = d.pager.Close()
			_ = releaseDatabaseLock(d.lock)
			return err
		}
		_ = releaseDatabaseWriteLock(d.lock)
	}
	d.mu.Unlock()
	err := d.wal.Close()
	if perr := d.pager.Close(); err == nil {
		err = perr
	}
	if lerr := releaseDatabaseLock(d.lock); err == nil {
		err = lerr
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
		d.checkpointed = meta.Applied
		d.state.Tables = make(map[string]*tableData)
		for name, schema := range d.state.Catalog.Tables {
			td, err := d.loadTablePages(name, schema, meta.Generation)
			if err != nil {
				return err
			}
			d.state.Tables[name] = td
			if err := d.verifyIndexPages(name, schema, meta.Generation); err != nil {
				return err
			}
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

func (d *DB) replayPhysicalPages() error {
	return d.wal.Replay(func(rec wal.Record) error {
		if rec.Type != wal.RecordPage {
			return nil
		}
		frame, err := wal.DecodePageRecord(rec.Data)
		if err != nil {
			return err
		}
		pg, err := page.Decode(frame.Data)
		if err != nil {
			return err
		}
		if uint64(pg.ID) != frame.PageID {
			return fmt.Errorf("page WAL id mismatch: frame=%d page=%d", frame.PageID, pg.ID)
		}
		return d.pager.Write(pg)
	})
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
			if ops, ok, err := decodeTransactionWAL(rec.Data); err != nil {
				return err
			} else if ok {
				d.state.NextTx = rec.TxID
				parseMu.Lock()
				for _, op := range ops {
					if _, err := execute(context.Background(), &d.state, op.SQL, op.Args...); err != nil {
						parseMu.Unlock()
						return err
					}
				}
				parseMu.Unlock()
				d.state.rebuildRuntime()
				d.applied = rec.TxID
				return nil
			}
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
			parseMu.Lock()
			if _, err := execute(context.Background(), &d.state, logical.SQL, logical.Args...); err != nil {
				parseMu.Unlock()
				return err
			}
			parseMu.Unlock()
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
	if len(tx.ops) == 0 {
		return nil
	}
	if err := acquireDatabaseWriteLock(d.lock); err != nil {
		return err
	}
	defer releaseDatabaseWriteLock(d.lock)
	next := d.applied + 1
	tx.writes.NextTx = next
	data, err := encodeTransactionWAL(tx.ops)
	if err != nil {
		return err
	}
	if err := d.wal.Append(wal.Record{Type: wal.RecordCommit, TxID: next, Data: data}, d.opts.SyncPolicy == SyncFull); err != nil {
		return err
	}
	d.state = tx.writes
	d.applied = next
	if err := d.maybeCheckpointLocked(context.Background()); err != nil {
		return err
	}
	return nil
}

func (d *DB) Checkpoint(ctx context.Context) error {
	if err := d.checkOpen(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := acquireDatabaseWriteLock(d.lock); err != nil {
		return err
	}
	defer releaseDatabaseWriteLock(d.lock)
	return d.checkpointLocked(ctx)
}

func (d *DB) maybeCheckpointLocked(ctx context.Context) error {
	if d.applied == d.checkpointed {
		return nil
	}
	if d.opts.CheckpointIntervalTx > 0 && d.applied-d.checkpointed >= d.opts.CheckpointIntervalTx {
		return d.checkpointLocked(ctx)
	}
	if d.opts.CheckpointIntervalBytes > 0 {
		info, err := os.Stat(filepath.Join(d.path, "db.wal"))
		if err != nil {
			return err
		}
		if info.Size() >= d.opts.CheckpointIntervalBytes {
			return d.checkpointLocked(ctx)
		}
	}
	return nil
}

func (d *DB) checkpointLocked(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if d.applied == d.checkpointed {
		return nil
	}
	return d.writeCheckpointPages(d.state, d.applied)
}

func (d *DB) writeCheckpointPages(state persistedState, applied uint64) error {
	generation := uint64(time.Now().UnixNano())
	if generation == 0 {
		generation = applied
	}
	for name, table := range state.Tables {
		if err := d.writeTablePages(name, state.Catalog.Tables[name], table, generation); err != nil {
			return err
		}
		if err := d.writeIndexPages(name, state.Catalog.Tables[name], table, generation); err != nil {
			return err
		}
	}
	meta := checkpointMeta{
		Catalog:    state.Catalog,
		NextTx:     state.NextTx,
		Pragmas:    state.Pragmas,
		Applied:    applied,
		Generation: generation,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := d.pager.WriteBlob(data); err != nil {
		return err
	}
	if err := d.appendPhysicalPagerWAL(applied); err != nil {
		return err
	}
	d.checkpointed = applied
	if err := d.cleanupCheckpointGenerations(state, generation); err != nil {
		return err
	}
	return nil
}

func (d *DB) appendPhysicalPagerWAL(txid uint64) error {
	raw, err := os.ReadFile(filepath.Join(d.path, "db.pages"))
	if err != nil {
		return err
	}
	if len(raw)%d.opts.PageSize != 0 {
		return fmt.Errorf("pager file size %d is not page aligned", len(raw))
	}
	for off := 0; off < len(raw); off += d.opts.PageSize {
		pg, err := page.Decode(raw[off : off+d.opts.PageSize])
		if err != nil {
			return err
		}
		frame := wal.PageRecord{
			PageID: uint64(pg.ID),
			LSN:    pg.LSN,
			Data:   append([]byte(nil), raw[off:off+d.opts.PageSize]...),
		}
		if err := d.wal.Append(wal.Record{Type: wal.RecordPage, TxID: txid, Data: wal.EncodePageRecord(frame)}, d.opts.SyncPolicy == SyncFull); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) tablePager(name string) (*pager.Pager, error) {
	return d.tablePagerFor(name, 0)
}

func (d *DB) tablePagerFor(name string, generation uint64) (*pager.Pager, error) {
	return pager.Open(d.tablePagerPath(name, generation), d.opts.PageSize)
}

func (d *DB) tablePagerPath(name string, generation uint64) string {
	if generation == 0 {
		return filepath.Join(d.path, "table_"+safeName(name)+".pages")
	}
	return filepath.Join(d.path, "table_"+safeName(name)+"_"+strconv.FormatUint(generation, 10)+".pages")
}

func (d *DB) indexPager(table, index string) (*pager.Pager, error) {
	return d.indexPagerFor(table, index, 0)
}

func (d *DB) indexPagerFor(table, index string, generation uint64) (*pager.Pager, error) {
	return pager.Open(d.indexPagerPath(table, index, generation), d.opts.PageSize)
}

func (d *DB) indexPagerPath(table, index string, generation uint64) string {
	base := "index_" + safeName(table) + "_" + safeName(index)
	if generation != 0 {
		base += "_" + strconv.FormatUint(generation, 10)
	}
	return filepath.Join(d.path, base+".pages")
}

func (d *DB) writeTablePages(name string, schema *catalog.Table, table *tableData, generation uint64) error {
	pg, err := d.tablePagerFor(name, generation)
	if err != nil {
		return err
	}
	defer pg.Close()
	records := make([]pager.Record, 0, len(table.Rows))
	for _, row := range table.Rows {
		data, err := encodeRowVersion(schema, row)
		if err != nil {
			return err
		}
		records = append(records, pager.Record{Key: row.RowID, Values: data})
	}
	return pg.WriteRecords(records)
}

func (d *DB) loadTablePages(name string, schema *catalog.Table, generation uint64) (*tableData, error) {
	pg, err := d.tablePagerFor(name, generation)
	if err != nil {
		return nil, err
	}
	defer pg.Close()
	data, err := pg.ReadBlob()
	if err != nil {
		records, rerr := pg.ReadRecords()
		if rerr != nil {
			if errors.Is(err, os.ErrNotExist) {
				if generation != 0 {
					return nil, fmt.Errorf("missing checkpoint table pages for %s generation %d", name, generation)
				}
				return &tableData{}, nil
			}
			return nil, err
		}
		table := &tableData{Rows: make([]rowVersion, 0, len(records))}
		for _, rec := range records {
			row, err := decodeRowVersion(schema, rec.Values)
			if err != nil {
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

func (d *DB) writeIndexPages(name string, table *catalog.Table, data *tableData, generation uint64) error {
	if table == nil {
		return nil
	}
	for _, idx := range table.Indexes {
		pg, err := d.indexPagerFor(name, idx.Name, generation)
		if err != nil {
			return err
		}
		records := make([]pager.Record, 0, len(data.Rows))
		for _, row := range data.Rows {
			if row.Deleted != 0 {
				continue
			}
			payload, err := encodeIndexRecord(row.RowID, indexKey(idx, row.Values))
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

func (d *DB) verifyIndexPages(name string, table *catalog.Table, generation uint64) error {
	if table == nil {
		return nil
	}
	for _, idx := range table.Indexes {
		path := d.indexPagerPath(name, idx.Name, generation)
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if info.Size() == 0 {
			continue
		}
		pg, err := pager.Open(path, d.opts.PageSize)
		if err != nil {
			return err
		}
		records, readErr := pg.ReadRecords()
		closeErr := pg.Close()
		if readErr != nil {
			return readErr
		}
		for _, rec := range records {
			if _, err := decodeIndexRecord(rec.Values); err != nil {
				return err
			}
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (d *DB) cleanupCheckpointGenerations(state persistedState, keep uint64) error {
	for name, table := range state.Catalog.Tables {
		if err := removeOldGenerationFiles(filepath.Join(d.path, "table_"+safeName(name)+"_*.pages"), d.tablePagerPath(name, keep)); err != nil {
			return err
		}
		for _, idx := range table.Indexes {
			if err := removeOldGenerationFiles(filepath.Join(d.path, "index_"+safeName(name)+"_"+safeName(idx.Name)+"_*.pages"), d.indexPagerPath(name, idx.Name, keep)); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeOldGenerationFiles(pattern, keep string) error {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	for _, match := range matches {
		if match == keep {
			continue
		}
		if err := os.Remove(match); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_ = os.Remove(match + ".free")
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
	ops        []logicalRecord
	closed     bool
}

type savepoint struct {
	name  string
	state persistedState
	ops   int
}

func (t *Tx) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	if err := t.check(); err != nil {
		return Result{}, err
	}
	parseMu.Lock()
	defer parseMu.Unlock()
	if ok, err := t.execTransactionControl(query); ok || err != nil {
		return Result{}, err
	}
	if res, fast, err := fastSimpleDML(&t.writes, query, args); fast || err != nil {
		if err == nil {
			t.logOp(query, args)
		}
		return res, err
	}
	if stmt, err := parseStatementCached(stripStrictOnly(query)); err == nil {
		if res, fast, err := fastDirectDML(&t.writes, stmt, args); fast || err != nil {
			if err == nil {
				t.logOp(query, args)
			}
			return res, err
		}
	}
	res, err := execute(ctx, &t.writes, query, args...)
	if err == nil {
		t.logOp(query, args)
	}
	return res, err
}

func (t *Tx) logOp(query string, args []any) {
	copied := append([]any(nil), args...)
	t.ops = append(t.ops, logicalRecord{SQL: query, Args: copied})
}

func (t *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	parseMu.Lock()
	defer parseMu.Unlock()
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
	if cmd, ok := classifyTxCommand(query); ok {
		_, err := t.execTransactionCommand(cmd)
		return true, err
	}
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
			t.ops = nil
			return true, nil
		}
		for i := len(t.savepoints) - 1; i >= 0; i-- {
			if t.savepoints[i].name == name {
				t.writes = t.savepoints[i].state.clone()
				t.ops = t.ops[:t.savepoints[i].ops]
				t.savepoints = t.savepoints[:i+1]
				return true, nil
			}
		}
		return true, errors.New("no such savepoint: " + name)
	case "savepoint":
		if name == "" {
			return true, errors.New("savepoint name is required")
		}
		t.savepoints = append(t.savepoints, savepoint{name: name, state: t.writes.clone(), ops: len(t.ops)})
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

func (t *Tx) execTransactionCommand(cmd txCommand) (Result, error) {
	switch cmd.kind {
	case "begin":
		return Result{}, nil
	case "commit":
		return Result{}, nil
	case "rollback":
		t.writes = t.db.state.clone()
		t.savepoints = nil
		t.ops = nil
		return Result{}, nil
	case "rollback_to":
		if cmd.name == "" {
			return Result{}, errors.New("savepoint name is required")
		}
		for i := len(t.savepoints) - 1; i >= 0; i-- {
			if t.savepoints[i].name == cmd.name {
				t.writes = t.savepoints[i].state.clone()
				t.ops = t.ops[:t.savepoints[i].ops]
				t.savepoints = t.savepoints[:i+1]
				return Result{}, nil
			}
		}
		return Result{}, errors.New("no such savepoint: " + cmd.name)
	case "savepoint":
		if cmd.name == "" {
			return Result{}, errors.New("savepoint name is required")
		}
		t.savepoints = append(t.savepoints, savepoint{name: cmd.name, state: t.writes.clone(), ops: len(t.ops)})
		return Result{}, nil
	case "release":
		if cmd.name == "" {
			return Result{}, errors.New("savepoint name is required")
		}
		for i := len(t.savepoints) - 1; i >= 0; i-- {
			if t.savepoints[i].name == cmd.name {
				t.savepoints = t.savepoints[:i]
				return Result{}, nil
			}
		}
		return Result{}, errors.New("no such savepoint: " + cmd.name)
	default:
		return Result{}, nil
	}
}

func (t *Tx) check() error {
	if t == nil || t.closed {
		return errors.New("transaction is closed")
	}
	return t.db.checkOpen()
}

var _ = isql.Result{}
