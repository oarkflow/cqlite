package cqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	catalog "github.com/oarkflow/cqlite/internal/catalog"
	"github.com/oarkflow/cqlite/internal/sqlparser"
	"github.com/oarkflow/cqlite/internal/sqlparser/ast"
)

// attachedDB is a database opened by ATTACH. Each attached file is a real,
// independently persisted *DB; statements whose target carries the schema
// qualifier are forwarded to it.
type attachedDB struct {
	mu   sync.RWMutex
	name string
	path string
	db   *DB
}

// attachDatabase opens (or creates) a file and registers it under a schema
// name. The schema name "main" and "temp" are reserved.
func (d *DB) attachDatabase(ctx context.Context, name, path string) error {
	name = catalog.Normalize(name)
	if name == "" {
		return errors.New("ATTACH requires a schema name")
	}
	if name == "main" || name == "temp" || name == "tempdb" {
		return fmt.Errorf("database %s is already in use", name)
	}
	if path == "" {
		return errors.New("ATTACH requires a database file")
	}
	d.attachedMu.RLock()
	_, exists := d.attached[name]
	d.attachedMu.RUnlock()
	if exists {
		return fmt.Errorf("database %s is already in use", name)
	}
	opts := d.opts
	opts.DisableFileLock = true
	// The attached file gets its own lock file so two processes cannot corrupt
	// it, but the parent connection already holds the outer lock for the
	// session.
	opts.DisableFileLock = false
	child, err := Open(path, opts)
	if err != nil {
		return fmt.Errorf("cannot open database: %w", err)
	}
	d.attachedMu.Lock()
	defer d.attachedMu.Unlock()
	if d.closed.Load() {
		_ = child.Close()
		return errors.New("database is closed")
	}
	if _, exists := d.attached[name]; exists {
		_ = child.Close()
		return fmt.Errorf("database %s is already in use", name)
	}
	if d.attached == nil {
		d.attached = map[string]*attachedDB{}
	}
	d.attached[name] = &attachedDB{name: name, path: path, db: child}
	d.hasAttached.Store(true)
	return nil
}

// detachDatabase closes and unregisters an attached database.
func (d *DB) detachDatabase(name string) error {
	name = catalog.Normalize(name)
	d.attachedMu.Lock()
	ad, ok := d.attached[name]
	if !ok {
		d.attachedMu.Unlock()
		return fmt.Errorf("no such database: %s", name)
	}
	delete(d.attached, name)
	d.hasAttached.Store(len(d.attached) != 0)
	d.attachedMu.Unlock()
	ad.mu.Lock()
	defer ad.mu.Unlock()
	return ad.db.Close()
}

// attachedByName resolves a schema name to an attached database.
func (d *DB) attachedByName(name string) *attachedDB {
	d.attachedMu.RLock()
	defer d.attachedMu.RUnlock()
	if d.attached == nil {
		return nil
	}
	return d.attached[catalog.Normalize(name)]
}

