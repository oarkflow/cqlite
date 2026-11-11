package sqlparser

import (
	"fmt"
	"strings"

	"github.com/oarkflow/sqlite/internal/sqlparser/ast"
)

type FindingSeverity string

const (
	SeverityInfo     FindingSeverity = "info"
	SeverityWarning  FindingSeverity = "warning"
	SeverityCritical FindingSeverity = "critical"
)

type AnalysisFinding struct {
	Severity       FindingSeverity
	Code           string
	Message        string
	Problem        string
	Recommendation string
	StatementIndex int
}

type AnalysisReport struct {
	Valid          bool
	StatementCount int
	Findings       []AnalysisFinding
}

type AnalysisOptions struct {
	Dialect Dialect
}

type OptimizationReport struct {
	Dialect      Dialect
	OriginalSQL  string
	OptimizedSQL string
	Converted    bool
	Analysis     AnalysisReport
	Actions      []string
}

func AnalyzeSQL(sql string) AnalysisReport {
	return AnalyzeSQLWithOptions(sql, AnalysisOptions{})
}

func AnalyzeSQLWithOptions(sql string, opts AnalysisOptions) AnalysisReport {
	report := AnalysisReport{}
	stmts, err := ParseStatements(sql)
	if err != nil {
		report.Valid = false
		addFinding(&report, SeverityCritical, "PARSE_ERROR", err.Error(), "Fix SQL syntax at the reported line/column and re-run parsing.", -1)
		return report
	}
	report.Valid = true
	report.StatementCount = len(stmts)

	for i, stmt := range stmts {
		analyzeStatement(stmt, i, &report, opts)
	}
	return report
}

func OptimizeSQLForDialect(sql string, dialect Dialect) (OptimizationReport, error) {
	report := OptimizationReport{
		Dialect:     dialect,
		OriginalSQL: sql,
	}
	report.Analysis = AnalyzeSQLWithOptions(sql, AnalysisOptions{Dialect: dialect})
	if !report.Analysis.Valid {
		return report, fmt.Errorf("cannot optimize invalid SQL: %s", report.Analysis.Findings[0].Problem)
	}
	converted, err := ConvertDialect(sql, dialect)
	if err != nil {
		return report, err
	}
	report.OptimizedSQL = converted
	report.Converted = strings.TrimSpace(sql) != strings.TrimSpace(converted)
	if report.Converted {
		report.Actions = append(report.Actions, fmt.Sprintf("Converted SQL to %s-compatible syntax", dialect))
	}
	seen := map[string]bool{}
	for _, f := range report.Analysis.Findings {
		if f.Recommendation == "" || seen[f.Recommendation] {
			continue
		}
		seen[f.Recommendation] = true
		report.Actions = append(report.Actions, f.Recommendation)
	}
	return report, nil
}

