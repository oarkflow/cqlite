package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	cqlite "github.com/oarkflow/cqlite"
)

type response struct {
	OK           bool        `json:"ok"`
	Error        string      `json:"error,omitempty"`
	Database     string      `json:"database,omitempty"`
	Objects      []tableInfo `json:"objects,omitempty"`
	Tables       []tableInfo `json:"tables,omitempty"`
	Results      []response  `json:"results,omitempty"`
	Columns      []string    `json:"columns,omitempty"`
	Rows         [][]any     `json:"rows,omitempty"`
	RowIDs       []any       `json:"rowids,omitempty"`
	RowsAffected int64       `json:"rowsAffected,omitempty"`
	LastInsertID int64       `json:"lastInsertId,omitempty"`
	SQL          string      `json:"sql,omitempty"`
	DurationMS   int64       `json:"durationMs,omitempty"`
	PreviewSQL   []string    `json:"previewSql,omitempty"`
}

type tableInfo struct {
	Name        string           `json:"name"`
	Type        string           `json:"type,omitempty"`
	SQL         string           `json:"sql,omitempty"`
	Columns     []columnInfo     `json:"columns,omitempty"`
	Indexes     []indexInfo      `json:"indexes,omitempty"`
	ForeignKeys []foreignKeyInfo `json:"foreignKeys,omitempty"`
	Triggers    []triggerInfo    `json:"triggers,omitempty"`
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

type foreignKeyInfo struct {
	ID       any    `json:"id,omitempty"`
	Seq      any    `json:"seq,omitempty"`
	Table    string `json:"table,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	OnUpdate string `json:"onUpdate,omitempty"`
	OnDelete string `json:"onDelete,omitempty"`
	Match    string `json:"match,omitempty"`
}

type triggerInfo struct {
	Name string `json:"name"`
	SQL  string `json:"sql,omitempty"`
}

type statementPayload struct {
	SQL        string   `json:"sql,omitempty"`
	Statements []string `json:"statements,omitempty"`
	PreviewSQL []string `json:"previewSql,omitempty"`
	Table      string   `json:"table,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Offset     int      `json:"offset,omitempty"`
	Sort       string   `json:"sort,omitempty"`
	Where      string   `json:"where,omitempty"`
}

func main() {
	var dbPath, sql, table, payload string
	var limit int
	flag.StringVar(&dbPath, "db", "", "database file or directory")
	flag.StringVar(&sql, "sql", "", "SQL to execute")
	flag.StringVar(&table, "table", "", "table name")
	flag.StringVar(&payload, "payload", "", "JSON payload")
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
	if isPlainSQLiteFile(dbPath) {
		write(response{OK: false, Database: dbPath, Error: "not a cQlite database: this is an ordinary SQLite file. cQlite opens only its own packed .cq format"})
		os.Exit(1)
	}
	database, err := cqlite.Open(dbPath, cqlite.Options{})
	if err != nil {
		write(response{OK: false, Error: err.Error(), Database: dbPath})
		os.Exit(1)
	}
	defer database.Close()

	var res response
	switch cmd {
	case "schema", "schemaDetail":
		res, err = schema(ctx, database, dbPath)
	case "query", "execute":
		if strings.TrimSpace(sql) == "" {
			err = fmt.Errorf("missing -sql")
			break
		}
		res, err = runSQL(ctx, database, dbPath, sql, limit)
	case "executeBatch":
		stmts := splitStatements(sql)
		if len(stmts) == 0 && payload != "" {
			var req statementPayload
			if decErr := json.Unmarshal([]byte(payload), &req); decErr != nil {
				err = decErr
				break
			}
			stmts = req.statements()
		}
		if len(stmts) == 0 {
			err = fmt.Errorf("missing statements")
			break
		}
		res, err = runBatch(ctx, database, dbPath, stmts, limit)
	case "browseRows":
		res, err = browseRows(ctx, database, dbPath, table, payload, limit)
	case "table":
		if table == "" {
			err = fmt.Errorf("missing -table")
			break
		}
		res, err = runSQL(ctx, database, dbPath, "SELECT * FROM "+quoteIdent(table)+" LIMIT "+fmt.Sprint(limit), limit)
	case "applyDataEdits", "applySchemaChange":
		res, err = applyStatements(ctx, database, dbPath, payload)
	case "explain":
		if strings.TrimSpace(sql) == "" {
			err = fmt.Errorf("missing -sql")
			break
		}
		res, err = runSQL(ctx, database, dbPath, "EXPLAIN QUERY PLAN "+strings.TrimSpace(strings.TrimSuffix(sql, ";")), limit)
	default:
		err = fmt.Errorf("unknown command: %s", cmd)
	}
	if err != nil {
		write(response{OK: false, Error: err.Error(), Database: dbPath})
		os.Exit(1)
	}
	write(res)
}

// isPlainSQLiteFile reports whether path is a regular SQLite 3 file, which the cQlite
// engine cannot read; checking the header lets us fail with a clear message.
func isPlainSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return string(head) == "SQLite format 3\x00"
}