// databaseList returns (name, file) pairs in SQLite's PRAGMA database_list
// order: main first, then attached databases in attach order.
func (d *DB) databaseList() [][2]string {
	d.attachedMu.RLock()
	defer d.attachedMu.RUnlock()
	out := [][2]string{{"main", d.singleFile}}
	for name, ad := range d.attached {
		_ = name
		out = append(out, [2]string{ad.name, ad.path})
	}
	// Keep the listing stable regardless of map order.
	for i := 2; i < len(out); i++ {
		for j := i; j > 1 && out[j][0] < out[j-1][0]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// statementSchema extracts the single schema qualifier a statement targets.
// It returns ok=false when the statement uses no qualifier or mixes several
// schemas, which the engine reports as unsupported rather than answering
// incorrectly.
func statementSchema(stmt ast.Statement) (string, bool) {
	var schemas []string
	collect := func(q *ast.QualifiedIdent) {
		if q == nil || len(q.Parts) < 2 {
			return
		}
		schemas = append(schemas, catalog.Normalize(q.Parts[0].Unquoted))
	}
	switch st := stmt.(type) {
	case *ast.InsertStmt:
		collect(st.Table)
	case *ast.UpdateStmt:
		for _, ref := range st.Tables {
			collectTableRefSchema(ref, collect)
		}
	case *ast.DeleteStmt:
		for _, ref := range st.From {
			collectTableRefSchema(ref, collect)
		}
	case *ast.CreateTableStmt:
		collect(st.Table)
	case *ast.DropTableStmt:
		for _, t := range st.Tables {
			collect(t)
		}
	case *ast.CreateIndexStmt:
		collect(st.Table)
	case *ast.SelectStmt:
		collectSelectSchemas(st, collect)
	default:
		return "", false
	}
	unique := map[string]struct{}{}
	for _, s := range schemas {
		unique[s] = struct{}{}
	}
	if len(unique) != 1 {
		return "", false
	}
	for s := range unique {
		return s, true
	}
	return "", false
}

// statementHasMultipleSchemas detects statements whose table references name
// more than one database. Those statements need a combined read/write context;
// routing them to one attached handle can otherwise return rows from the wrong
// file because the executor resolves only the final table-name component.
func statementHasMultipleSchemas(stmt ast.Statement) bool {
	var schemas []string
	collect := func(q *ast.QualifiedIdent) {
		if q == nil || len(q.Parts) == 0 {
			return
		}
		if len(q.Parts) < 2 {
			schemas = append(schemas, "main")
			return
		}
		schemas = append(schemas, catalog.Normalize(q.Parts[0].Unquoted))
	}
	switch st := stmt.(type) {
	case *ast.SelectStmt:
		collectSelectSchemas(st, collect)
	case *ast.InsertStmt:
		collect(st.Table)
		if st.Select != nil {
			collectSelectSchemas(st.Select, collect)
		}
	case *ast.UpdateStmt:
		for _, ref := range st.Tables {
			collectTableRefSchema(ref, collect)
		}
	case *ast.DeleteStmt:
		for _, ref := range st.From {
			collectTableRefSchema(ref, collect)
		}
	default:
		return false
	}
	seen := map[string]struct{}{}
	for _, schema := range schemas {
		seen[schema] = struct{}{}
	}
	return len(seen) > 1
}

func collectTableRefSchema(ref ast.TableRef, collect func(*ast.QualifiedIdent)) {
	switch r := ref.(type) {
	case *ast.SimpleTable:
		collect(r.Name)
	case *ast.JoinTable:
		collectTableRefSchema(r.Left, collect)
		collectTableRefSchema(r.Right, collect)
	}
}

func collectSelectSchemas(st *ast.SelectStmt, collect func(*ast.QualifiedIdent)) {
	if st == nil {
		return
	}
	for _, ref := range st.From {
		collectTableRefSchema(ref, collect)
	}
	if st.SetOp != nil {
		collectSelectSchemas(st.SetOp.Right, collect)
	}
}

func selectSchemaNames(st *ast.SelectStmt) []string {
	seen := map[string]struct{}{}
	var schemas []string
	collect := func(q *ast.QualifiedIdent) {
		if q == nil || len(q.Parts) == 0 {
			return
		}
		schema := "main"
		if len(q.Parts) > 1 {
			schema = catalog.Normalize(q.Parts[0].Unquoted)
		}
		if _, ok := seen[schema]; !ok {
			seen[schema] = struct{}{}
			schemas = append(schemas, schema)
		}
	}
	collectSelectSchemas(st, collect)
	return schemas
}

func addCompositeSchema(dst *persistedState, schema string, src persistedState, main bool) {
	for name, table := range src.Catalog.Tables {
		qualified := schema + "." + name
		cp := *table
		cp.Name = qualified
		dst.Catalog.Tables[qualified] = &cp
		if data := src.Tables[name]; data != nil {
			dst.Tables[qualified] = compositeTableData(data)
		}
		if main {
			mainCopy := *table
			mainCopy.Columns = append([]catalog.Column(nil), table.Columns...)
			dst.Catalog.Tables[name] = &mainCopy
			if data := src.Tables[name]; data != nil {
				dst.Tables[name] = compositeTableData(data)
			}
		}
	}
}

func compositeTableData(src *tableData) *tableData {
	cp := *src
	cp.rowids = nil
	cp.indexes = nil
	cp.baseRows = len(cp.Rows)
	cp.deferRuntime = true
	cp.rowsShared = true
	return &cp
}

// parseOneUnpooled parses a single statement on a private arena. The pooled
// parser shares mutable arena storage across goroutines and is only safe under
// parseMu, so connection-level schema inspection must not use it.
func parseOneUnpooled(query string) (ast.Statement, error) {
	p := sqlparser.NewString(query)
	return p.Next()
}

// statementTargetName returns the first fully-qualified table name a statement
// names, used to phrase "no such table" errors the way SQLite does.
func statementTargetName(query string) string {
	stmt, err := parseOneUnpooled(query)
	if err != nil {
		return ""
	}
	var name string
	got := func(q *ast.QualifiedIdent) {
		if name != "" || q == nil || len(q.Parts) == 0 {
			return
		}
		parts := make([]string, 0, len(q.Parts))
		for _, p := range q.Parts {
			parts = append(parts, p.Unquoted)
		}
		name = strings.Join(parts, ".")
	}
	switch st := stmt.(type) {
	case *ast.InsertStmt:
		got(st.Table)
	case *ast.UpdateStmt:
		for _, ref := range st.Tables {
			collectTableRefSchema(ref, got)
		}
	case *ast.DeleteStmt:
		for _, ref := range st.From {
			collectTableRefSchema(ref, got)
		}
	case *ast.CreateTableStmt:
		got(st.Table)
	case *ast.SelectStmt:
		collectSelectSchemas(st, got)
	}
	return name
}

// statementSchemaOfQuery resolves the schema a query names, reporting false
// when the statement cannot be parsed or names no schema.
func statementSchemaOfQuery(query string) (string, bool) {
	stmt, err := parseOneUnpooled(query)
	if err != nil {
		return "", false
	}
	return statementSchema(stmt)
}

// isReservedSchema reports whether a name refers to a built-in database.
func isReservedSchema(name string) bool {
	switch catalog.Normalize(name) {
	case "main", "temp", "tempdb":
		return true
	}
	return false
}

// execAttachStatement runs ATTACH/DETACH against the connection's schema list.
func (d *DB) execAttachStatement(ctx context.Context, query string, args ...any) (Result, bool, error) {
	first, _ := nextSQLField(strings.TrimLeft(query, " \t\r\n"))
	if !strings.HasPrefix(first, "--") && !strings.HasPrefix(first, "/*") &&
		!strings.EqualFold(first, "attach") && !strings.EqualFold(first, "detach") {
		return Result{}, false, nil
	}
	stmt, err := parseOneUnpooled(query)
	if err != nil {
		return Result{}, false, nil
	}
	switch st := stmt.(type) {
	case *ast.AttachStmt:
		path, err := d.attachPath(st, args)
		if err != nil {
			return Result{}, true, err
		}
		if err := d.attachDatabase(ctx, st.Name.Unquoted, path); err != nil {
			return Result{}, true, err
		}
		return Result{}, true, nil
	case *ast.DetachStmt:
		if err := d.detachDatabase(st.Name.Unquoted); err != nil {
			return Result{}, true, err
		}
		return Result{}, true, nil
	}
	return Result{}, false, nil
}

// attachPath evaluates the ATTACH file expression to a filesystem path.
func (d *DB) attachPath(st *ast.AttachStmt, args []any) (string, error) {
	b := binder{args: args}
	v, err := evalExpr(st.File, nil, &b)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", errors.New("ATTACH requires a database file")
	}
	return stringifySQL(v), nil
}
