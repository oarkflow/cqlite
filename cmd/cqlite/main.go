package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	db "github.com/oarkflow/cqlite"
)

type cliState struct {
	headers bool
	mode    string
}

func main() {
	path := flag.String("data", "./data", "database directory")
	flag.Parse()

	database, err := db.Open(*path, db.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()

	if err := runCLI(context.Background(), database, os.Stdin, os.Stdout, os.Stderr, true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, database *db.DB, in io.Reader, out, errOut io.Writer, prompt bool) error {
	state := cliState{headers: true, mode: "tabs"}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var pending strings.Builder
	printPrompt(out, prompt, pending.Len() > 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			printPrompt(out, prompt, pending.Len() > 0)
			continue
		}
		if pending.Len() == 0 && strings.HasPrefix(line, ".") {
			quit, err := runDotCommand(ctx, database, &state, line, out)
			if err != nil {
				fmt.Fprintln(errOut, "error:", err)
			}
			if quit {
				return nil
			}
			printPrompt(out, prompt, false)
			continue
		}
		if pending.Len() > 0 {
			pending.WriteByte('\n')
		}
		pending.WriteString(line)
		if !statementComplete(pending.String()) {
			printPrompt(out, prompt, true)
			continue
		}
		stmt := strings.TrimSpace(pending.String())
		pending.Reset()
		if err := runSQL(ctx, database, state, stmt, out); err != nil {
			fmt.Fprintln(errOut, "error:", err)
		}
		printPrompt(out, prompt, false)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(pending.String()) != "" {
		return fmt.Errorf("incomplete SQL statement")
	}
	return nil
}

func printPrompt(out io.Writer, prompt, cont bool) {
	if !prompt {
		return
	}
	if cont {
		fmt.Fprint(out, "   ...> ")
		return
	}
	fmt.Fprint(out, "cqlite> ")
}

func statementComplete(sql string) bool {
	return strings.HasSuffix(strings.TrimSpace(sql), ";")
}

func runDotCommand(ctx context.Context, database *db.DB, state *cliState, line string, out io.Writer) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false, nil
	}
	switch strings.ToLower(fields[0]) {
	case ".quit", ".exit":
		return true, nil
	case ".headers":
		if len(fields) != 2 {
			return false, fmt.Errorf("usage: .headers on|off")
		}
		switch strings.ToLower(fields[1]) {
		case "on":
			state.headers = true
		case "off":
			state.headers = false
		default:
			return false, fmt.Errorf("usage: .headers on|off")
		}
	case ".mode":
		if len(fields) != 2 {
			return false, fmt.Errorf("usage: .mode tabs|list|csv|column")
		}
		mode := strings.ToLower(fields[1])
		switch mode {
		case "tabs", "list", "csv", "column":
			state.mode = mode
		default:
			return false, fmt.Errorf("usage: .mode tabs|list|csv|column")
		}
	case ".tables":
		return false, printTables(ctx, database, out)
	case ".schema":
		pattern := ""
		if len(fields) > 1 {
			pattern = strings.ToLower(fields[1])
		}
		return false, printSchema(ctx, database, pattern, out)
	default:
		return false, fmt.Errorf("unknown command: %s", fields[0])
	}
	return false, nil
}

func runSQL(ctx context.Context, database *db.DB, state cliState, sql string, out io.Writer) error {
	if queryLike(sql) {
		rows, err := database.Query(ctx, sql)
		if err != nil {
			return err
		}
		return printRows(rows, state, out)
	}
	res, err := database.Exec(ctx, sql)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ok rows=%d last_insert_id=%d\n", res.RowsAffected, res.LastInsertID)
	return nil
}

func queryLike(sql string) bool {
	first := strings.ToUpper(firstWord(sql))
	return first == "SELECT" || first == "EXPLAIN" || first == "PRAGMA"
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

func printRows(rows *db.Rows, state cliState, out io.Writer) error {
	if state.headers {
		if err := printValues(rows.Columns, state, out); err != nil {
			return err
		}
	}
	for {
		row, ok := rows.Next()
		if !ok {
			return nil
		}
		values := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				values[i] = "NULL"
			} else {
				values[i] = fmt.Sprint(v)
			}
		}
		if err := printValues(values, state, out); err != nil {
			return err
		}
	}
}

func printValues(values []string, state cliState, out io.Writer) error {
	switch state.mode {
	case "csv":
		w := csv.NewWriter(out)
		if err := w.Write(values); err != nil {
			return err
		}
		w.Flush()
		return w.Error()
	case "list":
		fmt.Fprintln(out, strings.Join(values, "|"))
	case "column":
		for i, value := range values {
			if i > 0 {
				fmt.Fprint(out, "  ")
			}
			fmt.Fprintf(out, "%-12s", value)
		}
		fmt.Fprintln(out)
	default:
		fmt.Fprintln(out, strings.Join(values, "\t"))
	}
	return nil
}

func printTables(ctx context.Context, database *db.DB, out io.Writer) error {
	rows, err := database.Query(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return err
	}
	var names []string
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		if len(row) > 0 && row[0] != nil {
			names = append(names, fmt.Sprint(row[0]))
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprintln(out, strings.Join(names, " "))
	}
	return nil
}

func printSchema(ctx context.Context, database *db.DB, pattern string, out io.Writer) error {
	rows, err := database.Query(ctx, `SELECT name, sql FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return err
	}
	type schemaRow struct {
		name string
		sql  string
	}
	var schemas []schemaRow
	for {
		row, ok := rows.Next()
		if !ok {
			break
		}
		if len(row) < 2 || row[0] == nil || row[1] == nil {
			continue
		}
		name := fmt.Sprint(row[0])
		if pattern != "" && strings.ToLower(name) != pattern {
			continue
		}
		schemas = append(schemas, schemaRow{name: name, sql: fmt.Sprint(row[1])})
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].name < schemas[j].name })
	for _, schema := range schemas {
		fmt.Fprintln(out, strings.TrimSpace(schema.sql)+";")
	}
	return nil
}
