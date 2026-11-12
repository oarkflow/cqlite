package catalog

import (
	"errors"
	"strings"
)

type Type string

const (
	Integer Type = "INTEGER"
	Real    Type = "REAL"
	Text    Type = "TEXT"
	Blob    Type = "BLOB"
	Numeric Type = "NUMERIC"
	Any     Type = "ANY"
	Null    Type = "NULL"

	Int       Type = Integer
	BigInt    Type = Integer
	Boolean   Type = Numeric
	Timestamp Type = Numeric
	// JSON is a distinct declared type with TEXT storage affinity. Keeping it
	// distinct lets writes validate JSON documents and schema introspection
	// preserve the declared type.
	JSON Type = "JSON"
)

type Column struct {
	Name       string      `json:"name"`
	Type       Type        `json:"type"`
	DeclType   string      `json:"decl_type"`
	NotNull    bool        `json:"not_null"`
	PrimaryKey bool        `json:"primary_key"`
	Unique     bool        `json:"unique"`
	Default    any         `json:"default,omitempty"`
	DefaultSQL string      `json:"default_sql,omitempty"`
	Check      string      `json:"check,omitempty"`
	Collation  string      `json:"collation,omitempty"`
	References *ForeignKey `json:"references,omitempty"`
}

type Index struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	Unique     bool     `json:"unique"`
	Expression string   `json:"expression,omitempty"`
	Where      string   `json:"where,omitempty"`
	Partial    bool     `json:"partial,omitempty"`
	SQL        string   `json:"sql,omitempty"`
}

type ForeignKey struct {
	Columns    []string `json:"columns"`
	RefTable   string   `json:"ref_table"`
	RefColumns []string `json:"ref_columns"`
	OnDelete   string   `json:"on_delete,omitempty"`
	OnUpdate   string   `json:"on_update,omitempty"`
}

type Table struct {
	Name              string       `json:"name"`
	Columns           []Column     `json:"columns"`
	PrimaryKeyColumns []string     `json:"primary_key_columns,omitempty"`
	Indexes           []Index      `json:"indexes"`
	NextRowID         int64        `json:"next_row_id"`
	SchemaVers        uint64       `json:"schema_version"`
	Strict            bool         `json:"strict"`
	SQL               string       `json:"sql,omitempty"`
	ForeignKeys       []ForeignKey `json:"foreign_keys,omitempty"`
	Checks            []string     `json:"checks,omitempty"`
}

type View struct {
	Name       string   `json:"name"`
	Columns    []Column `json:"columns,omitempty"`
	SQL        string   `json:"sql,omitempty"`
	SelectSQL  string   `json:"select_sql,omitempty"`
	SchemaVers uint64   `json:"schema_version"`
}

// Trigger records a persisted trigger definition. The body is kept as SQL text
// and re-parsed on use, the same way views store their SELECT.
type Trigger struct {
	Name       string   `json:"name"`
	Table      string   `json:"table"`
	Timing     string   `json:"timing"` // "before", "after", "instead_of", or ""
	Event      string   `json:"event"`  // "insert", "delete", "update"
	UpdateOf   []string `json:"update_of,omitempty"`
	ForEachRow bool     `json:"for_each_row"`
	WhenSQL    string   `json:"when_sql,omitempty"`
	BodySQL    string   `json:"body_sql"`
	SQL        string   `json:"sql,omitempty"`
	Temporary  bool     `json:"temporary,omitempty"`
}

// VirtualTable is a persisted CREATE VIRTUAL TABLE definition. The concrete
// module fields are kept in ModuleKV so the catalog package stays independent
// of any particular virtual table implementation.
type VirtualTable struct {
	Name     string            `json:"name"`
	Module   string            `json:"module"`
	Args     string            `json:"args"`
	Columns  []string          `json:"columns"`
	Shadows  []string          `json:"shadows,omitempty"`
	ModuleKV map[string]string `json:"module_kv,omitempty"`
	SQL      string            `json:"sql,omitempty"`
}

type Catalog struct {
	Tables        map[string]*Table        `json:"tables"`
	Views         map[string]*View         `json:"views,omitempty"`
	Triggers      map[string]*Trigger      `json:"triggers,omitempty"`
	VirtualTables map[string]*VirtualTable `json:"virtual_tables,omitempty"`
}

func New() Catalog {
	return Catalog{Tables: make(map[string]*Table), Views: make(map[string]*View)}
}