func analyzeStatement(stmt Statement, idx int, report *AnalysisReport, opts AnalysisOptions) {
	switch s := stmt.(type) {
	case *ast.SelectStmt:
		if hasSelectStar(s.Columns) {
			addFinding(report, SeverityWarning, "SELECT_STAR", "Query uses SELECT *; this can read unnecessary columns and break clients if schema changes.", "Select explicit columns needed by the caller (e.g. SELECT id, name) to reduce IO and improve compatibility.", idx)
		}
		if s.SetOp != nil {
			for cur := s.SetOp; cur != nil; cur = cur.Right.SetOp {
				if cur.Op == ast.Union && !cur.All {
					addFinding(report, SeverityInfo, "UNION_DISTINCT_COST", "UNION performs duplicate elimination, which can add sort/hash overhead on large datasets.", "Use UNION ALL when duplicate removal is not required.", idx)
				}
			}
		}
		for _, tr := range s.From {
			if jt, ok := tr.(*ast.JoinTable); ok && jt.Kind == ast.CrossJoin {
				addFinding(report, SeverityWarning, "CROSS_JOIN", "CROSS JOIN can create a cartesian product and explode row counts.", "Ensure join cardinality is intended, or use an INNER/LEFT JOIN with explicit join predicates.", idx)
			}
		}
		analyzeExpr(s.Where, idx, report, opts)
		analyzeExpr(s.Having, idx, report, opts)
		for _, c := range s.Columns {
			analyzeExpr(c.Expr, idx, report, opts)
		}
	case *ast.InsertStmt:
		if len(s.Values) > 1000 {
			addFinding(report, SeverityInfo, "BULK_INSERT_SIZE", "Very large VALUES clause detected; this can increase lock time and memory pressure.", "Split into smaller batches (for example 200-1000 rows) and use transactions if needed.", idx)
		}
		if len(s.OnDupKey) > 0 || len(s.OnConflictUpdate) > 0 || s.OnConflictDoNothing {
			addFinding(report, SeverityInfo, "UPSERT_PRESENT", "Upsert logic detected (ON DUPLICATE KEY / ON CONFLICT).", "Verify matching unique/primary indexes exist on conflict columns to avoid full-table checks.", idx)
		}
		if opts.Dialect == DialectMySQL && (len(s.OnConflictUpdate) > 0 || s.OnConflictDoNothing) {
			addFinding(report, SeverityWarning, "DIALECT_UPSERT_MISMATCH", "ON CONFLICT is not native MySQL syntax.", "Use ON DUPLICATE KEY UPDATE (or run dialect conversion targeting mysql).", idx)
		}
		if opts.Dialect == DialectPostgres && len(s.OnDupKey) > 0 {
			addFinding(report, SeverityWarning, "DIALECT_UPSERT_MISMATCH", "ON DUPLICATE KEY is not native PostgreSQL syntax.", "Use ON CONFLICT (...) DO UPDATE/DO NOTHING (or run dialect conversion targeting postgres).", idx)
		}
		if s.Select != nil {
			for _, c := range s.Select.Columns {
				analyzeExpr(c.Expr, idx, report, opts)
			}
		}
		if s.Replace && opts.Dialect == DialectPostgres {
			addFinding(report, SeverityWarning, "REPLACE_NOT_PORTABLE", "REPLACE is not supported by PostgreSQL.", "Rewrite as INSERT ... ON CONFLICT ... DO UPDATE.", idx)
		}
	case *ast.UpdateStmt:
		if s.Where == nil {
			addFinding(report, SeverityCritical, "UPDATE_WITHOUT_WHERE", "UPDATE statement has no WHERE clause and will affect all rows.", "Add a WHERE predicate or confirm intentionally full-table update using explicit safeguards.", idx)
		}
		if s.Limit != nil && len(s.Order) == 0 {
			addFinding(report, SeverityWarning, "UPDATE_LIMIT_NO_ORDER", "UPDATE uses LIMIT without ORDER BY, so chosen rows may be nondeterministic.", "Add ORDER BY on a stable key (for example primary key) before LIMIT.", idx)
		}
		analyzeExpr(s.Where, idx, report, opts)
		for _, a := range s.Set {
			analyzeExpr(a.Value, idx, report, opts)
		}
	case *ast.DeleteStmt:
		if s.Where == nil {
			addFinding(report, SeverityCritical, "DELETE_WITHOUT_WHERE", "DELETE statement has no WHERE clause and will remove all rows.", "Add a WHERE predicate or use TRUNCATE explicitly when full deletion is intended.", idx)
		}
		if s.Limit != nil && len(s.Order) == 0 {
			addFinding(report, SeverityWarning, "DELETE_LIMIT_NO_ORDER", "DELETE uses LIMIT without ORDER BY, so deleted rows may be nondeterministic.", "Add ORDER BY on a stable key before LIMIT.", idx)
		}
		analyzeExpr(s.Where, idx, report, opts)
	case *ast.CreateTableStmt:
		for _, c := range s.Columns {
			if c.Type != nil && strings.EqualFold(string(c.Type.Name), "jsonb") {
				switch opts.Dialect {
				case DialectMySQL:
					addFinding(report, SeverityInfo, "JSONB_DIALECT_NOTE", "Column uses JSONB but target is MySQL.", "Use JSON type and generated columns + functional indexes for JSON paths.", idx)
				case DialectSQLite:
					addFinding(report, SeverityInfo, "JSONB_DIALECT_NOTE", "Column uses JSONB but target is SQLite.", "Use TEXT storage with JSON1 functions and check constraints for shape validation.", idx)
				default:
					addFinding(report, SeverityInfo, "JSONB_DIALECT_NOTE", "Column uses JSONB. Dialect conversion keeps JSONB for Postgres, rewrites to JSON in MySQL, and TEXT in SQLite.", "If converting across dialects, verify JSON operator compatibility and add dialect-specific indexes (for example GIN in Postgres, generated-column indexes in MySQL).", idx)
				}
			}
			if c.AutoIncrement && opts.Dialect == DialectPostgres {
				addFinding(report, SeverityInfo, "AUTO_INCREMENT_REWRITE", "AUTO_INCREMENT detected with PostgreSQL target.", "Use GENERATED AS IDENTITY (dialect converter can rewrite this).", idx)
			}
		}
	case *ast.GenericDDLStmt:
		addFinding(report, SeverityWarning, "GENERIC_DDL", "Statement was parsed with generic DDL fallback, so internals may not be fully analyzed.", "For best validation, rewrite this statement to a currently modeled form or extend parser support for this DDL type.", idx)
	case *ast.ObjectDDLStmt:
		if len(s.Body) > 0 {
			addFinding(report, SeverityInfo, "OBJECT_DDL_RAW_BODY", "Object DDL body was preserved as raw SQL.", "Use strict conversion or statement-specific validation before applying routine/trigger/function bodies in production.", idx)
		}
	case *ast.UseStmt:
		if opts.Dialect == DialectPostgres || opts.Dialect == DialectSQLite {
			addFinding(report, SeverityWarning, "USE_NOT_SUPPORTED", "USE statement is not portable to this dialect.", "For PostgreSQL use explicit database connection; for SQLite use file/database handle selection in the client.", idx)
		}
	case *ast.AlterDatabaseStmt:
		if opts.Dialect == DialectSQLite {
			addFinding(report, SeverityWarning, "ALTER_DATABASE_NOT_SUPPORTED", "ALTER DATABASE is not supported in SQLite.", "Move database-level options to application/connection settings.", idx)
		}
	}
}