func schema(ctx context.Context, database *cqlite.DB, dbPath string) (response, error) {
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
		info.ForeignKeys, _ = tableForeignKeys(ctx, database, name)
		info.Triggers, _ = tableTriggers(ctx, database, name)
		if kind == "table" {
			info.Indexes, _ = tableIndexes(ctx, database, name)
			tables = append(tables, info)
		}
		objects = append(objects, info)
	}
	return response{OK: true, Database: dbPath, Objects: objects, Tables: tables}, nil
}

func tableForeignKeys(ctx context.Context, database *cqlite.DB, table string) ([]foreignKeyInfo, error) {
	rows, err := database.Query(ctx, `PRAGMA foreign_key_list(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	var keys []foreignKeyInfo
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		fk := foreignKeyInfo{}
		if len(row) > 0 {
			fk.ID = row[0]
		}
		if len(row) > 1 {
			fk.Seq = row[1]
		}
		if len(row) > 2 {
			fk.Table = fmt.Sprint(row[2])
		}
		if len(row) > 3 {
			fk.From = fmt.Sprint(row[3])
		}
		if len(row) > 4 {
			fk.To = fmt.Sprint(row[4])
		}
		if len(row) > 5 {
			fk.OnUpdate = fmt.Sprint(row[5])
		}
		if len(row) > 6 {
			fk.OnDelete = fmt.Sprint(row[6])
		}
		if len(row) > 7 {
			fk.Match = fmt.Sprint(row[7])
		}
		keys = append(keys, fk)
	}
	return keys, nil
}

func tableTriggers(ctx context.Context, database *cqlite.DB, table string) ([]triggerInfo, error) {
	rows, err := database.Query(ctx, `SELECT name, sql FROM sqlite_schema WHERE type = 'trigger' AND tbl_name = `+sqlString(table)+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var triggers []triggerInfo
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		tr := triggerInfo{}
		if len(row) > 0 {
			tr.Name = fmt.Sprint(row[0])
		}
		if len(row) > 1 && row[1] != nil {
			tr.SQL = fmt.Sprint(row[1])
		}
		triggers = append(triggers, tr)
	}
	return triggers, nil
}

func tableColumns(ctx context.Context, database *cqlite.DB, table string) ([]columnInfo, error) {
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

func tableIndexes(ctx context.Context, database *cqlite.DB, table string) ([]indexInfo, error) {
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

func indexColumns(ctx context.Context, database *cqlite.DB, index string) ([]string, error) {
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

func runSQL(ctx context.Context, database *cqlite.DB, dbPath, sql string, limit int) (response, error) {
	started := time.Now()
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
		out.DurationMS = time.Since(started).Milliseconds()
		return out, nil
	}
	result, err := database.Exec(ctx, sql)
	if err != nil {
		return response{}, err
	}
	return response{OK: true, Database: dbPath, SQL: sql, RowsAffected: result.RowsAffected, LastInsertID: result.LastInsertID, DurationMS: time.Since(started).Milliseconds()}, nil
}

func runBatch(ctx context.Context, database *cqlite.DB, dbPath string, stmts []string, limit int) (response, error) {
	started := time.Now()
	out := response{OK: true, Database: dbPath}
	for _, stmt := range stmts {
		res, err := runSQL(ctx, database, dbPath, stmt, limit)
		if err != nil {
			res = response{OK: false, Database: dbPath, SQL: stmt, Error: err.Error()}
		}
		out.Results = append(out.Results, res)
	}
	out.DurationMS = time.Since(started).Milliseconds()
	return out, nil
}

func browseRows(ctx context.Context, database *cqlite.DB, dbPath, table, payload string, limit int) (response, error) {
	var req statementPayload
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return response{}, err
		}
	}
	if table == "" {
		table = req.Table
	}
	if table == "" {
		return response{}, fmt.Errorf("missing -table")
	}
	if req.Limit > 0 {
		limit = req.Limit
	}
	if limit <= 0 {
		limit = 200
	}
	columns, _ := tableColumns(ctx, database, table)
	columnNames := make(map[string]bool, len(columns))
	for _, column := range columns {
		columnNames[strings.ToLower(column.Name)] = true
	}
	rowIDName := ""
	for _, candidate := range []string{"rowid", "_rowid_", "oid"} {
		if !columnNames[candidate] {
			rowIDName = candidate
			break
		}
	}
	buildQuery := func(projection string) string {
		var q strings.Builder
		q.WriteString("SELECT ")
		q.WriteString(projection)
		q.WriteString(" FROM ")
		q.WriteString(quoteIdent(table))
		if strings.TrimSpace(req.Where) != "" {
			q.WriteString(" WHERE ")
			q.WriteString(req.Where)
		}
		if strings.TrimSpace(req.Sort) != "" {
			q.WriteString(" ORDER BY ")
			q.WriteString(req.Sort)
		}
		q.WriteString(" LIMIT ")
		q.WriteString(fmt.Sprint(limit))
		if req.Offset > 0 {
			q.WriteString(" OFFSET ")
			q.WriteString(fmt.Sprint(req.Offset))
		}
		return q.String()
	}
	if rowIDName != "" {
		result, err := runSQL(ctx, database, dbPath, buildQuery(rowIDName+", *"), limit)
		if err == nil && len(result.Columns) > 0 {
			out := result
			out.Columns = append([]string(nil), result.Columns[1:]...)
			out.RowIDs = make([]any, 0, len(result.Rows))
			out.Rows = make([][]any, 0, len(result.Rows))
			for _, row := range result.Rows {
				if len(row) == 0 {
					continue
				}
				out.RowIDs = append(out.RowIDs, row[0])
				out.Rows = append(out.Rows, append([]any(nil), row[1:]...))
			}
			return out, nil
		}
	}
	return runSQL(ctx, database, dbPath, buildQuery("*"), limit)
}

func applyStatements(ctx context.Context, database *cqlite.DB, dbPath, payload string) (response, error) {
	started := time.Now()
	var req statementPayload
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return response{}, err
	}
	stmts := req.statements()
	if len(stmts) == 0 {
		return response{}, fmt.Errorf("missing statements")
	}
	tx, err := database.Begin(ctx, cqlite.TxOptions{})
	if err != nil {
		return response{}, err
	}
	var out response
	out = response{OK: true, Database: dbPath, PreviewSQL: req.PreviewSQL}
	for _, stmt := range stmts {
		started := time.Now()
		result, err := tx.Exec(ctx, stmt)
		if err != nil {
			_ = tx.Rollback()
			return response{}, err
		}
		out.Results = append(out.Results, response{OK: true, Database: dbPath, SQL: stmt, RowsAffected: result.RowsAffected, LastInsertID: result.LastInsertID, DurationMS: time.Since(started).Milliseconds()})
		out.RowsAffected += result.RowsAffected
		out.LastInsertID = result.LastInsertID
	}
	if err := tx.Commit(); err != nil {
		return response{}, err
	}
	out.DurationMS = time.Since(started).Milliseconds()
	return out, nil
}

func (p statementPayload) statements() []string {
	var stmts []string
	if strings.TrimSpace(p.SQL) != "" {
		stmts = append(stmts, splitStatements(p.SQL)...)
	}
	for _, stmt := range p.Statements {
		if strings.TrimSpace(stmt) != "" {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}

func splitStatements(sql string) []string {
	var stmts []string
	var b strings.Builder
	var quote rune
	for _, r := range sql {
		b.WriteRune(r)
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ';' {
			stmt := strings.TrimSpace(b.String())
			if stmt != "" {
				stmts = append(stmts, stmt)
			}
			b.Reset()
		}
	}
	if stmt := strings.TrimSpace(b.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
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

func sqlString(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
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
