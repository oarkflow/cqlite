package parser_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sqlparser "github.com/oarkflow/cqlite/internal/sqlparser"
	"github.com/oarkflow/cqlite/internal/sqlparser/ast"
	"github.com/oarkflow/cqlite/internal/sqlparser/lexer"
)

// ---- helpers ----

func mustParse(t *testing.T, sql string) sqlparser.Statement {
	t.Helper()
	stmt, err := sqlparser.ParseStatement(sql)
	if err != nil {
		t.Fatalf("parse error: %v\nSQL: %s", err, sql)
	}
	return stmt
}

func mustParseAll(t *testing.T, sql string) []sqlparser.Statement {
	t.Helper()
	stmts, err := sqlparser.ParseStatements(sql)
	if err != nil {
		t.Fatalf("parse error: %v\nSQL: %s", err, sql)
	}
	return stmts
}

// ---- SELECT tests ----

func TestSelectSimple(t *testing.T) {
	stmt := mustParse(t, "SELECT 1")
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("expected *SelectStmt, got %T", stmt)
	}
	if len(sel.Columns) != 1 {
		t.Fatalf("expected 1 column, got %d", len(sel.Columns))
	}
}

func TestSelectStar(t *testing.T) {
	mustParse(t, "SELECT * FROM users")
}

func TestSelectMultiCol(t *testing.T) {
	stmt := mustParse(t, "SELECT id, name, email FROM users")
	sel := stmt.(*ast.SelectStmt)
	if len(sel.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(sel.Columns))
	}
}

func TestSelectWhere(t *testing.T) {
	mustParse(t, "SELECT * FROM users WHERE id = 42 AND active = true")
}

func TestSelectJoin(t *testing.T) {
	mustParse(t, `
		SELECT u.id, o.total
		FROM users u
		INNER JOIN orders o ON u.id = o.user_id
		WHERE o.total > 100
		ORDER BY o.total DESC
		LIMIT 10`)
}

func TestSelectSubquery(t *testing.T) {
	mustParse(t, `
		SELECT * FROM (
			SELECT id, name FROM users WHERE active = 1
		) sub WHERE sub.name LIKE 'A%'`)
}

func TestSelectCTE(t *testing.T) {
	mustParse(t, `
		WITH active_users AS (
			SELECT id, name FROM users WHERE active = 1
		),
		recent_orders AS (
			SELECT user_id, COUNT(*) AS cnt FROM orders GROUP BY user_id
		)
		SELECT u.name, r.cnt
		FROM active_users u
		JOIN recent_orders r ON u.id = r.user_id`)
}

func TestSelectCase(t *testing.T) {
	mustParse(t, `
		SELECT id,
		       CASE status
		           WHEN 1 THEN 'active'
		           WHEN 0 THEN 'inactive'
		           ELSE 'unknown'
		       END AS label
		FROM users`)
}

func TestSelectAggregates(t *testing.T) {
	mustParse(t, `
		SELECT dept, COUNT(*) AS cnt, AVG(salary) AS avg_sal, MAX(salary)
		FROM employees
		GROUP BY dept
		HAVING COUNT(*) > 5
		ORDER BY avg_sal DESC`)
}

func TestSelectDistinct(t *testing.T) {
	mustParse(t, "SELECT DISTINCT dept, role FROM employees")
}

func TestSelectUnion(t *testing.T) {
	mustParse(t, `
		SELECT id, name FROM users
		UNION ALL
		SELECT id, name FROM archived_users`)
}

func TestSelectSetOpChain(t *testing.T) {
	stmt := mustParse(t, `
		SELECT id FROM a
		UNION ALL
		SELECT id FROM b
		INTERSECT
		SELECT id FROM c`)
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("expected *SelectStmt, got %T", stmt)
	}
	if sel.SetOp == nil || sel.SetOp.Right == nil || sel.SetOp.Right.SetOp == nil {
		t.Fatalf("expected chained set operations")
	}
}

func TestSelectIn(t *testing.T) {
	mustParse(t, "SELECT * FROM t WHERE id IN (1, 2, 3)")
	mustParse(t, "SELECT * FROM t WHERE id NOT IN (SELECT id FROM blacklist)")
}

func TestSelectBetween(t *testing.T) {
	mustParse(t, "SELECT * FROM t WHERE age BETWEEN 18 AND 65")
}

func TestSelectLike(t *testing.T) {
	mustParse(t, "SELECT * FROM t WHERE name LIKE '%smith%' ESCAPE '\\'")
}

