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
	JSON      Type = Text
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
	Name        string       `json:"name"`
	Columns     []Column     `json:"columns"`
	Indexes     []Index      `json:"indexes"`
	NextRowID   int64        `json:"next_row_id"`
	SchemaVers  uint64       `json:"schema_version"`
	Strict      bool         `json:"strict"`
	SQL         string       `json:"sql,omitempty"`
	ForeignKeys []ForeignKey `json:"foreign_keys,omitempty"`
	Checks      []string     `json:"checks,omitempty"`
}

type Catalog struct {
	Tables map[string]*Table `json:"tables"`
}

func New() Catalog {
	return Catalog{Tables: make(map[string]*Table)}
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
	case strings.Contains(decl, "CHAR"), strings.Contains(decl, "CLOB"), strings.Contains(decl, "TEXT"), strings.Contains(decl, "JSON"):
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

func (c Catalog) CreateTable(t *Table, ifNotExists bool) error {
	name := Normalize(t.Name)
	if _, ok := c.Tables[name]; ok {
		if ifNotExists {
			return nil
		}
		return errors.New("table already exists: " + t.Name)
	}
	t.Name = name
	t.NextRowID = 1
	t.SchemaVers = 1
	c.Tables[name] = t
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

func (t *Table) Column(name string) (Column, int, bool) {
	key := Normalize(name)
	for i, col := range t.Columns {
		if Normalize(col.Name) == key {
			return col, i, true
		}
	}
	return Column{}, -1, false
}
