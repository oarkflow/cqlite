package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sqlparser "github.com/oarkflow/sqlparser"
)

type sample struct {
	name string
	sql  string
}

func main() {
	samples, err := loadSamples("examples/sql")
	if err != nil {
		fatal("load samples", err)
	}
	fmt.Printf("Loaded %d sample SQL files\n", len(samples))
	fmt.Println(strings.Repeat("=", 80))

	for _, s := range samples {
		runSample(s)
		fmt.Println(strings.Repeat("-", 80))
	}

	fmt.Println("Done.")
}

func loadSamples(dir string) ([]sample, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []sample
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, sample{name: e.Name(), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func runSample(s sample) {
	fmt.Printf("Sample: %s\n", s.name)
	fmt.Printf("Input : %s\n", compact(s.sql))

	stmts, err := sqlparser.ParseStatements(s.sql)
	if err != nil {
		fmt.Printf("Parse : ERROR: %v\n", err)
		return
	}
	fmt.Printf("Parse : OK (%d statement(s))\n", len(stmts))
	report := sqlparser.AnalyzeSQL(s.sql)
	fmt.Printf("Analyze (generic): %s\n", report.String())
	for _, f := range report.Findings {
		fmt.Printf("  - [%s] %s (stmt %d)\n", f.Severity, f.Code, f.StatementIndex)
		fmt.Printf("    Problem      : %s\n", f.Problem)
		if strings.TrimSpace(f.Recommendation) != "" {
			fmt.Printf("    Recommendation: %s\n", f.Recommendation)
		}
	}
	runDialectAnalysis(s.sql, sqlparser.DialectMySQL, "MySQL")
	runDialectAnalysis(s.sql, sqlparser.DialectPostgres, "PG")
	runDialectAnalysis(s.sql, sqlparser.DialectSQLite, "SQLite")

	mysql, err := sqlparser.ConvertDialect(s.sql, sqlparser.DialectMySQL)
	if err != nil {
		fmt.Printf("MySQL : ERROR: %v\n", err)
	} else {
		fmt.Printf("MySQL : %s\n", compact(mysql))
	}

	postgres, err := sqlparser.ConvertDialect(s.sql, sqlparser.DialectPostgres)
	if err != nil {
		fmt.Printf("PG    : ERROR: %v\n", err)
	} else {
		fmt.Printf("PG    : %s\n", compact(postgres))
	}

	sqlite, err := sqlparser.ConvertDialect(s.sql, sqlparser.DialectSQLite)
	if err != nil {
		fmt.Printf("SQLite: ERROR: %v\n", err)
	} else {
		fmt.Printf("SQLite: %s\n", compact(sqlite))
	}
}

func runDialectAnalysis(sql string, dialect sqlparser.Dialect, label string) {
	report := sqlparser.AnalyzeSQLWithOptions(sql, sqlparser.AnalysisOptions{Dialect: dialect})
	fmt.Printf("Analyze (%s): %s\n", label, report.String())
	for _, f := range report.Findings {
		fmt.Printf("  - [%s] %s (stmt %d)\n", f.Severity, f.Code, f.StatementIndex)
		fmt.Printf("    Problem      : %s\n", f.Problem)
		if strings.TrimSpace(f.Recommendation) != "" {
			fmt.Printf("    Recommendation: %s\n", f.Recommendation)
		}
	}
	opt, err := sqlparser.OptimizeSQLForDialect(sql, dialect)
	if err != nil {
		fmt.Printf("Optimize (%s): ERROR: %v\n", label, err)
		return
	}
	if opt.Converted {
		fmt.Printf("Optimize (%s): converted SQL available (%d action(s))\n", label, len(opt.Actions))
	} else {
		fmt.Printf("Optimize (%s): no conversion needed (%d action(s))\n", label, len(opt.Actions))
	}
	for _, a := range opt.Actions {
		fmt.Printf("  - Action: %s\n", a)
	}
}

func compact(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 220 {
		return s[:220] + " ..."
	}
	return s
}

func fatal(step string, err error) {
	fmt.Fprintf(os.Stderr, "%s failed: %v\n", step, err)
	os.Exit(1)
}