func TestSelectExists(t *testing.T) {
	mustParse(t, "SELECT * FROM t WHERE EXISTS (SELECT 1 FROM other WHERE other.id = t.id)")
}

func TestSelectCast(t *testing.T) {
	mustParse(t, "SELECT CAST(price AS DECIMAL(10,2)) FROM products")
}

func TestSelectMultipleJoins(t *testing.T) {
	mustParse(t, `
		SELECT a.id, b.name, c.total
		FROM a
		LEFT JOIN b ON a.b_id = b.id
		RIGHT JOIN c ON b.c_id = c.id
		CROSS JOIN d`)
}

func TestSelectOffset(t *testing.T) {
	mustParse(t, "SELECT * FROM t LIMIT 20 OFFSET 40")
	mustParse(t, "SELECT * FROM t LIMIT 40, 20")
}

func TestSelectFunctionCalls(t *testing.T) {
	mustParse(t, `SELECT NOW(), COALESCE(a, b, 0), IFNULL(x, 'default') FROM t`)
}

func TestSelectFunctionNamedArguments(t *testing.T) {
	stmt := mustParse(t, `SELECT make_interval(days => :days, hours := ?) FROM t`)
	sel := stmt.(*ast.SelectStmt)
	call, ok := sel.Columns[0].Expr.(*ast.FuncCall)
	if !ok {
		t.Fatalf("expected *FuncCall, got %T", sel.Columns[0].Expr)
	}
	if len(call.Args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(call.Args))
	}
	first, ok := call.Args[0].(*ast.NamedArg)
	if !ok {
		t.Fatalf("expected first arg *NamedArg, got %T", call.Args[0])
	}
	if got := first.Name.Parts[0].Unquoted; got != "days" {
		t.Fatalf("expected first arg name days, got %q", got)
	}
	if first.Op != lexer.DARROW {
		t.Fatalf("expected => operator, got %s", first.Op)
	}
	if _, ok := first.Value.(*ast.Param); !ok {
		t.Fatalf("expected first arg value *Param, got %T", first.Value)
	}
	second, ok := call.Args[1].(*ast.NamedArg)
	if !ok {
		t.Fatalf("expected second arg *NamedArg, got %T", call.Args[1])
	}
	if second.Op != lexer.ASSIGN {
		t.Fatalf("expected := operator, got %s", second.Op)
	}
}

func TestSelectJSONBOperators(t *testing.T) {
	mustParse(t, `SELECT payload->'user' FROM events`)
	mustParse(t, `SELECT payload->>'user' FROM events`)
	mustParse(t, `SELECT payload#>'{user,name}' FROM events`)
	mustParse(t, `SELECT payload#>>'{user,name}' FROM events`)
	mustParse(t, `SELECT * FROM events WHERE payload @> '{"active":true}'`)
	mustParse(t, `SELECT * FROM events WHERE payload <@ payload`)
	mustParse(t, `SELECT * FROM events WHERE payload ? 'user'`)
	mustParse(t, `SELECT * FROM events WHERE payload ?| '{user,email}'`)
	mustParse(t, `SELECT * FROM events WHERE payload ?& '{user,email}'`)
}

// ---- INSERT tests ----

func TestInsertValues(t *testing.T) {
	mustParse(t, "INSERT INTO users (name, email) VALUES ('Alice', 'alice@example.com')")
}

func TestInsertMultiRow(t *testing.T) {
	mustParse(t, `INSERT INTO users (name, age) VALUES ('A', 1), ('B', 2), ('C', 3)`)
}

func TestInsertValuesPayloadParam(t *testing.T) {
	for _, sql := range []string{
		`INSERT INTO orders (account_id, status, total, created_at) VALUES :rows RETURNING id`,
		`INSERT INTO orders (account_id, status, total, created_at) VALUES :row RETURNING id`,
	} {
		stmt := mustParse(t, sql)
		ins, ok := stmt.(*ast.InsertStmt)
		if !ok {
			t.Fatalf("expected *InsertStmt, got %T", stmt)
		}
		if _, ok := ins.ValuesExpr.(*ast.Param); !ok {
			t.Fatalf("expected ValuesExpr *Param, got %T", ins.ValuesExpr)
		}
		if len(ins.Values) != 0 {
			t.Fatalf("expected no tuple values, got %d row(s)", len(ins.Values))
		}
		if len(ins.Returning) != 1 {
			t.Fatalf("expected RETURNING column, got %d", len(ins.Returning))
		}
	}
}

