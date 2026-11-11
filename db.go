package cqlite

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oarkflow/cqlite/internal/buffer"
	"github.com/oarkflow/cqlite/internal/catalog"
	"github.com/oarkflow/cqlite/internal/page"
	"github.com/oarkflow/cqlite/internal/pager"
	isql "github.com/oarkflow/cqlite/internal/sql"
	"github.com/oarkflow/cqlite/internal/sqlparser/ast"
	"github.com/oarkflow/cqlite/internal/txn"
	"github.com/oarkflow/cqlite/internal/wal"
)

type DB struct {
	mu           sync.RWMutex
	writeGate    chan struct{}
	path         string
	singleFile   string
	lockPath     string
	cleanupPath  bool
	cleanupLocks bool
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
	hasAttached  atomic.Bool
	// attached holds databases opened by ATTACH, keyed by schema name.
	attachedMu sync.RWMutex
	attached   map[string]*attachedDB
	// ephemeralRoot is the temporary directory backing a ":memory:" database.
	// Close removes the whole tree so nothing survives the connection.
	ephemeralRoot string
	applied       uint64
	checkpointed  uint64
	clean         bool
	walSize       int64 // protected by mu; bytes observed from the shared WAL
}

type logicalRecord struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
}

const simpleWALMagic byte = 0xA5
const txWALMagic byte = 0xA6
const txSimpleWALMagic byte = 0xA7
const stateWALMagic byte = 0xA8
const txSimpleInsertBatchWALMagic byte = 0xA9

type simpleWALRecord struct {
	plan simpleDMLPlan
	args []any
}

// Open opens a database at path. Two special names are recognised so that no
// filesystem entry is ever created for them:
//
//   - ":memory:" (and "file::memory:") is a database that lives only for the
//     lifetime of the returned *DB and is backed by a private temporary
//     directory that Close removes.
//   - "" is a private temporary database with the same lifetime.
//
// Any other path is a regular persistent database. Paths ending in ".db" are
// stored as a single packed file; all others are directories.
func Open(path string, opts Options) (*DB, error) {
	if isMemoryDatabaseName(path) || path == "" {
		root, err := os.MkdirTemp("", "cqlite-ephemeral-*")
		if err != nil {
			return nil, err
		}
		db, err := openWithPath(filepath.Join(root, "mem.db"), opts)
		if err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
		db.ephemeralRoot = root
		return db, nil
	}
	return openWithPath(path, opts)
}

// isMemoryDatabaseName reports whether a path names a pure in-memory database.
func isMemoryDatabaseName(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	if p == ":memory:" {
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

func openWithPath(path string, opts Options) (*DB, error) {
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
	storagePath, lockPath, singleFile, cleanupPath, cleanupLocks, err := prepareDatabaseStorage(path)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if cleanupPath {
			_ = os.RemoveAll(storagePath)
		}
		if cleanupLocks {
			_ = os.RemoveAll(lockPath)
		}
	}
	var lock *databaseLock
	if !opts.DisableFileLock {
		var err error
		lock, err = acquireDatabaseLock(lockPath)
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	log, err := wal.Open(filepath.Join(storagePath, "db.wal"))
	if err != nil {
		_ = releaseDatabaseLock(lock)
		cleanup()
		return nil, err
	}
	pg, err := pager.Open(filepath.Join(storagePath, "db.pages"), opts.PageSize)
	if err != nil {
		_ = log.Close()
		_ = releaseDatabaseLock(lock)
		cleanup()
		return nil, err
	}
	d := &DB{
		writeGate:    make(chan struct{}, 1),
		path:         storagePath,
		singleFile:   singleFile,
		lockPath:     lockPath,
		cleanupPath:  cleanupPath,
		cleanupLocks: cleanupLocks,
		opts:         opts,
		wal:          log,
		pager:        pg,
		lock:         lock,
		buffer:       buffer.New(opts.BufferPoolSize),
		txmgr:        txn.NewManager(),
		state:        newState(),
		opened:       time.Now(),
	}
	d.writeGate <- struct{}{}
	// A handle may open while another process is committing. Remember the WAL
	// boundary before recovery so a commit that lands between the clean-marker
	// check and the end of replay cannot be mistaken for already-applied data.
	recoveryWALSize, err := d.wal.Size()
	if err != nil {
		_ = log.Close()
		_ = pg.Close()
		_ = releaseDatabaseLock(lock)
		cleanup()
		return nil, err
	}
	if err := d.loadSnapshot(); err != nil {
		if walHasRecords(filepath.Join(storagePath, "db.wal")) {
			if replayErr := d.replayPhysicalPages(); replayErr != nil {
				_ = log.Close()
				_ = pg.Close()
				_ = releaseDatabaseLock(lock)
				cleanup()
				return nil, replayErr
			}
			d.state = newState()
			d.applied = 0
			d.checkpointed = 0
			_ = d.loadSnapshot()
		} else {
			_ = log.Close()
			_ = pg.Close()
			_ = releaseDatabaseLock(lock)
			cleanup()
			return nil, err
		}
	}
	if clean, err := d.hasCleanCheckpointMarker(); err != nil {
		_ = log.Close()
		_ = pg.Close()
		_ = releaseDatabaseLock(lock)
		cleanup()
		return nil, err
	} else if !clean {
		if err := d.replay(); err != nil {
			_ = log.Close()
			_ = pg.Close()
			_ = releaseDatabaseLock(lock)
			cleanup()
			return nil, err
		}
	} else {
		d.clean = true
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
	// synchronous is a connection setting in SQLite. Apply the current open
	// options even when the checkpoint contains a value from an earlier handle.
	d.state.Pragmas.Synchronous = strings.ToLower(opts.Synchronous)
	// Replay captures and reads a fixed WAL size. If another writer appends after
	// that boundary, the next refresh sees the larger file and replays it.
	if d.walSize == 0 {
		d.walSize = recoveryWALSize
	}
	return d, nil
}

func prepareDatabaseStorage(path string) (storagePath, lockPath, singleFile string, cleanupPath, cleanupLocks bool, err error) {
	if !isSingleFileDatabasePath(path) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", "", "", false, false, err
		}
		return path, path, "", false, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", "", false, false, err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", "", "", false, false, fmt.Errorf("single-file database path is a directory: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", "", false, false, err
	}
	storagePath, err = os.MkdirTemp("", "cqlite-db-*")
	if err != nil {
		return "", "", "", false, false, err
	}
	if err := unpackDatabaseFile(path, storagePath); err != nil {
		_ = os.RemoveAll(storagePath)
		return "", "", "", false, false, err
	}
	lockPath = path + ".locks"
	if err := os.MkdirAll(lockPath, 0o700); err != nil {
		_ = os.RemoveAll(storagePath)
		return "", "", "", false, false, err
	}
	return storagePath, lockPath, path, true, true, nil
}

func isSingleFileDatabasePath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".db")
}

func unpackDatabaseFile(path, dest string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if name == "." || name == ".." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("invalid database archive entry: %s", f.Name)
		}
		target := filepath.Join(dest, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		inErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if inErr != nil {
			return inErr
		}
	}
	return nil
}

func packDatabaseFile(src, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	walkErr := filepath.WalkDir(src, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, filePath)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		in, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, in)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeZipErr := zw.Close()
	closeFileErr := out.Close()
	if walkErr != nil {
		_ = os.Remove(tmp)
		return walkErr
	}
	if closeZipErr != nil {
		_ = os.Remove(tmp)
		return closeZipErr
	}
	if closeFileErr != nil {
		_ = os.Remove(tmp)
		return closeFileErr
	}
	return os.Rename(tmp, path)
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
	if err := d.refreshIfWALChanged(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	writes := d.state.clone()
	baseApplied := d.applied
	d.mu.Unlock()
	return &Tx{db: d, tx: d.txmgr.Begin(opts.Isolation), writes: writes, baseApplied: baseApplied}, nil
}

// acquireWriteLock waits for a cross-process writer to finish instead of
// surfacing a transient lock collision to callers. A positive BusyTimeout
// bounds the wait; the default is unlimited and remains cancellable through
// ctx. The local writer gate serializes writers within this process.
func (d *DB) acquireWriteLock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	err := acquireDatabaseWriteLock(d.lock)
	if err == nil || !errors.Is(err, ErrDatabaseLocked) {
		return err
	}
	var deadline <-chan time.Time
	if d.opts.BusyTimeout > 0 {
		timer := time.NewTimer(d.opts.BusyTimeout)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return err
		case <-ticker.C:
			if err := acquireDatabaseWriteLock(d.lock); err == nil || !errors.Is(err, ErrDatabaseLocked) {
				return err
			}
		}
	}
}

// acquireWriteGate serializes local writers without holding the state mutex
// while they wait for another handle or process to finish its commit.
func (d *DB) acquireWriteGate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.writeGate:
		if err := ctx.Err(); err != nil {
			d.releaseWriteGate()
			return err
		}
		return nil
	}
}

func (d *DB) releaseWriteGate() { d.writeGate <- struct{}{} }

