package db

import (
	"fmt"
	"sort"
	"strings"

	catalog "github.com/sujit/sqlite/internal/catalog"
)

// VirtualTable is an alias for the catalog's persisted CREATE VIRTUAL TABLE
// definition. The module supplies the row source; the shadow tables it creates
// live in the ordinary catalog so the WAL and checkpoint machinery keeps them
// durable.
type VirtualTable = catalog.VirtualTable

// vtabModule implements a virtual table module such as fts5.
type vtabModule interface {
	Name() string
	// Declare reports the columns and shadow tables a virtual table needs.
	Declare(s *persistedState, vt *VirtualTable, args string) error
	// Open binds a virtual table definition to the state it will read and
	// write. The returned cursor factory is used for SELECT.
	Open(s *persistedState, vt *VirtualTable) (vtabInstance, error)
}

// vtabInstance is a live virtual table bound to a database state.
type vtabInstance interface {
	// Insert adds a row with the given rowid and column values.
	Insert(rowid int64, values []any) (int64, error)
	// Update replaces the row with the given rowid.
	Update(rowid int64, values []any) error
	// Delete removes the row with the given rowid.
	Delete(rowid int64) error
	// Query runs a full-text query and returns matching rowids in rank order
	// together with their scores. A nil query means "match every row".
	Query(q string, cols []int) ([]int64, []float64, error)
	// Row returns the stored column values for a rowid.
	Row(rowid int64) ([]any, bool)
	// ColumnNames lists the user-visible columns.
	ColumnNames() []string
	// Tokenizer exposes the tokenizer used to build the index, so auxiliary
	// functions can normalize terms the same way the index does.
	Tokenizer() Tokenizer
	// Rows returns every stored rowid in ascending order.
	RowIDs() []int64
}

// vtabModules is the registry of built-in virtual table modules.
var vtabModules = map[string]vtabModule{}

func registerVTabModule(m vtabModule) {
	vtabModules[strings.ToLower(m.Name())] = m
}

func lookupVTabModule(name string) (vtabModule, bool) {
	m, ok := vtabModules[strings.ToLower(name)]
	return m, ok
}

// hasVirtualTables reports whether a name resolves to a virtual table.
func hasVirtualTables(s *persistedState, name string) (*VirtualTable, bool) {
	if s == nil || s.Catalog.VirtualTables == nil {
		return nil, false
	}
	vt, ok := s.Catalog.VirtualTables[catalog.Normalize(name)]
	return vt, ok
}

// execCreateVirtualTable creates a virtual table and its shadow tables.
func execCreateVirtualTable(s *persistedState, name, moduleName, args, rawSQL string, ifNotExists bool) (Result, error) {
	key := catalog.Normalize(name)
	if _, exists := s.Catalog.VirtualTables[key]; exists {
		if ifNotExists {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("table %s already exists", name)
	}
	if _, exists := s.Catalog.Table(key); exists {
		return Result{}, fmt.Errorf("table %s already exists", name)
	}
	mod, ok := lookupVTabModule(moduleName)
	if !ok {
		return Result{}, fmt.Errorf("no such module: %s", moduleName)
	}
	vt := &VirtualTable{
		Name:    key,
		Module:  strings.ToLower(moduleName),
		Args:    args,
		SQL:     rawSQL,
		ModuleKV: map[string]string{},
	}
	if err := mod.Declare(s, vt, args); err != nil {
		return Result{}, err
	}
	if s.Catalog.VirtualTables == nil {
		s.Catalog.VirtualTables = map[string]*VirtualTable{}
	}
	s.Catalog.VirtualTables[key] = vt
	s.Pragmas.SchemaVer++
	return Result{}, nil
}

// execDropVirtualTable removes a virtual table and everything it owns.
func execDropVirtualTable(s *persistedState, name string) error {
	key := catalog.Normalize(name)
	vt, ok := s.Catalog.VirtualTables[key]
	if !ok {
		return fmt.Errorf("no such virtual table: %s", name)
	}
	for _, shadow := range vt.Shadows {
		delete(s.Catalog.Tables, catalog.Normalize(shadow))
		delete(s.Tables, catalog.Normalize(shadow))
	}
	delete(s.Catalog.VirtualTables, key)
	s.Pragmas.SchemaVer++
	return nil
}

// openVTab binds a virtual table to a state for reading and writing.
func openVTab(s *persistedState, vt *VirtualTable) (vtabInstance, error) {
	mod, ok := lookupVTabModule(vt.Module)
	if !ok {
		return nil, fmt.Errorf("no such module: %s", vt.Module)
	}
	return mod.Open(s, vt)
}

// virtualTableColumns returns the user-visible columns of a virtual table as
// catalog columns, so ordinary row contexts can reference them.
func virtualTableColumns(vt *VirtualTable) []catalog.Column {
	out := make([]catalog.Column, 0, len(vt.Columns)+1)
	out = append(out, catalog.Column{Name: "rowid", Type: catalog.Integer, DeclType: "INTEGER", PrimaryKey: true})
	for _, name := range vt.Columns {
		out = append(out, catalog.Column{Name: name, Type: catalog.Text, DeclType: "TEXT"})
	}
	return out
}

// sortedShadowNames returns shadow table names in creation order.
func sortedShadowNames(vt *VirtualTable) []string {
	out := append([]string(nil), vt.Shadows...)
	sort.Strings(out)
	return out
}