func TestInsertSelect(t *testing.T) {
	mustParse(t, "INSERT INTO archive SELECT * FROM users WHERE created_at < '2020-01-01'")
}

func TestInsertOnDuplicate(t *testing.T) {
	mustParse(t, `
		INSERT INTO counters (id, val) VALUES (1, 1)
		ON DUPLICATE KEY UPDATE val = val + 1`)
}

func TestInsertOnConflict(t *testing.T) {
	mustParse(t, `
		INSERT INTO counters (id, val) VALUES (1, 1)
		ON CONFLICT (id) DO UPDATE SET val = 2`)
	mustParse(t, `
		INSERT INTO counters (id, val) VALUES (1, 1)
		ON CONFLICT DO NOTHING`)
}

func TestInsertWithCTE(t *testing.T) {
	stmt := mustParse(t, `
		WITH recent AS (SELECT id, name FROM users WHERE active = 1)
		INSERT INTO archive_users (id, name)
		SELECT id, name FROM recent`)
	ins, ok := stmt.(*ast.InsertStmt)
	if !ok {
		t.Fatalf("expected *InsertStmt, got %T", stmt)
	}
	if ins.With == nil {
		t.Fatalf("expected WITH clause on INSERT")
	}
}

func TestReplaceWithCTE(t *testing.T) {
	stmt := mustParse(t, `
		WITH src AS (SELECT 1 AS id, 'A' AS name)
		REPLACE INTO users (id, name)
		SELECT id, name FROM src`)
	ins, ok := stmt.(*ast.InsertStmt)
	if !ok {
		t.Fatalf("expected *InsertStmt, got %T", stmt)
	}
	if ins.With == nil || !ins.Replace {
		t.Fatalf("expected WITH clause on REPLACE")
	}
}

func TestReplace(t *testing.T) {
	mustParse(t, "REPLACE INTO users (id, name) VALUES (1, 'Bob')")
}

// ---- UPDATE tests ----

func TestUpdateSimple(t *testing.T) {
	mustParse(t, "UPDATE users SET name = 'Bob', age = 30 WHERE id = 1")
}

func TestUpdateLimit(t *testing.T) {
	mustParse(t, "UPDATE users SET active = 0 ORDER BY last_login ASC LIMIT 100")
}

func TestUpdateWithCTE(t *testing.T) {
	stmt := mustParse(t, `
		WITH targets AS (SELECT id FROM users WHERE active = 0)
		UPDATE users SET active = 1
		WHERE id IN (SELECT id FROM targets)`)
	upd, ok := stmt.(*ast.UpdateStmt)
	if !ok {
		t.Fatalf("expected *UpdateStmt, got %T", stmt)
	}
	if upd.With == nil {
		t.Fatalf("expected WITH clause on UPDATE")
	}
}

func TestUpdateJSONB(t *testing.T) {
	mustParse(t, `UPDATE events SET payload = payload || '{"processed":true}' WHERE payload ? 'user'`)
}

// ---- DELETE tests ----

func TestDeleteSimple(t *testing.T) {
	mustParse(t, "DELETE FROM users WHERE id = 42")
}

func TestDeleteLimit(t *testing.T) {
	mustParse(t, "DELETE FROM logs WHERE ts < NOW() ORDER BY ts ASC LIMIT 1000")
}

func TestDeleteWithCTE(t *testing.T) {
	stmt := mustParse(t, `
		WITH old_rows AS (SELECT id FROM logs WHERE ts < '2020-01-01')
		DELETE FROM logs WHERE id IN (SELECT id FROM old_rows)`)
	del, ok := stmt.(*ast.DeleteStmt)
	if !ok {
		t.Fatalf("expected *DeleteStmt, got %T", stmt)
	}
	if del.With == nil {
		t.Fatalf("expected WITH clause on DELETE")
	}
}

func TestDeleteJSONB(t *testing.T) {
	mustParse(t, `DELETE FROM events WHERE payload @> '{"deleted":true}'`)
}

// ---- DDL tests ----