func (d *DB) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	if res, ok, err := d.execAttachStatement(ctx, query, args...); ok || err != nil {
		return res, err
	}
	ad, err := d.foreignTarget(query)
	if err != nil {
		return Result{}, err
	}
	if ad != nil {
		defer ad.mu.RUnlock()
		// The schema qualifier has already been routed; the target database
		// executes the statement as its own.
		return ad.db.execLocal(ctx, query, args...)
	}
	return d.execLocal(ctx, query, args...)
}

// execLocal runs a statement against this database without schema routing. It
// is used for statements already dispatched to an attached database, whose own
// qualifier must not be re-interpreted.
func (d *DB) execLocal(ctx context.Context, query string, args ...any) (Result, error) {
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
	first, rest := nextSQLField(q)
	switch {
	case strings.EqualFold(first, "begin"):
		second, _ := nextSQLField(rest)
		if second == "" {
			return txCommand{kind: "begin"}, true
		}
		switch strings.ToLower(second) {
		case "transaction", "deferred", "immediate", "exclusive":
			return txCommand{kind: "begin"}, true
		default:
			return txCommand{}, false
		}
	case strings.EqualFold(first, "start"):
		second, _ := nextSQLField(rest)
		if strings.EqualFold(second, "transaction") {
			return txCommand{kind: "begin"}, true
		}
	case strings.EqualFold(first, "commit"), strings.EqualFold(first, "end"):
		return txCommand{kind: "commit"}, true
	case strings.EqualFold(first, "rollback"):
		second, rest := nextSQLField(rest)
		if strings.EqualFold(second, "to") {
			if savepoint, afterSavepoint := nextSQLField(rest); strings.EqualFold(savepoint, "savepoint") {
				rest = afterSavepoint
			}
			name, _ := lastSQLField(rest)
			return txCommand{kind: "rollback_to", name: cleanTxName(name)}, true
		}
		return txCommand{kind: "rollback"}, true
	case strings.EqualFold(first, "savepoint"):
		name, _ := nextSQLField(rest)
		if name != "" {
			return txCommand{kind: "savepoint", name: cleanTxName(name)}, true
		}
		return txCommand{kind: "savepoint"}, true
	case strings.EqualFold(first, "release"):
		name, rest := nextSQLField(rest)
		if strings.EqualFold(name, "savepoint") {
			name, _ = nextSQLField(rest)
		}
		if name != "" {
			return txCommand{kind: "release", name: cleanTxName(name)}, true
		}
		return txCommand{kind: "release"}, true
	}
	return txCommand{}, false
}

func nextSQLField(s string) (string, string) {
	s = strings.TrimLeft(s, " \t\n\r")
	if s == "" {
		return "", ""
	}
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			return s[:i], s[i+1:]
		}
		i++
	}
	return s, ""
}

func lastSQLField(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	end := len(s)
	for end > 0 {
		switch s[end-1] {
		case ' ', '\t', '\n', '\r':
			return s[end:], s[:end-1]
		}
		end--
	}
	return s, ""
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
	if res, ok, err := d.execSimpleDirect(ctx, query, args...); ok || err != nil {
		return res, ok, err
	}
	type directExecution struct {
		result  Result
		handled bool
		err     error
	}
	parsed, parseErr := withQueryStatement(stripStrictOnly(query), func(stmt ast.Statement) (directExecution, error) {
		switch stmt.(type) {
		case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt:
		default:
			return directExecution{}, nil
		}
		if err := d.acquireWriteGate(ctx); err != nil {
			return directExecution{handled: true, err: err}, nil
		}
		defer d.releaseWriteGate()
		if err := d.checkOpen(); err != nil {
			return directExecution{handled: true, err: err}, nil
		}
		if err := d.acquireWriteLock(ctx); err != nil {
			return directExecution{handled: true, err: err}, nil
		}
		defer releaseDatabaseWriteLock(d.lock)
		d.mu.Lock()
		defer d.mu.Unlock()
		if err := d.refreshWALLocked(); err != nil {
			return directExecution{handled: true, err: err}, nil
		}
		next := d.applied + 1
		d.state.ensureRuntime()
		before := d.state.clone()
		d.state.NextTx = next
		triggerScopeMu.RLock()
		res, fast, err := fastDirectDML(&d.state, stmt, args)
		triggerScopeMu.RUnlock()
		if err != nil {
			d.state = before
			return directExecution{handled: true, err: err}, nil
		}
		if !fast {
			res, err = execute(ctx, &d.state, query, args...)
			if err != nil {
				d.state = before
				return directExecution{handled: true, err: err}, nil
			}
		}
		if err := d.markDirty(); err != nil {
			d.state = before
			return directExecution{handled: true, err: err}, nil
		}
		if err := d.appendLogical(next, query, args); err != nil {
			d.state = before
			return directExecution{handled: true, err: err}, nil
		}
		d.applied = next
		if err := d.maybeCheckpointLocked(ctx); err != nil {
			return directExecution{handled: true, err: err}, nil
		}
		return directExecution{result: res, handled: true}, nil
	})
	if parseErr != nil {
		return Result{}, false, nil
	}
	return parsed.result, parsed.handled, parsed.err
}

func (d *DB) execSimpleDirect(ctx context.Context, query string, args ...any) (Result, bool, error) {
	if err := d.acquireWriteGate(ctx); err != nil {
		return Result{}, true, err
	}
	defer d.releaseWriteGate()
	if err := d.checkOpen(); err != nil {
		return Result{}, true, err
	}
	if err := d.acquireWriteLock(ctx); err != nil {
		return Result{}, true, err
	}
	defer releaseDatabaseWriteLock(d.lock)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.refreshWALLocked(); err != nil {
		return Result{}, true, err
	}
	next := d.applied + 1
	d.state.ensureRuntime()
	plan, planOK := simplePlan(query)
	useFullRollback := planOK && plan.kind == 'd' && d.state.Pragmas.ForeignKeys
	var before persistedState
	if useFullRollback {
		before = d.state.clone()
	}
	rollback := newSimpleDirectRollback(&d.state)
	if planOK {
		rollback.snapshotTable(plan.table)
		if plan.kind != 'i' {
			rollback.snapshotAllTables()
		}
	}
	d.state.NextTx = next
	res, fast, err := fastSimpleDMLWithPlan(&d.state, query, args, rollback.recordRow, plan, planOK)
	if err != nil {
		if useFullRollback {
			d.state = before
		} else {
			rollback.restore()
		}
		rollback.release()
		return Result{}, true, err
	}
	if !fast {
		if useFullRollback {
			d.state = before
		} else {
			rollback.restore()
		}
		rollback.release()
		return Result{}, false, nil
	}
	if err := d.markDirty(); err != nil {
		if useFullRollback {
			d.state = before
		} else {
			rollback.restore()
		}
		rollback.release()
		return Result{}, true, err
	}
	var appendErr error
	if planOK {
		appendErr = d.appendLogicalPlan(next, &d.state, plan, args)
	} else {
		appendErr = d.appendLogical(next, query, args)
	}
	if appendErr != nil {
		if useFullRollback {
			d.state = before
		} else {
			rollback.restore()
		}
		rollback.release()
		return Result{}, true, appendErr
	}
	rollback.release()
	d.applied = next
	if err := d.maybeCheckpointLocked(context.Background()); err != nil {
		return Result{}, true, err
	}
	return res, true, nil
}

type simpleDirectRollback struct {
	state   *persistedState
	nextTx  uint64
	tables  map[string]tableRollback
	changes []rowRollback
}

type tableRollback struct {
	exists       bool
	rowsLen      int
	pendingLen   int
	pendingRowID map[int64]struct{}
	pendingLast  int64
	pendingAsc   bool
	baseRows     int
	deferRuntime bool
	rowsShared   bool
	lazyRecords  []pager.Record
	lazyPath     string
	lazyPageSize int
	lazyCount    int
	nextRowID    int64
}

type rowRollback struct {
	table       string
	pos         int
	row         rowVersion
	columnIndex int
	columnName  string
	oldValue    any
}

var rollbackPool = sync.Pool{
	New: func() interface{} {
		return &simpleDirectRollback{
			tables: make(map[string]tableRollback),
		}
	},
}

func newSimpleDirectRollback(state *persistedState) *simpleDirectRollback {
	r := rollbackPool.Get().(*simpleDirectRollback)
	r.state = state
	r.nextTx = state.NextTx
	r.changes = r.changes[:0]
	for k := range r.tables {
		delete(r.tables, k)
	}
	return r
}

func (r *simpleDirectRollback) snapshotAllTables() {
	if r == nil || r.state == nil {
		return
	}
	for name := range r.state.Tables {
		r.snapshotTable(name)
	}
}