func analyzeExpr(e Expr, idx int, report *AnalysisReport, opts AnalysisOptions) {
	if e == nil {
		return
	}
	switch ex := e.(type) {
	case *ast.LikeExpr:
		if lit, ok := ex.Pattern.(*ast.Literal); ok {
			raw := string(lit.Raw)
			if strings.HasPrefix(raw, "'%") || strings.HasPrefix(raw, "\"%") {
				addFinding(report, SeverityInfo, "LIKE_LEADING_WILDCARD", "LIKE pattern starts with wildcard; index seeks are usually not possible.", "Use anchored pattern (for example 'abc%') or consider full-text/trigram indexing.", idx)
			}
		}
		analyzeExpr(ex.Expr, idx, report, opts)
		analyzeExpr(ex.Pattern, idx, report, opts)
		analyzeExpr(ex.Escape, idx, report, opts)
	case *ast.BinaryExpr:
		if strings.EqualFold(ex.Op.String(), "OR") {
			addFinding(report, SeverityInfo, "OR_PREDICATE", "OR predicate can reduce index selectivity and lead to less efficient plans.", "Consider splitting into UNION ALL branches or adding composite indexes aligned with predicates.", idx)
		}
		analyzeExpr(ex.Left, idx, report, opts)
		analyzeExpr(ex.Right, idx, report, opts)
	case *ast.UnaryExpr:
		analyzeExpr(ex.Expr, idx, report, opts)
	case *ast.FuncCall:
		if ex.Name != nil && len(ex.Name.Parts) == 1 {
			fn := strings.ToUpper(ex.Name.Parts[0].Unquoted)
			if opts.Dialect == DialectPostgres && fn == "IFNULL" {
				addFinding(report, SeverityWarning, "FUNCTION_DIALECT_REWRITE", "IFNULL is not idiomatic in PostgreSQL.", "Use COALESCE(...) for PostgreSQL compatibility.", idx)
			}
			if opts.Dialect == DialectMySQL && fn == "COALESCE" {
				addFinding(report, SeverityInfo, "FUNCTION_DIALECT_REWRITE", "COALESCE will work in MySQL, but IFNULL is often preferred for 2-arg null handling.", "Use IFNULL(a,b) when you specifically need MySQL-style two-argument null coalescing.", idx)
			}
		}
		for _, a := range ex.Args {
			analyzeExpr(a, idx, report, opts)
		}
		if ex.Filter != nil {
			analyzeExpr(ex.Filter, idx, report, opts)
		}
		if ex.Over != nil && len(ex.Over.Raw) > 0 {
			addFinding(report, SeverityInfo, "RAW_WINDOW_FRAME", "Window frame details were preserved as raw SQL.", "Review dialect-specific window frame syntax when converting across databases.", idx)
		}
	case *ast.CaseExpr:
		analyzeExpr(ex.Operand, idx, report, opts)
		analyzeExpr(ex.Else, idx, report, opts)
		for _, w := range ex.Whens {
			analyzeExpr(w.Cond, idx, report, opts)
			analyzeExpr(w.Result, idx, report, opts)
		}
	case *ast.BetweenExpr:
		analyzeExpr(ex.Expr, idx, report, opts)
		analyzeExpr(ex.Lo, idx, report, opts)
		analyzeExpr(ex.Hi, idx, report, opts)
	case *ast.InExpr:
		analyzeExpr(ex.Expr, idx, report, opts)
		for _, v := range ex.List {
			analyzeExpr(v, idx, report, opts)
		}
		if ex.Subq != nil {
			for _, c := range ex.Subq.Columns {
				analyzeExpr(c.Expr, idx, report, opts)
			}
			analyzeExpr(ex.Subq.Where, idx, report, opts)
		}
	case *ast.IsNullExpr:
		analyzeExpr(ex.Expr, idx, report, opts)
	case *ast.ExistsExpr:
		if ex.Subq != nil {
			for _, c := range ex.Subq.Columns {
				analyzeExpr(c.Expr, idx, report, opts)
			}
			analyzeExpr(ex.Subq.Where, idx, report, opts)
		}
	case *ast.SubqueryExpr:
		if ex.Subq != nil {
			for _, c := range ex.Subq.Columns {
				analyzeExpr(c.Expr, idx, report, opts)
			}
			analyzeExpr(ex.Subq.Where, idx, report, opts)
		}
	case *ast.CastExpr:
		analyzeExpr(ex.Expr, idx, report, opts)
	}
}

func hasSelectStar(cols []ast.SelectColumn) bool {
	for _, c := range cols {
		if c.Star {
			return true
		}
	}
	return false
}

func addFinding(report *AnalysisReport, sev FindingSeverity, code, problem, recommendation string, idx int) {
	msg := problem
	if recommendation != "" {
		msg += " Recommendation: " + recommendation
	}
	report.Findings = append(report.Findings, AnalysisFinding{
		Severity:       sev,
		Code:           code,
		Message:        msg,
		Problem:        problem,
		Recommendation: recommendation,
		StatementIndex: idx,
	})
}

func (r AnalysisReport) String() string {
	if !r.Valid {
		if len(r.Findings) == 0 {
			return "invalid SQL"
		}
		return fmt.Sprintf("invalid SQL: %s", r.Findings[0].Problem)
	}
	if len(r.Findings) == 0 {
		return fmt.Sprintf("valid SQL (%d statements), no findings", r.StatementCount)
	}
	return fmt.Sprintf("valid SQL (%d statements), %d finding(s)", r.StatementCount, len(r.Findings))
}