func TestCreateTable(t *testing.T) {
	mustParse(t, `
		CREATE TABLE users (
			id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			username   VARCHAR(64) NOT NULL,
			email      VARCHAR(255) NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uq_email (email),
			INDEX idx_username (username)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
}

func TestCreateTableJSONB(t *testing.T) {
	mustParse(t, `CREATE TABLE events (id BIGINT, payload JSONB, meta JSON)`)
}

func TestCreateTableForeignKey(t *testing.T) {
	mustParse(t, `
		CREATE TABLE orders (
			id      INT NOT NULL AUTO_INCREMENT,
			user_id INT NOT NULL,
			total   DECIMAL(10,2) NOT NULL,
			PRIMARY KEY (id),
			CONSTRAINT fk_user FOREIGN KEY (user_id) REFERENCES users (id)
			    ON DELETE CASCADE ON UPDATE RESTRICT
		) ENGINE=InnoDB`)
}

func TestCreateTableIfNotExists(t *testing.T) {
	mustParse(t, `CREATE TABLE IF NOT EXISTS config (k VARCHAR(64) PRIMARY KEY, v TEXT)`)
}

func TestCreateTableAsSelect(t *testing.T) {
	mustParse(t, `CREATE TABLE summary AS SELECT dept, COUNT(*) AS n FROM employees GROUP BY dept`)
}

func TestCreateIndex(t *testing.T) {
	mustParse(t, "CREATE UNIQUE INDEX idx_email ON users (email)")
	mustParse(t, "CREATE INDEX idx_multi ON t (a ASC, b DESC, c(10))")
}

func TestCreateView(t *testing.T) {
	mustParse(t, `
		CREATE OR REPLACE VIEW active_users AS
		SELECT id, name, email FROM users WHERE active = 1`)
}

func TestCreateDatabase(t *testing.T) {
	mustParse(t, "CREATE DATABASE IF NOT EXISTS appdb")
	mustParse(t, "CREATE SCHEMA analytics")
}

func TestAlterTable(t *testing.T) {
	mustParse(t, "ALTER TABLE users ADD COLUMN phone VARCHAR(20) AFTER email")
	mustParse(t, "ALTER TABLE users DROP COLUMN phone")
	mustParse(t, "ALTER TABLE users RENAME TO members")
	mustParse(t, "ALTER TABLE users ADD INDEX idx_phone (phone)")
}

func TestAlterDatabase(t *testing.T) {
	mustParse(t, "ALTER DATABASE appdb CHARACTER SET utf8mb4")
}

func TestDropTable(t *testing.T) {
	mustParse(t, "DROP TABLE IF EXISTS users, orders, products")
}

func TestDropDatabase(t *testing.T) {
	mustParse(t, "DROP DATABASE IF EXISTS appdb")
	mustParse(t, "DROP SCHEMA analytics")
}

func TestDropIndex(t *testing.T) {
	mustParse(t, "DROP INDEX idx_email ON users")
}

func TestTruncate(t *testing.T) {
	mustParse(t, "TRUNCATE TABLE logs")
}

func TestUse(t *testing.T) {
	mustParse(t, "USE mydb")
}

func TestShow(t *testing.T) {
	mustParse(t, "SHOW TABLES")
	mustParse(t, "SHOW TABLES LIKE 'user%'")
}

func TestExplain(t *testing.T) {
	mustParse(t, "EXPLAIN SELECT * FROM users WHERE id = 1")
}

func TestCallStatement(t *testing.T) {
	stmt := mustParse(t, "CALL refresh_cache(42, 'full')")
	if _, ok := stmt.(*ast.CallStmt); !ok {
		t.Fatalf("expected *CallStmt, got %T", stmt)
	}
}

func TestCallNamedArguments(t *testing.T) {
	stmt := mustParse(t, "CALL refresh_cache(mode => 'full', force := ?)")
	call, ok := stmt.(*ast.CallStmt)
	if !ok {
		t.Fatalf("expected *CallStmt, got %T", stmt)
	}
	if len(call.Args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(call.Args))
	}
	if _, ok := call.Args[0].(*ast.NamedArg); !ok {
		t.Fatalf("expected first arg *NamedArg, got %T", call.Args[0])
	}
	if _, ok := call.Args[1].(*ast.NamedArg); !ok {
		t.Fatalf("expected second arg *NamedArg, got %T", call.Args[1])
	}
}

func TestTransactionStatements(t *testing.T) {
	mustParse(t, "BEGIN")
	mustParse(t, "BEGIN TRANSACTION")
	mustParse(t, "COMMIT")
	mustParse(t, "ROLLBACK")
	mustParse(t, "ROLLBACK TO SAVEPOINT sp1")
	mustParse(t, "START TRANSACTION READ WRITE")
	mustParse(t, "SAVEPOINT sp1")
	mustParse(t, "RELEASE SAVEPOINT sp1")
	mustParse(t, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE")
}

func TestGenericRoutineDDL(t *testing.T) {
	stmt := mustParse(t, "CREATE FUNCTION f")
	if _, ok := stmt.(*ast.ObjectDDLStmt); !ok {
		t.Fatalf("expected *ObjectDDLStmt for CREATE FUNCTION, got %T", stmt)
	}
	// DROP TRIGGER now has a dedicated node so the engine can act on it.
	stmt = mustParse(t, "DROP TRIGGER trg_before_insert")
	dt, ok := stmt.(*ast.DropTriggerStmt)
	if !ok {
		t.Fatalf("expected *ast.DropTriggerStmt for DROP TRIGGER, got %T", stmt)
	}
	if dt.Name == nil || dt.Name.Unquoted != "trg_before_insert" {
		t.Fatalf("DROP TRIGGER name = %v", dt.Name)
	}
}

func TestCreateTriggerParsesBody(t *testing.T) {
	stmt := mustParse(t, `CREATE TRIGGER trg BEFORE UPDATE OF a, b ON t FOR EACH ROW WHEN NEW.a > 0 BEGIN UPDATE t SET x = 1 WHERE id = NEW.id; SELECT RAISE(IGNORE); END`)
	ct, ok := stmt.(*ast.CreateTriggerStmt)
	if !ok {
		t.Fatalf("expected *ast.CreateTriggerStmt, got %T", stmt)
	}
	if ct.Name.Unquoted != "trg" || ct.Table.Unquoted != "t" {
		t.Fatalf("trigger name/table = %s/%s", ct.Name.Unquoted, ct.Table.Unquoted)
	}
	if ct.Timing != ast.TriggerBefore || ct.Event != ast.TriggerUpdate {
		t.Fatalf("trigger timing/event = %d/%d", ct.Timing, ct.Event)
	}
	if len(ct.UpdateOf) != 2 {
		t.Fatalf("UPDATE OF columns = %d", len(ct.UpdateOf))
	}
	if ct.When == nil {
		t.Fatal("expected a WHEN clause")
	}
	if len(ct.Body) != 2 {
		t.Fatalf("trigger body statements = %d", len(ct.Body))
	}
}

func TestSelectAdvancedClauses(t *testing.T) {
	stmt := mustParse(t, `
		SELECT DISTINCT ON (u.id)
		       u.*,
		       COUNT(o.id) FILTER (WHERE o.total > 0) OVER w AS order_count
		FROM users u (id, email)
		LEFT JOIN LATERAL (
			SELECT * FROM orders o WHERE o.user_id = u.id
		) lo (id, total) ON true
		JOIN (VALUES (1, 'a'), (2, 'b')) v (id, label) ON v.id = u.id
		WHERE EXISTS (SELECT 1 FROM orders x WHERE x.user_id = u.id)
		WINDOW w AS (PARTITION BY u.id ORDER BY o.created_at DESC)
		ORDER BY u.id NULLS LAST
		LIMIT 10
		FOR UPDATE OF u SKIP LOCKED`)
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("expected *SelectStmt, got %T", stmt)
	}
	if len(sel.DistinctOn) != 1 || len(sel.Windows) != 1 || sel.Lock == nil {
		t.Fatalf("expected distinct-on, window, and lock clauses")
	}
	if !sel.Columns[0].Star || sel.Columns[0].Qualifier == nil {
		t.Fatalf("expected qualified table star")
	}
	if len(sel.OrderBy) != 1 || sel.OrderBy[0].NullsFirst == nil || *sel.OrderBy[0].NullsFirst {
		t.Fatalf("expected NULLS LAST order item")
	}
}

func TestDMLReturningAndExpandedInsert(t *testing.T) {
	ins := mustParse(t, `
		INSERT INTO users SET id = 1, email = 'a@example.com'
		ON CONFLICT (id) WHERE id > 0 DO UPDATE SET email = EXCLUDED.email
		RETURNING id, email`)
	insert, ok := ins.(*ast.InsertStmt)
	if !ok {
		t.Fatalf("expected *InsertStmt, got %T", ins)
	}
	if len(insert.Set) != 2 || insert.OnConflictWhere == nil || len(insert.Returning) != 2 {
		t.Fatalf("expected insert-set, conflict where, and returning")
	}
	def := mustParse(t, `INSERT INTO audit DEFAULT VALUES RETURNING id`)
	if !def.(*ast.InsertStmt).DefaultValues {
		t.Fatalf("expected DEFAULT VALUES")
	}
	upd := mustParse(t, `UPDATE users u SET u.email = 'x' WHERE u.id = 1 RETURNING id`)
	if len(upd.(*ast.UpdateStmt).Returning) != 1 {
		t.Fatalf("expected update returning")
	}
	del := mustParse(t, `DELETE u FROM users u JOIN orders o ON o.user_id = u.id WHERE o.id = 1 RETURNING id`)
	if len(del.(*ast.DeleteStmt).Tables) != 1 || len(del.(*ast.DeleteStmt).Returning) != 1 {
		t.Fatalf("expected mysql delete targets and returning")
	}
}

func TestObjectDDLAndExtendedTableDDL(t *testing.T) {
	stmt := mustParse(t, `CREATE OR REPLACE FUNCTION normalize_email(text) RETURNS text LANGUAGE sql AS 'select lower($1)'`)
	obj, ok := stmt.(*ast.ObjectDDLStmt)
	if !ok {
		t.Fatalf("expected *ObjectDDLStmt, got %T", stmt)
	}
	if !obj.OrReplace || len(obj.Body) == 0 {
		t.Fatalf("expected OR REPLACE and preserved body")
	}
	view := mustParse(t, `CREATE MATERIALIZED VIEW mv_users AS SELECT id FROM users`)
	if v, ok := view.(*ast.CreateViewStmt); !ok || !v.Materialized {
		t.Fatalf("expected materialized view, got %T", view)
	}
	table := mustParse(t, `
		CREATE TEMPORARY TABLE t (
			id BIGINT GENERATED BY DEFAULT AS IDENTITY,
			email VARCHAR(255),
			email_lc VARCHAR(255) GENERATED ALWAYS AS (LOWER(email)) STORED
		)`)
	ct := table.(*ast.CreateTableStmt)
	if !ct.Temporary || !ct.Columns[0].Identity || ct.Columns[2].Generated == nil || !ct.Columns[2].Generated.Stored {
		t.Fatalf("expected temporary table with identity and stored generated column")
	}
	alt := mustParse(t, `
		ALTER TABLE t
		DROP CONSTRAINT IF EXISTS uq_t_email,
		ALTER COLUMN email SET NOT NULL,
		RENAME COLUMN email TO email_address`)
	if len(alt.(*ast.AlterTableStmt).Cmds) != 3 {
		t.Fatalf("expected three alter commands")
	}
	dropSeq := mustParse(t, `DROP SEQUENCE IF EXISTS seq_users`)
	if _, ok := dropSeq.(*ast.ObjectDDLStmt); !ok {
		t.Fatalf("expected object DDL for sequence, got %T", dropSeq)
	}
}

func TestDDLNamedParams(t *testing.T) {
	table := mustParse(t, `
		CREATE TABLE IF NOT EXISTS param_defaults (
			id BIGINT GENERATED BY DEFAULT AS IDENTITY,
			account_id BIGINT NOT NULL DEFAULT :default_account_id CHECK (account_id <> :blocked_account_id),
			status VARCHAR(32) DEFAULT @default_status,
			status_lc VARCHAR(32) GENERATED ALWAYS AS (normalize_status(status => :default_status)) STORED,
			updated_at DATETIME DEFAULT NOW() ON UPDATE COALESCE(:updated_at, NOW()),
			CONSTRAINT chk_param_defaults_status CHECK (status <> :invalid_status)
		)`)
	ct, ok := table.(*ast.CreateTableStmt)
	if !ok {
		t.Fatalf("expected *CreateTableStmt, got %T", table)
	}
	if _, ok := ct.Columns[1].Default.(*ast.Param); !ok {
		t.Fatalf("expected account_id default param, got %T", ct.Columns[1].Default)
	}
	if ct.Columns[1].Check == nil || ct.Columns[3].Generated == nil || ct.Columns[4].OnUpdate == nil {
		t.Fatalf("expected column check, generated expression, and on-update expression")
	}
	if len(ct.Constraints) != 1 || ct.Constraints[0].Check == nil {
		t.Fatalf("expected table-level CHECK constraint")
	}

	ctas := mustParse(t, `CREATE TABLE param_orders AS SELECT * FROM orders WHERE account_id = :account_id`)
	if ctas.(*ast.CreateTableStmt).Select == nil {
		t.Fatalf("expected CREATE TABLE AS SELECT")
	}

	view := mustParse(t, `
		CREATE MATERIALIZED VIEW active_param_orders AS
		SELECT id, account_id, status FROM orders WHERE account_id = :account_id AND status = @active_status`)
	if v, ok := view.(*ast.CreateViewStmt); !ok || !v.Materialized || v.Select == nil {
		t.Fatalf("expected materialized view with select, got %T", view)
	}

	alter := mustParse(t, `
		ALTER TABLE param_defaults
			ADD COLUMN expires_at DATETIME DEFAULT :expires_at,
			ADD CONSTRAINT chk_param_defaults_account CHECK (account_id <> :blocked_account_id),
			ALTER COLUMN status SET DEFAULT @default_status`)
	at, ok := alter.(*ast.AlterTableStmt)
	if !ok {
		t.Fatalf("expected *AlterTableStmt, got %T", alter)
	}
	if len(at.Cmds) != 3 {
		t.Fatalf("expected three alter commands, got %d", len(at.Cmds))
	}

	objDDL := mustParse(t, `CREATE FUNCTION normalize_status(text) RETURNS text LANGUAGE sql AS 'select COALESCE(:status, $1)'`)
	obj, ok := objDDL.(*ast.ObjectDDLStmt)
	if !ok {
		t.Fatalf("expected *ObjectDDLStmt, got %T", objDDL)
	}
	if !strings.Contains(string(obj.Body), ":status") {
		t.Fatalf("expected raw object DDL body to preserve params, got %q", obj.Body)
	}
}

func TestExampleSQLRegression(t *testing.T) {
	for _, path := range []string{
		"../examples/sql/01_select_join_cte_setops.sql",
		"../examples/sql/02_insert_upsert_mysql.sql",
		"../examples/sql/03_insert_upsert_postgres.sql",
		"../examples/sql/04_update_delete_jsonb.sql",
		"../examples/sql/05_create_table_index_view.sql",
		"../examples/sql/06_alter_drop_database.sql",
		"../examples/sql/07_transaction_and_call.sql",
		"../examples/sql/08_replace_and_misc.sql",
		"../examples/sql/09_routine_trigger_fallback.sql",
		"../examples/sql/10_complete_everything.sql",
		"../examples/sql/11_advanced_select_window_lateral.sql",
		"../examples/sql/12_dml_returning_and_defaults.sql",
		"../examples/sql/13_object_ddl_generated_alter.sql",
		"../examples/sql/14_production_migration_comments.sql",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := sqlparser.ParseStatements(string(b)); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
	}
}

// ---- Multiple statements ----

func TestMultipleStatements(t *testing.T) {
	stmts := mustParseAll(t, `
		CREATE TABLE t (id INT);
		INSERT INTO t VALUES (1), (2);
		SELECT * FROM t WHERE id > 0;
		DROP TABLE t;
	`)
	if len(stmts) != 4 {
		t.Fatalf("expected 4 statements, got %d", len(stmts))
	}
}

// ---- Tokenizer tests ----

func TestTokenize(t *testing.T) {
	sql := "SELECT id, name FROM users WHERE id = 1"
	buf := make([]sqlparser.Token, 0, 32)
	toks := sqlparser.Tokenize([]byte(sql), buf)
	if len(toks) == 0 {
		t.Fatal("expected tokens")
	}
}

// ---- Benchmark suite ----

var benchSQL = `
SELECT
    u.id,
    u.username,
    u.email,
    COUNT(o.id) AS order_count,
    SUM(o.total) AS total_spent,
    MAX(o.created_at) AS last_order
FROM users u
LEFT JOIN orders o ON u.id = o.user_id
WHERE u.active = 1
  AND u.created_at BETWEEN '2023-01-01' AND '2024-01-01'
  AND u.country IN ('US', 'CA', 'GB')
GROUP BY u.id, u.username, u.email
HAVING COUNT(o.id) > 0
ORDER BY total_spent DESC
LIMIT 100 OFFSET 0`

var benchDDL = `
CREATE TABLE orders (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id     BIGINT UNSIGNED NOT NULL,
    state       TINYINT(1) NOT NULL DEFAULT 0,
    total       DECIMAL(12,2) NOT NULL DEFAULT 0.00,
    created_at  DATETIME NOT NULL,
    PRIMARY KEY (id),
    KEY idx_user (user_id),
    KEY idx_state (state),
    KEY idx_created (created_at)
) ENGINE=InnoDB`

func BenchmarkParseSelect(b *testing.B) {
	src := []byte(benchSQL)
	p := sqlparser.New(src)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(src)
		_, err := p.Next()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseCreateTable(b *testing.B) {
	src := []byte(benchDDL)
	p := sqlparser.New(src)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(src)
		_, err := p.Next()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTokenize(b *testing.B) {
	src := []byte(benchSQL)
	buf := make([]sqlparser.Token, 0, 128)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sqlparser.Tokenize(src, buf)
	}
}

func BenchmarkParseStatementString(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := sqlparser.ParseStatement(benchSQL)
		if err != nil {
			b.Fatal(err)
		}
	}
}

var benchSimple = `SELECT id, name FROM users WHERE id = 1`

func BenchmarkParseSimpleSelect(b *testing.B) {
	src := []byte(benchSimple)
	p := sqlparser.New(src)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(src)
		_, err := p.Next()
		if err != nil {
			b.Fatal(err)
		}
	}
}

var benchInsert = `INSERT INTO users (id, name, email) VALUES (1, 'Alice', 'alice@example.com')`

func BenchmarkParseInsert(b *testing.B) {
	src := []byte(benchInsert)
	p := sqlparser.New(src)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(src)
		_, err := p.Next()
		if err != nil {
			b.Fatal(err)
		}
	}
}

var benchComplexQuery = `
WITH RECURSIVE active_users AS (
    SELECT id, email FROM users WHERE profile @> '{"active":true}'
),
ranked_orders AS (
    SELECT user_id,
           COUNT(*) FILTER (WHERE total > 0) OVER (PARTITION BY user_id) AS order_count
    FROM orders
)
SELECT DISTINCT ON (u.id) u.id, u.email, r.order_count
FROM active_users u
LEFT JOIN LATERAL (
    SELECT * FROM ranked_orders r WHERE r.user_id = u.id
) r ON true
JOIN (VALUES (1), (2), (3)) v (id) ON v.id = u.id
WHERE EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.id)
ORDER BY u.id NULLS LAST
LIMIT 50
FOR UPDATE OF u SKIP LOCKED`

func BenchmarkParseComplexQuery(b *testing.B) {
	src := []byte(benchComplexQuery)
	p := sqlparser.New(src)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(src)
		_, err := p.Next()
		if err != nil {
			b.Fatal(err)
		}
	}
}

var benchDDLHeavy = `
CREATE TEMPORARY TABLE t (
    id BIGINT GENERATED BY DEFAULT AS IDENTITY,
    email VARCHAR(255),
    email_lc VARCHAR(255) GENERATED ALWAYS AS (LOWER(email)) STORED
);
CREATE MATERIALIZED VIEW mv_t AS SELECT id, email FROM t;
ALTER TABLE t DROP CONSTRAINT IF EXISTS uq_t_email, ALTER COLUMN email SET NOT NULL;
DROP SEQUENCE IF EXISTS seq_t;`

func BenchmarkParseDDLHeavy(b *testing.B) {
	src := []byte(benchDDLHeavy)
	p := sqlparser.New(src)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(src)
		_, err := p.All()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseAllExampleSQL(b *testing.B) {
	inputs := loadExampleSQLBenchInputs(b)
	var totalBytes int64
	for _, in := range inputs {
		totalBytes += int64(len(in.src))
	}

	b.SetBytes(totalBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, in := range inputs {
			in.parser.Reset(in.src)
			if _, err := in.parser.All(); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkParseEachExampleSQL(b *testing.B) {
	for _, in := range loadExampleSQLBenchInputs(b) {
		in := in
		b.Run(in.name, func(b *testing.B) {
			b.SetBytes(int64(len(in.src)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				in.parser.Reset(in.src)
				if _, err := in.parser.All(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseDocumentAllExampleSQL(b *testing.B) {
	inputs := loadExampleSQLBenchInputs(b)
	var totalBytes int64
	for _, in := range inputs {
		totalBytes += int64(len(in.src))
	}
	b.SetBytes(totalBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, in := range inputs {
			doc, err := sqlparser.ParseDocument(in.src)
			if err != nil {
				b.Fatal(err)
			}
			if len(doc.Statements) == 0 {
				b.Fatal("expected statements")
			}
		}
	}
}

type exampleSQLBenchInput struct {
	name   string
	src    []byte
	parser *sqlparser.Parser
}

func loadExampleSQLBenchInputs(b *testing.B) []exampleSQLBenchInput {
	b.Helper()
	paths, err := filepath.Glob("../examples/sql/*.sql")
	if err != nil {
		b.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		b.Fatal("no example SQL files found")
	}

	inputs := make([]exampleSQLBenchInput, 0, len(paths))
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			b.Fatalf("read %s: %v", path, err)
		}
		p := sqlparser.New(src)
		if _, err := p.All(); err != nil {
			b.Fatalf("warm parse %s: %v", path, err)
		}
		inputs = append(inputs, exampleSQLBenchInput{
			name:   filepath.Base(path),
			src:    src,
			parser: p,
		})
	}
	return inputs
}