func (r *simpleDirectRollback) snapshotTable(name string) {
	if r == nil || r.state == nil {
		return
	}
	name = catalog.Normalize(name)
	if _, ok := r.tables[name]; ok {
		return
	}
	tbl := r.state.Catalog.Tables[name]
	data := r.state.Tables[name]
	snap := tableRollback{}
	if tbl != nil {
		snap.nextRowID = tbl.NextRowID
	}
	if data == nil {
		r.tables[name] = snap
		return
	}
	snap.exists = true
	snap.rowsLen = len(data.Rows)
	snap.pendingLen = len(data.Pending)
	if len(data.pendingRowID) > 0 {
		snap.pendingRowID = make(map[int64]struct{}, len(data.pendingRowID))
		for rowid := range data.pendingRowID {
			snap.pendingRowID[rowid] = struct{}{}
		}
	}
	snap.pendingLast = data.pendingLast
	snap.pendingAsc = data.pendingAsc
	snap.baseRows = data.baseRows
	snap.deferRuntime = data.deferRuntime
	snap.rowsShared = data.rowsShared
	if len(data.lazyRecords) > 0 {
		snap.lazyRecords = append([]pager.Record(nil), data.lazyRecords...)
	}
	snap.lazyPath = data.lazyPath
	snap.lazyPageSize = data.lazyPageSize
	snap.lazyCount = data.lazyCount
	r.tables[name] = snap
}

func (r *simpleDirectRollback) recordRow(table string, data *tableData, pos int, columnIndex int, columnName string, oldValue any) {
	if r == nil || data == nil || pos < 0 || pos >= len(data.Rows) {
		return
	}
	table = catalog.Normalize(table)
	r.snapshotTable(table)
	row := data.Rows[pos]
	r.changes = append(r.changes, rowRollback{
		table:       table,
		pos:         pos,
		row:         row,
		columnIndex: columnIndex,
		columnName:  columnName,
		oldValue:    oldValue,
	})
}

func (r *simpleDirectRollback) restore() {
	if r == nil || r.state == nil {
		return
	}
	r.state.NextTx = r.nextTx
	for name, snap := range r.tables {
		tbl := r.state.Catalog.Tables[name]
		if tbl != nil {
			tbl.NextRowID = snap.nextRowID
		}
		if !snap.exists {
			delete(r.state.Tables, name)
			continue
		}
		data := r.state.Tables[name]
		if data == nil {
			data = &tableData{}
			r.state.Tables[name] = data
		}
		if len(data.Rows) > snap.rowsLen {
			data.Rows = data.Rows[:snap.rowsLen]
		}
		if len(data.Pending) > snap.pendingLen {
			data.Pending = data.Pending[:snap.pendingLen]
		} else {
			data.Pending = nil
		}
		data.pendingRowID = snap.pendingRowID
		data.pendingLast = snap.pendingLast
		data.pendingAsc = snap.pendingAsc
		data.baseRows = snap.baseRows
		data.deferRuntime = snap.deferRuntime
		data.rowsShared = snap.rowsShared
		data.lazyRecords = snap.lazyRecords
		data.lazyPath = snap.lazyPath
		data.lazyPageSize = snap.lazyPageSize
		data.lazyCount = snap.lazyCount
		data.rowids = nil
		data.indexes = nil
		data.deferRuntime = true
	}
	for i := len(r.changes) - 1; i >= 0; i-- {
		change := r.changes[i]
		data := r.state.Tables[change.table]
		if data == nil || change.pos < 0 || change.pos >= len(data.Rows) {
			continue
		}
		if change.columnIndex >= 0 {
			row := &data.Rows[change.pos]
			if change.columnIndex < len(row.Pos) {
				row.Pos[change.columnIndex] = change.oldValue
			}
			if row.Values != nil {
				row.Values[change.columnName] = change.oldValue
			}
		} else {
			data.Rows[change.pos] = change.row
		}
		data.rowids = nil
		data.indexes = nil
		data.deferRuntime = true
	}
}

func (r *simpleDirectRollback) release() {
	if r == nil {
		return
	}
	r.state = nil
	r.changes = r.changes[:0]
	for k := range r.tables {
		delete(r.tables, k)
	}
	rollbackPool.Put(r)
}

func (d *DB) appendLogical(txid uint64, query string, args []any) error {
	return d.appendLogicalState(txid, &d.state, query, args)
}

func (d *DB) appendWALRecord(rec wal.Record) error {
	if err := d.wal.Append(rec, d.walSyncEnabled()); err != nil {
		return err
	}
	d.walSize += wal.RecordSize(rec)
	return nil
}

func (d *DB) walSyncEnabled() bool {
	level := strings.ToUpper(strings.TrimSpace(d.state.Pragmas.Synchronous))
	if level == "" {
		level = strings.ToUpper(strings.TrimSpace(d.opts.Synchronous))
	}
	switch level {
	case "OFF", "0", "NORMAL", "1":
		return false
	case "FULL", "2", "EXTRA", "3":
		return true
	default:
		return d.opts.SyncPolicy == SyncFull
	}
}

func (d *DB) appendLogicalState(txid uint64, state *persistedState, query string, args []any) error {
	if plan, ok := simplePlan(query); ok {
		return d.appendLogicalPlan(txid, state, plan, args)
	}
	data, err := encodeStateWAL(state)
	if err != nil {
		return err
	}
	return d.appendWALRecord(wal.Record{Type: wal.RecordCommit, TxID: txid, Data: data})
}

func (d *DB) appendLogicalPlan(txid uint64, state *persistedState, plan simpleDMLPlan, args []any) error {
	if data, ok := encodeSimpleWALPlan(plan, args); ok {
		err := d.appendWALRecord(wal.Record{Type: wal.RecordCommit, TxID: txid, Data: data})
		releaseWALBuf(&data)
		return err
	}
	data, err := encodeStateWAL(state)
	if err != nil {
		return err
	}
	return d.appendWALRecord(wal.Record{Type: wal.RecordCommit, TxID: txid, Data: data})
}

func encodeStateWAL(s *persistedState) ([]byte, error) {
	if s == nil {
		return nil, errors.New("missing state")
	}
	s.materializePending()
	out := []byte{stateWALMagic}
	catalogData, err := json.Marshal(s.Catalog)
	if err != nil {
		return nil, err
	}
	out = appendBytes32(out, catalogData)
	out = binary.LittleEndian.AppendUint64(out, s.NextTx)
	pragmaData, err := json.Marshal(s.Pragmas)
	if err != nil {
		return nil, err
	}
	out = appendBytes32(out, pragmaData)
	tableNames := make([]string, 0, len(s.Tables))
	for name, data := range s.Tables {
		if data != nil && s.Catalog.Tables[name] != nil {
			tableNames = append(tableNames, name)
		}
	}
	if uint64(len(tableNames)) > uint64(math.MaxUint32) {
		return nil, errors.New("too many tables for state WAL")
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(tableNames)))
	for _, name := range tableNames {
		data := s.Tables[name]
		table := s.Catalog.Tables[name]
		if err := data.materializeLazy(table); err != nil {
			return nil, err
		}
		var ok bool
		out, ok = appendString16OK(out, name)
		if !ok {
			return nil, errors.New("table name too long for state WAL")
		}
		if uint64(len(data.Rows)) > uint64(math.MaxUint32) {
			return nil, errors.New("too many rows for state WAL")
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(len(data.Rows)))
		for _, row := range data.Rows {
			rowData, err := encodeRowVersion(table, row)
			if err != nil {
				return nil, err
			}
			out = appendBytes32(out, rowData)
		}
	}
	return out, nil
}

func decodeStateWAL(data []byte) (persistedState, bool, error) {
	if len(data) == 0 || data[0] != stateWALMagic {
		return persistedState{}, false, nil
	}
	data = data[1:]
	var catalogData []byte
	var err error
	catalogData, data, err = readBytes32(data)
	if err != nil {
		return persistedState{}, true, err
	}
	state := newState()
	if err := json.Unmarshal(catalogData, &state.Catalog); err != nil {
		return persistedState{}, true, err
	}
	if len(data) < 8 {
		return persistedState{}, true, errors.New("truncated state WAL next tx")
	}
	state.NextTx = binary.LittleEndian.Uint64(data[:8])
	data = data[8:]
	var pragmaData []byte
	pragmaData, data, err = readBytes32(data)
	if err != nil {
		return persistedState{}, true, err
	}
	if err := json.Unmarshal(pragmaData, &state.Pragmas); err != nil {
		return persistedState{}, true, err
	}
	if len(data) < 4 {
		return persistedState{}, true, errors.New("truncated state WAL table count")
	}
	n := int(binary.LittleEndian.Uint32(data[:4]))
	data = data[4:]
	state.Tables = make(map[string]*tableData, n)
	for i := 0; i < n; i++ {
		name, rest, err := readString16(data)
		if err != nil {
			return persistedState{}, true, err
		}
		data = rest
		if len(data) < 4 {
			return persistedState{}, true, errors.New("truncated state WAL row count")
		}
		rows := int(binary.LittleEndian.Uint32(data[:4]))
		data = data[4:]
		table := state.Catalog.Tables[name]
		if table == nil {
			return persistedState{}, true, fmt.Errorf("state WAL references unknown table: %s", name)
		}
		td := &tableData{Rows: make([]rowVersion, 0, rows)}
		for j := 0; j < rows; j++ {
			var rowData []byte
			rowData, data, err = readBytes32(data)
			if err != nil {
				return persistedState{}, true, err
			}
			row, err := decodeRowVersion(table, rowData)
			if err != nil {
				return persistedState{}, true, err
			}
			td.Rows = append(td.Rows, row)
		}
		state.Tables[name] = td
	}
	if len(data) != 0 {
		return persistedState{}, true, errors.New("trailing state WAL bytes")
	}
	return state, true, nil
}

var walBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 128)
		return &b
	},
}

func encodeSimpleWAL(query string, args []any) ([]byte, bool) {
	plan, ok := simplePlan(query)
	if !ok || plan.params != len(args) || len(args) > math.MaxUint16 {
		return nil, false
	}
	return encodeSimpleWALPlan(plan, args)
}

func encodeSimpleWALPlan(plan simpleDMLPlan, args []any) ([]byte, bool) {
	bp := walBufPool.Get().(*[]byte)
	*bp = (*bp)[:0]
	data, ok := appendSimpleWALPlan(*bp, plan, args)
	if !ok {
		walBufPool.Put(bp)
		return nil, false
	}
	// append may have grown the backing array. Keep the actual encoded slice
	// attached to its pool entry so releaseWALBuf can recycle it.
	*bp = data
	return data, true
}

func releaseWALBuf(data *[]byte) {
	if data == nil {
		return
	}
	if cap(*data) <= 64<<10 {
		*data = (*data)[:0]
		walBufPool.Put(data)
	}
}

func appendSimpleWALPlan(data []byte, plan simpleDMLPlan, args []any) ([]byte, bool) {
	data = append(data, simpleWALMagic, plan.kind)
	var ok bool
	data, ok = appendString16OK(data, plan.table)
	if !ok {
		return nil, false
	}
	if plan.kind == 'd' {
		data, ok = appendString16OK(data, plan.where)
		if !ok {
			return nil, false
		}
	}
	if plan.kind == 'u' {
		data, ok = appendString16OK(data, plan.col)
		if !ok {
			return nil, false
		}
		data, ok = appendString16OK(data, plan.where)
		if !ok {
			return nil, false
		}
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

func decodeSimpleWALRecord(data []byte) (simpleWALRecord, bool, error) {
	if len(data) < 2 || data[0] != simpleWALMagic {
		return simpleWALRecord{}, false, nil
	}
	kind := data[1]
	rest := data[2:]
	table, rest, err := readString16(rest)
	if err != nil {
		return simpleWALRecord{}, true, err
	}
	plan := simpleDMLPlan{kind: kind, table: table}
	switch kind {
	case 'i':
	case 'd':
		plan.where, rest, err = readString16(rest)
		if err != nil {
			return simpleWALRecord{}, true, err
		}
	case 'u':
		plan.col, rest, err = readString16(rest)
		if err != nil {
			return simpleWALRecord{}, true, err
		}
		plan.where, rest, err = readString16(rest)
		if err != nil {
			return simpleWALRecord{}, true, err
		}
		if len(rest) < 8 {
			return simpleWALRecord{}, true, errors.New("truncated simple WAL update increment")
		}
		plan.inc = int64(binary.LittleEndian.Uint64(rest[:8]))
		rest = rest[8:]
	default:
		return simpleWALRecord{}, true, errors.New("unknown simple WAL record")
	}
	if len(rest) < 2 {
		return simpleWALRecord{}, true, errors.New("truncated simple WAL argument count")
	}
	n := int(binary.LittleEndian.Uint16(rest[:2]))
	rest = rest[2:]
	args := make([]any, 0, n)
	for i := 0; i < n; i++ {
		var v any
		v, rest, err = readWALValue(rest)
		if err != nil {
			return simpleWALRecord{}, true, err
		}
		args = append(args, v)
	}
	if len(rest) != 0 {
		return simpleWALRecord{}, true, errors.New("trailing simple WAL bytes")
	}
	plan.params = len(args)
	return simpleWALRecord{plan: plan, args: args}, true, nil
}

func applySimpleWALRecord(s *persistedState, rec simpleWALRecord) error {
	var (
		res  Result
		fast bool
		err  error
	)
	switch rec.plan.kind {
	case 'i':
		res, fast, err = fastInsertValues(s, rec.plan.table, rec.args)
	case 'u':
		s.ensureRuntime()
		res, fast, err = fastIncrementUpdate(s, simpleIncrementUpdate{
			table: rec.plan.table,
			col:   rec.plan.col,
			where: rec.plan.where,
			inc:   rec.plan.inc,
		}, rec.args, nil)
	case 'd':
		s.ensureRuntime()
		res, fast, err = fastDeleteWhere(s, rec.plan.table, rec.plan.where, rec.args, nil)
	default:
		return errors.New("unknown simple WAL mutation")
	}
	_ = res
	if err != nil {
		return err
	}
	if !fast {
		if rec.plan.kind == 'u' {
			if table, ok := s.Catalog.Table(rec.plan.table); ok && integerPrimaryKeyColumn(table) == rec.plan.where {
				query := fmt.Sprintf("UPDATE %s SET %s = %s + %d WHERE %s = ?",
					quoteWALIdentifier(rec.plan.table), quoteWALIdentifier(rec.plan.col),
					quoteWALIdentifier(rec.plan.col), rec.plan.inc, quoteWALIdentifier(rec.plan.where))
				if _, err := execute(context.Background(), s, query, rec.args...); err != nil {
					return fmt.Errorf("replay primary-key update for %s: %w", rec.plan.table, err)
				}
				return nil
			}
		}
		return fmt.Errorf("could not replay simple WAL mutation (kind %q, table %q, %d arguments)", rec.plan.kind, rec.plan.table, len(rec.args))
	}
	return nil
}

func quoteWALIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func appendBytes32(dst []byte, data []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(data)))
	return append(dst, data...)
}

func readBytes32(data []byte) ([]byte, []byte, error) {
	if len(data) < 4 {
		return nil, nil, errors.New("truncated length-prefixed payload")
	}
	n := int(binary.LittleEndian.Uint32(data[:4]))
	data = data[4:]
	if len(data) < n {
		return nil, nil, errors.New("truncated length-prefixed payload bytes")
	}
	return data[:n], data[n:], nil
}

func appendString16OK(dst []byte, s string) ([]byte, bool) {
	if len(s) > math.MaxUint16 {
		return nil, false
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(s)))
	return append(dst, s...), true
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
		if uint64(len(x)) > uint64(math.MaxUint32) {
			return nil, false
		}
		dst = append(dst, 3)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(x)))
		return append(dst, x...), true
	case []byte:
		if x == nil {
			return append(dst, 0), true
		}
		if uint64(len(x)) > uint64(math.MaxUint32) {
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

func encodeTxWAL(tx *Tx) ([]byte, error) {
	if len(tx.ops) == 0 && len(tx.pendingInserts) > 0 {
		if data, ok := encodePendingInsertTransactionWAL(tx); ok {
			return data, nil
		}
	}
	tx.flushPendingInsertLogs()
	if data, ok := encodeSimpleTransactionWAL(tx.ops); ok {
		return data, nil
	}
	return encodeStateWAL(&tx.writes)
}

func encodePendingInsertTransactionWAL(tx *Tx) ([]byte, bool) {
	count := 0
	for _, run := range tx.pendingInserts {
		count += run.count
	}
	if count == 0 || uint64(count) > math.MaxUint32 {
		return nil, false
	}
	if len(tx.pendingInserts) > math.MaxUint16 {
		return nil, false
	}
	// Reserve the encoded size once. Without this pass, a large transaction
	// repeatedly grows and recopies the WAL buffer while holding the writer
	// lock. Values in Pending are already normalized SQLite storage classes.
	capacity := 1 + 4 + 2
	maxInt := int(^uint(0) >> 1)
	addCapacity := func(size int) bool {
		if size < 0 || size > maxInt-capacity {
			return false
		}
		capacity += size
		return true
	}
	for _, run := range tx.pendingInserts {
		if run.table == "" || len(run.table) > math.MaxUint16 || run.count < 0 {
			return nil, false
		}
		table := tx.writes.Catalog.Tables[run.table]
		data := tx.writes.Tables[run.table]
		if table == nil || data == nil || run.index < 0 || run.count <= 0 || run.index+run.count > len(data.Pending) {
			return nil, false
		}
		if !addCapacity(2 + len(run.table) + 2 + 4) {
			return nil, false
		}
		for i := run.index; i < run.index+run.count; i++ {
			values := (&data.Pending[i]).positionedValues()
			if len(values) != len(table.Columns) {
				return nil, false
			}
			for _, value := range values {
				size, ok := walValueSize(value)
				if !ok || !addCapacity(size) {
					return nil, false
				}
			}
		}
	}
	out := make([]byte, 0, capacity)
	out = append(out, txSimpleInsertBatchWALMagic)
	out = binary.LittleEndian.AppendUint32(out, uint32(count))
	out = binary.LittleEndian.AppendUint16(out, uint16(len(tx.pendingInserts)))
	for _, run := range tx.pendingInserts {
		table := tx.writes.Catalog.Tables[run.table]
		data := tx.writes.Tables[run.table]
		if table == nil || data == nil || run.index < 0 || run.count <= 0 || run.index+run.count > len(data.Pending) || len(table.Columns) > math.MaxUint16 {
			return nil, false
		}
		var ok bool
		out, ok = appendString16OK(out, table.Name)
		if !ok {
			return nil, false
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(len(table.Columns)))
		out = binary.LittleEndian.AppendUint32(out, uint32(run.count))
		for i := run.index; i < run.index+run.count; i++ {
			values := (&data.Pending[i]).positionedValues()
			if len(values) != len(table.Columns) {
				return nil, false
			}
			for _, value := range values {
				out, ok = appendWALValue(out, value)
				if !ok {
					return nil, false
				}
			}
		}
	}
	return out, true
}

func walValueSize(value any) (int, bool) {
	switch v := value.(type) {
	case nil:
		return 1, true
	case int, int64, float64:
		return 9, true
	case bool:
		return 2, true
	case string:
		if uint64(len(v)) > math.MaxUint32 {
			return 0, false
		}
		return 5 + len(v), true
	case []byte:
		if v == nil {
			return 1, true
		}
		if uint64(len(v)) > math.MaxUint32 {
			return 0, false
		}
		return 5 + len(v), true
	default:
		return 0, false
	}
}

func encodeSimpleTransactionWAL(ops []logicalRecord) ([]byte, bool) {
	if len(ops) == 0 || uint64(len(ops)) > uint64(math.MaxUint32) {
		return nil, false
	}
	out := []byte{txSimpleWALMagic}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(ops)))
	for _, op := range ops {
		plan, planOK := simplePlan(op.SQL)
		if !planOK || plan.params != len(op.Args) {
			return nil, false
		}
		lengthAt := len(out)
		out = binary.LittleEndian.AppendUint32(out, 0)
		start := len(out)
		var ok bool
		out, ok = appendSimpleWALPlan(out, plan, op.Args)
		if !ok || uint64(len(out)-start) > uint64(math.MaxUint32) {
			return nil, false
		}
		binary.LittleEndian.PutUint32(out[lengthAt:lengthAt+4], uint32(len(out)-start))
	}
	return out, true
}