// TriggerByName resolves a trigger by normalized name.
func (c Catalog) TriggerByName(name string) (*Trigger, bool) {
	if c.Triggers == nil {
		return nil, false
	}
	t, ok := c.Triggers[Normalize(name)]
	return t, ok
}

// TriggersFor returns the triggers attached to a table for a given event,
// ordered BEFORE first then AFTER/INSTEAD OF, so firing order is stable.
func (c Catalog) TriggersFor(table, event string) []*Trigger {
	if c.Triggers == nil {
		return nil
	}
	table = Normalize(table)
	var before, after []*Trigger
	for _, name := range sortedTriggerNames(c.Triggers) {
		t := c.Triggers[name]
		if t.Table != table || t.Event != event {
			continue
		}
		if t.Timing == "before" {
			before = append(before, t)
		} else {
			after = append(after, t)
		}
	}
	return append(before, after...)
}

func sortedTriggerNames(m map[string]*Trigger) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

func Normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func ParseType(s string) (Type, error) {
	decl := strings.ToUpper(strings.TrimSpace(s))
	if decl == "" {
		return Blob, nil
	}
	switch {
	case decl == "ANY":
		return Any, nil
	case strings.Contains(decl, "INT"):
		return Integer, nil
	case decl == "JSON" || strings.HasPrefix(decl, "JSON("):
		return JSON, nil
	case strings.Contains(decl, "CHAR"), strings.Contains(decl, "CLOB"), strings.Contains(decl, "TEXT"):
		return Text, nil
	case strings.Contains(decl, "BLOB"):
		return Blob, nil
	case strings.Contains(decl, "REAL"), strings.Contains(decl, "FLOA"), strings.Contains(decl, "DOUB"):
		return Real, nil
	case decl == "BOOLEAN", decl == "DATE", decl == "DATETIME", decl == "TIMESTAMP", strings.Contains(decl, "NUMERIC"), strings.Contains(decl, "DECIMAL"):
		return Numeric, nil
	default:
		return Numeric, nil
	}
}

func StrictTypeAllowed(t Type) bool {
	switch t {
	case Integer, Real, Text, Blob, Any:
		return true
	default:
		return false
	}
}

func (c Catalog) Table(name string) (*Table, bool) {
	t, ok := c.Tables[Normalize(name)]
	return t, ok
}

func (c Catalog) View(name string) (*View, bool) {
	if c.Views == nil {
		return nil, false
	}
	v, ok := c.Views[Normalize(name)]
	return v, ok
}

func (c Catalog) CreateTable(t *Table, ifNotExists bool) error {
	name := Normalize(t.Name)
	if _, ok := c.Tables[name]; ok {
		if ifNotExists {
			return nil
		}
		return errors.New("table already exists: " + t.Name)
	}
	if _, ok := c.View(name); ok {
		return errors.New("view already exists: " + t.Name)
	}
	t.Name = name
	t.NextRowID = 1
	t.SchemaVers = 1
	c.Tables[name] = t
	return nil
}

func (c Catalog) CreateView(v *View, orReplace bool) error {
	if c.Views == nil {
		c.Views = make(map[string]*View)
	}
	name := Normalize(v.Name)
	if _, ok := c.Tables[name]; ok {
		return errors.New("table already exists: " + v.Name)
	}
	if _, ok := c.Views[name]; ok && !orReplace {
		return errors.New("view already exists: " + v.Name)
	}
	v.Name = name
	v.SchemaVers++
	if v.SchemaVers == 0 {
		v.SchemaVers = 1
	}
	c.Views[name] = v
	return nil
}

func (c Catalog) DropTable(name string, ifExists bool) error {
	key := Normalize(name)
	if _, ok := c.Tables[key]; !ok {
		if ifExists {
			return nil
		}
		return errors.New("table does not exist: " + name)
	}
	delete(c.Tables, key)
	return nil
}

func (c Catalog) DropView(name string, ifExists bool) error {
	key := Normalize(name)
	if c.Views == nil {
		if ifExists {
			return nil
		}
		return errors.New("view does not exist: " + name)
	}
	if _, ok := c.Views[key]; !ok {
		if ifExists {
			return nil
		}
		return errors.New("view does not exist: " + name)
	}
	delete(c.Views, key)
	return nil
}

func (t *Table) Column(name string) (Column, int, bool) {
	key := Normalize(name)
	for i, col := range t.Columns {
		if Normalize(col.Name) == key {
			return col, i, true
		}
	}
	return Column{}, -1, false
}
