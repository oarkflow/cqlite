package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	sqlite "github.com/sujit/sqlite"
)

type response struct {
	OK           bool        `json:"ok"`
	Error        string      `json:"error,omitempty"`
	Database     string      `json:"database,omitempty"`
	Objects      []tableInfo `json:"objects,omitempty"`
	Tables       []tableInfo `json:"tables,omitempty"`
	Columns      []string    `json:"columns,omitempty"`
	Rows         [][]any     `json:"rows,omitempty"`
	RowsAffected int64       `json:"rowsAffected,omitempty"`
	LastInsertID int64       `json:"lastInsertId,omitempty"`
	SQL          string      `json:"sql,omitempty"`
}

type tableInfo struct {
	Name    string       `json:"name"`
	Type    string       `json:"type,omitempty"`
	SQL     string       `json:"sql,omitempty"`
	Columns []columnInfo `json:"columns,omitempty"`
	Indexes []indexInfo  `json:"indexes,omitempty"`
}

type columnInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type,omitempty"`
	NotNull    bool   `json:"notNull,omitempty"`
	PrimaryKey bool   `json:"primaryKey,omitempty"`
	Default    any    `json:"default,omitempty"`
}

type indexInfo struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique,omitempty"`
	Columns []string `json:"columns,omitempty"`
	SQL     string   `json:"sql,omitempty"`
}

func main() {
	var dbPath, sql, table string
	var limit int
	flag.StringVar(&dbPath, "db", "", "database file or directory")
	flag.StringVar(&sql, "sql", "", "SQL to execute")
	flag.StringVar(&table, "table", "", "table name")
	flag.IntVar(&limit, "limit", 200, "maximum rows to return")
	flag.Parse()

	cmd := "schema"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}
	if dbPath == "" {
		write(response{OK: false, Error: "missing -db"})
		os.Exit(2)
	}
	if limit <= 0 {
		limit = 200
	}

	ctx := context.Background()
	database, err := sqlite.Open(dbPath, sqlite.Options{})
	if err != nil {
		write(response{OK: false, Error: err.Error(), Database: dbPath})
		os.Exit(1)
	}
	defer database.Close()

	var res response
	switch cmd {
	case "schema":
		res, err = schema(ctx, database, dbPath)
	case "query":
		if strings.TrimSpace(sql) == "" {
			err = fmt.Errorf("missing -sql")
			break
		}
		res, err = runSQL(ctx, database, dbPath, sql, limit)
	case "table":
		if table == "" {
			err = fmt.Errorf("missing -table")
			break
		}
		res, err = runSQL(ctx, database, dbPath, "SELECT * FROM "+quoteIdent(table)+" LIMIT "+fmt.Sprint(limit), limit)
	default:
		err = fmt.Errorf("unknown command: %s", cmd)
	}
	if err != nil {
		write(response{OK: false, Error: err.Error(), Database: dbPath})
		os.Exit(1)
	}
	write(res)
}

func schema(ctx context.Context, database *sqlite.DB, dbPath string) (response, error) {
	rows, err := database.Query(ctx, `SELECT type, name, sql FROM sqlite_schema`)
	if err != nil {
		return response{}, err
	}
	var objects []tableInfo
	var tables []tableInfo
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		if len(row) < 2 {
			continue
		}
		kind := fmt.Sprint(row[0])
		if kind != "table" && kind != "view" {
			continue
		}
		name := fmt.Sprint(row[1])
		info := tableInfo{Name: name, Type: kind}
		if len(row) > 2 && row[2] != nil {
			info.SQL = fmt.Sprint(row[2])
		}
		info.Columns, _ = tableColumns(ctx, database, name)
		if kind == "table" {
			info.Indexes, _ = tableIndexes(ctx, database, name)
			tables = append(tables, info)
		}
		objects = append(objects, info)
	}
	return response{OK: true, Database: dbPath, Objects: objects, Tables: tables}, nil
}

func tableColumns(ctx context.Context, database *sqlite.DB, table string) ([]columnInfo, error) {
	rows, err := database.Query(ctx, `PRAGMA table_info(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	var cols []columnInfo
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		col := columnInfo{}
		if len(row) > 1 {
			col.Name = fmt.Sprint(row[1])
		}
		if len(row) > 2 && row[2] != nil {
			col.Type = fmt.Sprint(row[2])
		}
		if len(row) > 3 {
			col.NotNull = truthy(row[3])
		}
		if len(row) > 4 {
			col.Default = row[4]
		}
		if len(row) > 5 {
			col.PrimaryKey = truthy(row[5])
		}
		cols = append(cols, col)
	}
	return cols, nil
}

func tableIndexes(ctx context.Context, database *sqlite.DB, table string) ([]indexInfo, error) {
	rows, err := database.Query(ctx, `PRAGMA index_list(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	var indexes []indexInfo
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		idx := indexInfo{}
		if len(row) > 1 {
			idx.Name = fmt.Sprint(row[1])
		}
		if len(row) > 2 {
			idx.Unique = truthy(row[2])
		}
		idx.Columns, _ = indexColumns(ctx, database, idx.Name)
		indexes = append(indexes, idx)
	}
	return indexes, nil
}

func indexColumns(ctx context.Context, database *sqlite.DB, index string) ([]string, error) {
	rows, err := database.Query(ctx, `PRAGMA index_info(`+quoteIdent(index)+`)`)
	if err != nil {
		return nil, err
	}
	var columns []string
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		if len(row) > 2 {
			columns = append(columns, fmt.Sprint(row[2]))
		}
	}
	return columns, nil
}

func runSQL(ctx context.Context, database *sqlite.DB, dbPath, sql string, limit int) (response, error) {
	if queryLike(sql) {
		rows, err := database.Query(ctx, sql)
		if err != nil {
			return response{}, err
		}
		out := response{OK: true, Database: dbPath, SQL: sql, Columns: rows.Columns}
		for len(out.Rows) < limit {
			row, ok := rows.Next()
			if !ok {
				break
			}
			out.Rows = append(out.Rows, normalizeRow(row))
		}
		return out, nil
	}
	result, err := database.Exec(ctx, sql)
	if err != nil {
		return response{}, err
	}
	return response{OK: true, Database: dbPath, SQL: sql, RowsAffected: result.RowsAffected, LastInsertID: result.LastInsertID}, nil
}

func queryLike(sql string) bool {
	word := strings.ToUpper(firstWord(sql))
	return word == "SELECT" || word == "WITH" || word == "EXPLAIN" || word == "PRAGMA" || word == "SHOW"
}

func firstWord(sql string) string {
	sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	if sql == "" {
		return ""
	}
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func normalizeRow(row []any) []any {
	out := make([]any, len(row))
	for i, v := range row {
		switch x := v.(type) {
		case []byte:
			out[i] = string(x)
		default:
			out[i] = x
		}
	}
	return out
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	default:
		return false
	}
}

func write(res response) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