func decodeSimpleTransactionWAL(data []byte) ([]simpleWALRecord, bool, error) {
	if len(data) > 0 && data[0] == txSimpleInsertBatchWALMagic {
		return decodeSimpleInsertBatchTransactionWAL(data[1:])
	}
	if len(data) == 0 || data[0] != txSimpleWALMagic {
		return nil, false, nil
	}
	data = data[1:]
	if len(data) < 4 {
		return nil, true, errors.New("truncated simple transaction WAL count")
	}
	n64 := uint64(binary.LittleEndian.Uint32(data[:4]))
	data = data[4:]
	if n64 > uint64(len(data))/4 {
		return nil, true, errors.New("invalid simple transaction WAL count")
	}
	n := int(n64)
	records := make([]simpleWALRecord, 0, n)
	for i := 0; i < n; i++ {
		if len(data) < 4 {
			return nil, true, errors.New("truncated simple transaction WAL record length")
		}
		size64 := uint64(binary.LittleEndian.Uint32(data[:4]))
		data = data[4:]
		if size64 > uint64(len(data)) {
			return nil, true, errors.New("truncated simple transaction WAL record")
		}
		size := int(size64)
		rec, ok, err := decodeSimpleWALRecord(data[:size])
		if err != nil {
			return nil, true, err
		}
		if !ok {
			return nil, true, errors.New("invalid simple transaction WAL record")
		}
		records = append(records, rec)
		data = data[size:]
	}
	if len(data) != 0 {
		return nil, true, errors.New("trailing simple transaction WAL bytes")
	}
	return records, true, nil
}

func decodeSimpleInsertBatchTransactionWAL(data []byte) ([]simpleWALRecord, bool, error) {
	if len(data) < 6 {
		return nil, true, errors.New("truncated simple insert batch WAL header")
	}
	total64 := uint64(binary.LittleEndian.Uint32(data[:4]))
	runs64 := uint64(binary.LittleEndian.Uint16(data[4:6]))
	data = data[6:]
	if total64 == 0 || runs64 == 0 || runs64 > total64 || total64 > uint64(len(data)) {
		return nil, true, errors.New("invalid simple insert batch WAL counts")
	}
	total, runs := int(total64), int(runs64)
	records := make([]simpleWALRecord, 0, total)
	for runIndex := 0; runIndex < runs; runIndex++ {
		table, rest, err := readString16(data)
		if err != nil {
			return nil, true, err
		}
		data = rest
		if len(data) < 6 {
			return nil, true, errors.New("truncated simple insert batch WAL run")
		}
		columns := int(binary.LittleEndian.Uint16(data[:2]))
		rows64 := uint64(binary.LittleEndian.Uint32(data[2:6]))
		data = data[6:]
		if columns == 0 || rows64 == 0 || rows64 > uint64(total-len(records)) {
			return nil, true, errors.New("invalid simple insert batch WAL run counts")
		}
		rows := int(rows64)
		plan := simpleDMLPlan{kind: 'i', table: table, params: columns}
		for rowIndex := 0; rowIndex < rows; rowIndex++ {
			args := make([]any, 0, columns)
			for columnIndex := 0; columnIndex < columns; columnIndex++ {
				value, remaining, err := readWALValue(data)
				if err != nil {
					return nil, true, err
				}
				args = append(args, value)
				data = remaining
			}
			records = append(records, simpleWALRecord{plan: plan, args: args})
		}
	}
	if len(records) != total || len(data) != 0 {
		return nil, true, errors.New("invalid simple insert batch WAL payload")
	}
	return records, true, nil
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
		v := cloneSQLiteBlob(data[:n])
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

func (d *DB) cleanCheckpointMarkerPath() string {
	return filepath.Join(d.path, "db.clean")
}

func (d *DB) hasCleanCheckpointMarker() (bool, error) {
	if d.applied == 0 || d.applied != d.checkpointed {
		return false, nil
	}
	data, err := os.ReadFile(d.cleanCheckpointMarkerPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	fields := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
	if len(fields) != 2 {
		return false, nil
	}
	txid, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return false, nil
	}
	walSize, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return false, nil
	}
	currentWALSize, err := d.wal.Size()
	if err != nil {
		return false, err
	}
	return txid == d.applied && walSize == currentWALSize, nil
}

func (d *DB) writeCleanCheckpointMarker(applied uint64) error {
	tmp := d.cleanCheckpointMarkerPath() + ".tmp"
	walSize, err := d.wal.Size()
	if err != nil {
		return err
	}
	data := []byte(strconv.FormatUint(applied, 10) + ":" + strconv.FormatInt(walSize, 10))
	if err := writeFileSynced(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.cleanCheckpointMarkerPath()); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(d.cleanCheckpointMarkerPath())); err != nil {
		return err
	}
	d.clean = true
	return nil
}

func (d *DB) markDirty() error {
	if !d.clean {
		return nil
	}
	err := os.Remove(d.cleanCheckpointMarkerPath())
	if err == nil || errors.Is(err, os.ErrNotExist) {
		d.clean = false
		return nil
	}
	return err
}

func writeFileSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if n, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	} else if n != len(data) {
		_ = f.Close()
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (d *DB) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if d.hasAttached.Load() {
		if stmt, err := parseOneUnpooled(query); err == nil {
			if selectStmt, ok := stmt.(*ast.SelectStmt); ok {
				schemas := selectSchemaNames(selectStmt)
				if len(schemas) > 1 {
					return d.queryAttachedSelect(ctx, query, args, schemas)
				}
			}
		}
	}
	ad, ferr := d.foreignTarget(query)
	if ferr != nil {
		return nil, ferr
	}
	if ad != nil {
		defer ad.mu.RUnlock()
		return ad.db.queryLocal(ctx, query, args...)
	}
	return d.queryLocal(ctx, query, args...)
}

// queryAttachedSelect evaluates a read-only SELECT over stable per-database
// snapshots. Each table is copied into an ephemeral schema-qualified catalog,
// so identically named tables in different files cannot alias accidentally.
func (d *DB) queryAttachedSelect(ctx context.Context, query string, args []any, schemas []string) (*Rows, error) {
	if err := d.checkOpen(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if err := d.refreshIfWALChanged(ctx); err != nil {
		return nil, err
	}
	mainState := d.snapshotForRead()
	view := newState()
	view.compositeRead = true
	addCompositeSchema(&view, "main", mainState, true)

	// Keep each attachment alive through snapshot creation. Detach removes the
	// name first, then waits on this read lock before closing its handle.
	d.attachedMu.RLock()
	attached := make(map[string]*attachedDB, len(schemas))
	for _, schema := range schemas {
		if schema == "main" {
			continue
		}
		ad := d.attached[schema]
		if ad == nil {
			d.attachedMu.RUnlock()
			return nil, fmt.Errorf("no such database: %s", schema)
		}
		ad.mu.RLock()
		attached[schema] = ad
	}
	d.attachedMu.RUnlock()
	defer func() {
		for _, ad := range attached {
			ad.mu.RUnlock()
		}
	}()
	for _, schema := range schemas {
		if schema == "main" {
			continue
		}
		ad := attached[schema]
		if err := ad.db.refreshIfWALChanged(ctx); err != nil {
			return nil, err
		}
		addCompositeSchema(&view, schema, ad.db.snapshotForRead(), false)
	}
	triggerScopeMu.RLock()
	defer triggerScopeMu.RUnlock()
	rows, err := queryRows(ctx, &view, query, args...)
	if err != nil {
		return nil, d.rewriteMissingQualifiedTableError(query, err)
	}
	return rows, nil
}

func (d *DB) snapshotForRead() persistedState {
	d.mu.Lock()
	s := d.state.clone()
	d.mu.Unlock()
	return s
}

// queryLocal runs a query against this database without schema routing.
func (d *DB) queryLocal(ctx context.Context, query string, args ...any) (*Rows, error) {
	if rows, ok, err := d.queryDatabaseList(query); ok {
		return rows, err
	}
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
	if err := d.refreshIfWALChanged(ctx); err != nil {
		return nil, err
	}
	for {
		d.mu.RLock()
		if !d.state.hasDeferredRuntime() || canQueryDeferredSimplePoint(&d.state, query, args) {
			break
		}
		d.mu.RUnlock()
		d.mu.Lock()
		d.state.ensureRuntime()
		d.mu.Unlock()
	}
	defer d.mu.RUnlock()
	triggerScopeMu.RLock()
	defer triggerScopeMu.RUnlock()
	stateView := d.state
	stateView.ctes = nil
	rows, err := queryRows(ctx, &stateView, query, args...)
	if err != nil {
		return nil, d.rewriteMissingQualifiedTableError(query, err)
	}
	return rows, nil
}

func (d *DB) rewriteMissingQualifiedTableError(query string, err error) error {
	if err == nil || !strings.HasPrefix(err.Error(), "table does not exist: ") {
		return err
	}
	schema, ok := statementSchemaOfQuery(query)
	if !ok || isReservedSchema(schema) {
		return err
	}
	if target := statementTargetName(query); target != "" {
		return fmt.Errorf("no such table: %s", target)
	}
	return err
}

// refreshIfWALChanged detects commits from another handle or process. A reader
// keeps its last committed snapshot while another writer owns the lock; replay
// runs once the writer is idle, so reads never fail just because a commit is in
// progress.
func (d *DB) refreshIfWALChanged(ctx context.Context) error {
	d.mu.RLock()
	size, err := d.wal.Size()
	stale := err == nil && size != d.walSize
	d.mu.RUnlock()
	if err != nil || !stale {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	size, err = d.wal.Size()
	if err != nil || size == d.walSize {
		return nil
	}
	if err := acquireDatabaseWriteLock(d.lock); err != nil {
		if errors.Is(err, ErrDatabaseLocked) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		}
		return err
	}
	defer releaseDatabaseWriteLock(d.lock)
	return d.refreshWALLocked()
}

func (d *DB) refreshWALLocked() error {
	size, err := d.wal.Size()
	if err != nil {
		return err
	}
	if size == d.walSize && !d.wal.HasIncompleteTail() {
		return nil
	}
	return d.replayWithRepair()
}

// queryDatabaseList answers PRAGMA database_list, which must see the
// connection's attached schemas rather than a single database's state.
func (d *DB) queryDatabaseList(query string) (*Rows, bool, error) {
	q := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	if !strings.EqualFold(q, "pragma database_list") {
		return nil, false, nil
	}
	var out [][]any
	for _, e := range d.databaseList() {
		out = append(out, []any{int64(len(out)), e[0], e[1]})
	}
	return isql.NewRows([]string{"seq", "name", "file"}, out), true, nil
}

// foreignTarget reports the attached database a query targets, or nil when the
// query belongs to this database. A statement naming a schema that is not
// attached is an error rather than a fallback to main, so a detached schema can
// never silently read another database's table of the same name.
func (d *DB) foreignTarget(query string) (*attachedDB, error) {
	if !d.hasAttached.Load() {
		return nil, nil
	}
	q := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	if q == "" {
		return nil, nil
	}
	// Most statements use unqualified names. Avoid allocating a parser arena
	// just to discover that there is no schema qualifier to route.
	if !strings.ContainsRune(q, '.') {
		return nil, nil
	}
	stmt, parseErr := parseOneUnpooled(q)
	if parseErr == nil && statementHasMultipleSchemas(stmt) {
		return nil, errors.New("cross-database statements spanning attached schemas are not supported")
	}
	schema, ok := statementSchemaOfQuery(q)
	if !ok {
		return nil, nil
	}
	if isReservedSchema(schema) {
		return nil, nil
	}
	d.attachedMu.RLock()
	ad := d.attached[catalog.Normalize(schema)]
	if ad != nil {
		ad.mu.RLock()
	}
	d.attachedMu.RUnlock()
	if ad == nil {
		// SQLite reports a detached schema as a missing table, not a missing
		// database, so match its wording.
		if target := statementTargetName(query); target != "" {
			return nil, fmt.Errorf("no such table: %s", target)
		}
		return nil, fmt.Errorf("no such database: %s", schema)
	}
	return ad, nil
}

func (d *DB) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	if err := d.acquireWriteGate(context.Background()); err != nil {
		return err
	}
	defer d.releaseWriteGate()
	if err := d.acquireWriteLock(context.Background()); err != nil {
		_ = d.wal.Close()
		_ = d.pager.Close()
		_ = releaseDatabaseLock(d.lock)
		return err
	}
	d.attachedMu.Lock()
	attached := d.attached
	d.attached = nil
	d.hasAttached.Store(false)
	d.attachedMu.Unlock()
	for _, ad := range attached {
		ad.mu.Lock()
		_ = ad.db.Close()
		ad.mu.Unlock()
	}
	d.explicitMu.Lock()
	if d.explicitTx != nil {
		_ = d.explicitTx.Rollback()
		d.explicitTx = nil
	}
	d.explicitMu.Unlock()
	d.mu.Lock()
	if d.applied != d.checkpointed {
		// Another process may have committed since this handle's last write.
		// Refresh under the writer lock before checkpointing, or this stale
		// snapshot could replace a newer clean-checkpoint marker.
		if err := d.refreshWALLocked(); err != nil {
			_ = releaseDatabaseWriteLock(d.lock)
			d.mu.Unlock()
			_ = d.wal.Close()
			_ = d.pager.Close()
			_ = releaseDatabaseLock(d.lock)
			return err
		}
		// Rows staged into Pending by the fast insert paths have to be
		// materialised before they can be written out as table pages.
		d.state.materializePending()
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
	if d.singleFile != "" {
		if perr := packDatabaseFile(d.path, d.singleFile); err == nil {
			err = perr
		}
	}
	if lerr := releaseDatabaseLock(d.lock); err == nil {
		err = lerr
	}
	if d.cleanupPath {
		if rerr := os.RemoveAll(d.path); err == nil {
			err = rerr
		}
	}
	if d.cleanupLocks {
		if rerr := os.RemoveAll(d.lockPath); err == nil {
			err = rerr
		}
	}
	if d.ephemeralRoot != "" {
		if rerr := os.RemoveAll(d.ephemeralRoot); err == nil {
			err = rerr
		}
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
			td, err := d.loadTablePagesLazy(name, schema, meta.Generation)
			if err != nil {
				return err
			}
			d.state.Tables[name] = td
			if err := d.verifyIndexPages(name, schema, meta.Generation); err != nil {
				return err
			}
		}
		d.state.deferRuntime()
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
	return d.replayWAL(false)
}

func (d *DB) replayWithRepair() error { return d.replayWAL(true) }

func (d *DB) replayWAL(repair bool) error {
	apply := func(rec wal.Record) error {
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
			if records, ok, err := decodeSimpleTransactionWAL(rec.Data); err != nil {
				return err
			} else if ok {
				d.state.NextTx = rec.TxID
				for _, simple := range records {
					if err := applySimpleWALRecord(&d.state, simple); err != nil {
						return err
					}
				}
				d.state.materializePending()
				d.applied = rec.TxID
				return nil
			}
			if state, ok, err := decodeStateWAL(rec.Data); err != nil {
				return err
			} else if ok {
				d.state = state
				d.state.rebuildRuntime()
				d.applied = rec.TxID
				return nil
			}
			if ops, ok, err := decodeTransactionWAL(rec.Data); err != nil {
				return err
			} else if ok {
				d.state.NextTx = rec.TxID
				for _, op := range ops {
					if _, err := execute(context.Background(), &d.state, op.SQL, op.Args...); err != nil {
						return err
					}
				}
				d.state.rebuildRuntime()
				d.applied = rec.TxID
				return nil
			}
			if simple, ok, err := decodeSimpleWALRecord(rec.Data); err != nil {
				return err
			} else if ok {
				d.state.NextTx = rec.TxID
				if err := applySimpleWALRecord(&d.state, simple); err != nil {
					return err
				}
				d.state.materializePending()
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
	}
	var replayErr error
	if repair {
		replayErr = d.wal.ReplayAndRepair(apply)
	} else {
		replayErr = d.wal.Replay(apply)
	}
	if replayErr != nil {
		return replayErr
	}
	d.walSize = d.wal.ReplaySize()
	return nil
}

func (d *DB) commit(ctx context.Context, tx *Tx) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := d.acquireWriteGate(ctx); err != nil {
		return err
	}
	defer d.releaseWriteGate()
	if err := d.checkOpen(); err != nil {
		return err
	}
	if err := d.acquireWriteLock(ctx); err != nil {
		return err
	}
	defer releaseDatabaseWriteLock(d.lock)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(); err != nil {
		return err
	}
	if len(tx.ops) == 0 && len(tx.pendingInserts) == 0 {
		return nil
	}
	if err := d.refreshWALLocked(); err != nil {
		return err
	}
	next := d.applied + 1
	pendingOnly, err := d.validatePendingOnly(tx, d.applied != tx.baseApplied)
	if err != nil {
		return err
	}
	if !pendingOnly && d.applied != tx.baseApplied {
		// Transactions execute against a snapshot. Replay intervening commits
		// under the writer lock, then apply this transaction to the latest state
		// so a stale snapshot cannot erase another writer's changes.
		latest := d.state.clone()
		for _, op := range tx.ops {
			if _, err := execute(context.Background(), &latest, op.SQL, op.Args...); err != nil {
				return err
			}
		}
		tx.writes = latest
	}
	tx.writes.NextTx = next
	data, err := encodeTxWAL(tx)
	if err != nil {
		return err
	}
	if err := d.markDirty(); err != nil {
		return err
	}
	if err := d.appendWALRecord(wal.Record{Type: wal.RecordCommit, TxID: next, Data: data}); err != nil {
		return err
	}
	if pendingOnly {
		d.applyPendingOnly(tx, next)
		d.applied = next
		if err := d.maybeCheckpointLocked(context.Background()); err != nil {
			return err
		}
		return nil
	}
	tx.writes.materializePending()
	d.state = tx.writes
	d.applied = next
	if err := d.maybeCheckpointLocked(context.Background()); err != nil {
		return err
	}
	return nil
}

func (d *DB) validatePendingOnly(tx *Tx, revalidateConflicts bool) (bool, error) {
	// Pending-only commits are encoded as a batch of physical table inserts.
	// Statements logged in tx.ops may have side effects outside those pending
	// rows (virtual tables, triggers, and other SQL paths), so they must use the
	// normal transaction commit path even when their shadow rows are pending.
	if len(tx.ops) != 0 {
		return false, nil
	}
	if tx.writes.Pragmas.SchemaVer != d.state.Pragmas.SchemaVer || len(tx.writes.Catalog.Tables) != len(d.state.Catalog.Tables) {
		return false, nil
	}
	hasPending := false
	for name, data := range tx.writes.Tables {
		if data == nil {
			continue
		}
		if !data.deferRuntime || !data.rowsShared || len(data.Rows) != data.baseRows {
			return false, nil
		}
		if len(data.Pending) > 0 {
			hasPending = true
		}
		if _, ok := d.state.Tables[name]; !ok {
			return false, nil
		}
		if _, ok := d.state.Catalog.Tables[name]; !ok {
			return false, nil
		}
	}
	if !hasPending {
		return false, nil
	}
	if revalidateConflicts {
		for name, data := range tx.writes.Tables {
			if len(data.Pending) == 0 {
				continue
			}
			liveData := d.state.Tables[name]
			liveTable := d.state.Catalog.Tables[name]
			if liveData == nil || liveTable == nil {
				return false, nil
			}
			for _, row := range data.Pending {
				if row.Values == nil {
					if err := validateIntegerPrimaryKeyFast(liveTable, liveData, row.RowID, integerPrimaryKeyColumn(liveTable)); err != nil {
						return false, err
					}
				} else {
					if err := validateInsertFast(&d.state, liveTable, liveData, row.Values, row.RowID); err != nil {
						return false, err
					}
				}
			}
		}
	}
	return true, nil
}

func (d *DB) applyPendingOnly(tx *Tx, next uint64) {
	d.state.NextTx = next
	d.state.Pragmas = tx.writes.Pragmas
	for name, data := range tx.writes.Tables {
		if len(data.Pending) == 0 {
			continue
		}
		liveData := d.state.Tables[name]
		liveTable := d.state.Catalog.Tables[name]
		txTable := tx.writes.Catalog.Tables[name]
		if len(liveTable.Indexes) == 0 && !liveData.deferRuntime && liveData.rowids != nil && liveData.indexes != nil {
			start := len(liveData.Rows)
			liveData.Rows = reserveRowsForAppend(liveData.Rows, len(data.Pending))
			liveData.Rows = append(liveData.Rows, data.Pending...)
			liveData.pendingCapHint = len(data.Pending)
			if liveData.pendingCapHint > 256 {
				liveData.pendingCapHint = 256
			}
			// The B-tree retains inserted byte slices; share one backing buffer
			// across this batch instead of allocating a key and value per row.
			indexBytes := make([]byte, len(data.Pending)*16)
			liveData.rowids.PutManyOwnedFunc(len(data.Pending), func(i int) ([]byte, []byte) {
				pos := start + i
				liveData.Rows[pos].Created = next
				rowID := liveData.Rows[pos].RowID
				base := i * 16
				key := indexBytes[base : base+8]
				value := indexBytes[base+8 : base+16]
				binary.BigEndian.PutUint64(key, uint64(rowID)^(uint64(1)<<63))
				binary.LittleEndian.PutUint64(value, uint64(pos))
				return key, value
			})
			liveTable.NextRowID = txTable.NextRowID
			continue
		}
		for _, row := range data.Pending {
			row.Created = next
			liveData.Rows = append(liveData.Rows, row)
			liveData.addRuntime(liveTable, len(liveData.Rows)-1)
		}
		liveTable.NextRowID = txTable.NextRowID
	}
}

// reserveRowsForAppend grows the live table's row slice geometrically before
// applying a committed insert batch. Transaction snapshots cap their own slice
// at the snapshot length, so the live table can safely use this extra capacity
// without exposing appended rows to older snapshots.
func reserveRowsForAppend(rows []rowVersion, appendCount int) []rowVersion {
	need := len(rows) + appendCount
	if need <= cap(rows) {
		return rows
	}
	capacity := cap(rows) * 2
	if capacity < need {
		capacity = need
	}
	if capacity < 8 {
		capacity = 8
	}
	grown := make([]rowVersion, len(rows), capacity)
	copy(grown, rows)
	return grown
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
	if err := d.acquireWriteGate(ctx); err != nil {
		return err
	}
	defer d.releaseWriteGate()
	if err := d.checkOpen(); err != nil {
		return err
	}
	if err := d.acquireWriteLock(ctx); err != nil {
		return err
	}
	defer releaseDatabaseWriteLock(d.lock)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.refreshWALLocked(); err != nil {
		return err
	}
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
		if d.wal.Written() >= d.opts.CheckpointIntervalBytes {
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
	return d.writeCleanCheckpointMarker(applied)
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
		if err := d.appendWALRecord(wal.Record{Type: wal.RecordPage, TxID: txid, Data: wal.EncodePageRecord(frame)}); err != nil {
			return err
		}
	}
	return nil
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
	return d.loadTablePagesWithOptions(name, schema, generation, true)
}

func (d *DB) loadTablePagesLazy(name string, schema *catalog.Table, generation uint64) (*tableData, error) {
	return d.loadTablePagesWithOptions(name, schema, generation, false)
}

func (d *DB) loadTablePagesWithOptions(name string, schema *catalog.Table, generation uint64, includeValues bool) (*tableData, error) {
	pg, err := d.tablePagerFor(name, generation)
	if err != nil {
		return nil, err
	}
	defer pg.Close()
	data, err := pg.ReadBlob()
	if err != nil {
		if !includeValues && walHasRecords(filepath.Join(d.path, "db.wal")) {
			count, countErr := pg.ReadRecordCount()
			if countErr != nil {
				if errors.Is(err, os.ErrNotExist) {
					if generation != 0 {
						return nil, fmt.Errorf("missing checkpoint table pages for %s generation %d", name, generation)
					}
					return &tableData{}, nil
				}
				return nil, countErr
			}
			return &tableData{
				deferRuntime: true,
				baseRows:     count,
				lazyPath:     d.tablePagerPath(name, generation),
				lazyPageSize: d.opts.PageSize,
				lazyCount:    count,
			}, nil
		}
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
		if !includeValues {
			return &tableData{lazyRecords: records, deferRuntime: true, baseRows: len(records)}, nil
		}
		table := &tableData{Rows: make([]rowVersion, 0, len(records))}
		for _, rec := range records {
			var row rowVersion
			if includeValues {
				row, err = decodeRowVersion(schema, rec.Values)
			} else {
				row, err = decodeRowVersionLazy(schema, rec.Values)
			}
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
	db             *DB
	tx             *txn.Tx
	writes         persistedState
	baseApplied    uint64
	savepoints     []savepoint
	ops            []logicalRecord
	pendingInserts []pendingInsertRun
	closed         bool
}

type savepoint struct {
	name           string
	state          persistedState
	ops            int
	pendingInserts int
	pendingLast    int
}

type pendingInsertRun struct {
	table string
	index int
	count int
}

func (t *Tx) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	if err := t.check(); err != nil {
		return Result{}, err
	}
	if ok, err := t.execTransactionControl(query); ok || err != nil {
		return Result{}, err
	}
	plan, planOK := simplePlan(query)
	if planOK && plan.kind != 'i' {
		t.writes.ensureRuntime()
	}
	res, fast, fastErr := fastSimpleDMLWithPlan(&t.writes, query, args, nil, plan, planOK)
	if fast || fastErr != nil {
		if fastErr == nil {
			t.logOpWithPlan(query, args, plan, planOK)
		}
		return res, fastErr
	}
	type directResult struct {
		result Result
		fast   bool
		err    error
	}
	direct, parseErr := withQueryStatement(stripStrictOnly(query), func(stmt ast.Statement) (directResult, error) {
		if _, insert := stmt.(*ast.InsertStmt); !insert {
			t.writes.ensureRuntime()
		}
		triggerScopeMu.RLock()
		res, fast, err := fastDirectDML(&t.writes, stmt, args)
		triggerScopeMu.RUnlock()
		return directResult{res, fast, err}, nil
	})
	if parseErr == nil && (direct.fast || direct.err != nil) {
		if direct.err == nil {
			t.logOp(query, args)
		}
		return direct.result, direct.err
	}
	res, err := execute(ctx, &t.writes, query, args...)
	if err == nil {
		t.logOp(query, args)
	}
	return res, err
}

func (t *Tx) logOp(query string, args []any) {
	plan, planOK := simplePlan(query)
	t.logOpWithPlan(query, args, plan, planOK)
}

func (t *Tx) logOpWithPlan(query string, args []any, plan simpleDMLPlan, planOK bool) {
	if ref, ok := t.pendingInsertRef(plan, planOK, len(args)); ok && len(t.ops) == 0 {
		if n := len(t.pendingInserts); n > 0 && t.pendingInserts[n-1].table == ref.table && t.pendingInserts[n-1].index+t.pendingInserts[n-1].count == ref.index {
			t.pendingInserts[n-1].count++
		} else {
			t.pendingInserts = append(t.pendingInserts, pendingInsertRun{table: ref.table, index: ref.index, count: 1})
		}
		return
	}
	t.flushPendingInsertLogs()
	copied := append([]any(nil), args...)
	t.ops = append(t.ops, logicalRecord{SQL: query, Args: copied})
}

func (t *Tx) pendingInsertRef(plan simpleDMLPlan, planOK bool, argCount int) (pendingInsertRun, bool) {
	if !planOK || plan.kind != 'i' || plan.params != argCount {
		return pendingInsertRun{}, false
	}
	table, ok := t.writes.Catalog.Table(plan.table)
	if !ok || !canFastPositionedInsert(&t.writes, table) {
		return pendingInsertRun{}, false
	}
	data := t.writes.Tables[table.Name]
	if data == nil || len(data.Pending) == 0 {
		return pendingInsertRun{}, false
	}
	return pendingInsertRun{table: table.Name, index: len(data.Pending) - 1, count: 1}, true
}

func (t *Tx) pendingInsertLastCount() int {
	if len(t.pendingInserts) == 0 {
		return 0
	}
	return t.pendingInserts[len(t.pendingInserts)-1].count
}

func (t *Tx) flushPendingInsertLogs() {
	if len(t.pendingInserts) == 0 {
		return
	}
	for _, run := range t.pendingInserts {
		table := t.writes.Catalog.Tables[run.table]
		data := t.writes.Tables[run.table]
		if table == nil || data == nil || run.index < 0 || run.count < 0 || run.index+run.count > len(data.Pending) {
			continue
		}
		for i := run.index; i < run.index+run.count; i++ {
			row := data.Pending[i]
			t.ops = append(t.ops, logicalRecord{
				SQL:  insertSQLForTable(table),
				Args: append([]any(nil), row.positionedValues()...),
			})
		}
	}
	t.pendingInserts = nil
}

func insertSQLForTable(t *catalog.Table) string {
	return "INSERT INTO " + t.Name + " VALUES (" + strings.TrimRight(strings.Repeat("?,", len(t.Columns)), ",") + ")"
}

func (t *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	t.writes.materializePending()
	triggerScopeMu.RLock()
	defer triggerScopeMu.RUnlock()
	stateView := t.writes
	stateView.ctes = nil
	return queryRows(ctx, &stateView, query, args...)
}

func (t *Tx) Commit() error {
	return t.CommitContext(context.Background())
}

// CommitContext commits the transaction, waiting for another process to
// release the writer lock until either the database busy timeout or ctx ends.
// A canceled wait leaves the transaction open so the caller may retry.
func (t *Tx) CommitContext(ctx context.Context) error {
	if err := t.check(); err != nil {
		return err
	}
	if err := t.db.commit(ctx, t); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.closed = true
			t.tx.Close()
		}
		return err
	}
	t.closed = true
	t.tx.Close()
	return nil
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
	if !looksLikeTransactionControl(query) {
		return false, nil
	}
	type parsedTxControl struct {
		action string
		name   string
		valid  bool
	}
	parsed, err := withQueryStatement(query, func(stmt ast.Statement) (parsedTxControl, error) {
		txstmt, ok := stmt.(*ast.TransactionStmt)
		if !ok {
			return parsedTxControl{}, nil
		}
		name := ""
		if txstmt.Savepoint != nil {
			name = strings.ToLower(txstmt.Savepoint.Unquoted)
		}
		return parsedTxControl{action: strings.ToLower(strings.TrimSpace(string(txstmt.Action))), name: name, valid: true}, nil
	})
	if err != nil || !parsed.valid {
		return false, nil
	}
	action, name := parsed.action, parsed.name
	switch action {
	case "begin", "start", "start_transaction", "commit":
		return true, nil
	case "rollback":
		if name == "" {
			t.writes = t.db.state.clone()
			t.savepoints = nil
			t.ops = nil
			t.pendingInserts = nil
			return true, nil
		}
		for i := len(t.savepoints) - 1; i >= 0; i-- {
			if t.savepoints[i].name == name {
				t.writes = t.savepoints[i].state.clone()
				t.ops = t.ops[:t.savepoints[i].ops]
				t.pendingInserts = t.pendingInserts[:t.savepoints[i].pendingInserts]
				if len(t.pendingInserts) > 0 {
					t.pendingInserts[len(t.pendingInserts)-1].count = t.savepoints[i].pendingLast
				}
				t.savepoints = t.savepoints[:i+1]
				return true, nil
			}
		}
		return true, errors.New("no such savepoint: " + name)
	case "savepoint":
		if name == "" {
			return true, errors.New("savepoint name is required")
		}
		t.savepoints = append(t.savepoints, savepoint{name: name, state: t.writes.clone(), ops: len(t.ops), pendingInserts: len(t.pendingInserts), pendingLast: t.pendingInsertLastCount()})
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

func looksLikeTransactionControl(query string) bool {
	q := strings.TrimSpace(query)
	if q == "" {
		return false
	}
	end := 0
	for end < len(q) {
		c := q[end]
		if c == ';' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		end++
	}
	first := q[:end]
	switch {
	case strings.EqualFold(first, "begin"),
		strings.EqualFold(first, "start"),
		strings.EqualFold(first, "commit"),
		strings.EqualFold(first, "end"),
		strings.EqualFold(first, "rollback"),
		strings.EqualFold(first, "savepoint"),
		strings.EqualFold(first, "release"):
		return true
	default:
		return false
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
		t.pendingInserts = nil
		return Result{}, nil
	case "rollback_to":
		if cmd.name == "" {
			return Result{}, errors.New("savepoint name is required")
		}
		for i := len(t.savepoints) - 1; i >= 0; i-- {
			if t.savepoints[i].name == cmd.name {
				t.writes = t.savepoints[i].state.clone()
				t.ops = t.ops[:t.savepoints[i].ops]
				t.pendingInserts = t.pendingInserts[:t.savepoints[i].pendingInserts]
				if len(t.pendingInserts) > 0 {
					t.pendingInserts[len(t.pendingInserts)-1].count = t.savepoints[i].pendingLast
				}
				t.savepoints = t.savepoints[:i+1]
				return Result{}, nil
			}
		}
		return Result{}, errors.New("no such savepoint: " + cmd.name)
	case "savepoint":
		if cmd.name == "" {
			return Result{}, errors.New("savepoint name is required")
		}
		t.savepoints = append(t.savepoints, savepoint{name: cmd.name, state: t.writes.clone(), ops: len(t.ops), pendingInserts: len(t.pendingInserts), pendingLast: t.pendingInsertLastCount()})
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
