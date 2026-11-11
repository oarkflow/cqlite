package cqlite

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// assertTriggerParity compares results and error messages against sqlite3. A
// statement that fails is recorded as an ERROR row so RAISE(...) behaviour is
// compared on the same footing as successful statements.
func assertTriggerParity(t *testing.T, setup, statements []string) {
	t.Helper()
	custom := runCustomMixedErr(t, setup, statements)
	sqlite := runSQLiteMixedErr(t, setup, statements)
	for i, q := range statements {
		if got, want := formatParity(custom[i]), formatParity(sqlite[i]); got != want {
			t.Errorf("query %d mismatch: %s\n  custom: %s\n  sqlite: %s", i, q, got, want)
		}
	}
}

func runCustomMixedErr(t *testing.T, setup, statements []string) [][][]string {
	t.Helper()
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, stmt := range setup {
		if _, err := d.Exec(ctx, stmt); err != nil {
			t.Fatalf("custom setup %q: %v", stmt, err)
		}
	}
	var out [][][]string
	for _, stmt := range statements {
		trimmed := strings.TrimLeft(strings.ToLower(strings.TrimSpace(stmt)), " \t\r\n")
		isQuery := strings.HasPrefix(trimmed, "select") || strings.HasPrefix(trimmed, "with")
		if !isQuery {
			if _, err := d.Exec(ctx, stmt); err != nil {
				out = append(out, [][]string{{"ERROR", err.Error()}})
				continue
			}
			out = append(out, nil)
			continue
		}
		rows, err := d.Query(ctx, stmt)
		if err != nil {
			out = append(out, [][]string{{"ERROR", err.Error()}})
			continue
		}
		var result [][]string
		for {
			row, ok := rows.Next()
			if !ok {
				break
			}
			result = append(result, normalizeRow(row))
		}
		out = append(out, result)
	}
	return out
}

func runSQLiteMixedErr(t *testing.T, setup, statements []string) [][][]string {
	t.Helper()
	dbPath := t.TempDir() + "/sqlite.db"
	runSQLiteScript(t, dbPath, setup)
	var out [][][]string
	for _, stmt := range statements {
		raw, runErr := trySQLiteQuery(dbPath, stmt)
		if runErr != nil {
			out = append(out, [][]string{{"ERROR", runErr.Error()}})
			continue
		}
		if raw == "" {
			out = append(out, nil)
			continue
		}
		text := strings.TrimSuffix(raw, "\n")
		var result [][]string
		for _, line := range strings.Split(text, "\n") {
			result = append(result, strings.Split(line, "\t"))
		}
		out = append(out, result)
	}
	return out
}

func trySQLiteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", "-batch", "-noheader", "-separator", "\t", "-nullvalue", "NULL", dbPath, query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New(normalizeSQLiteError(strings.TrimSpace(stderr.String())))
	}
	return stdout.String(), nil
}

// normalizeSQLiteError strips the decoration the sqlite3 CLI wraps around the
// underlying library error so message text can be compared directly.
func normalizeSQLiteError(msg string) string {
	msg = strings.TrimSpace(msg)
	msg = strings.TrimPrefix(msg, "Error: stepping, ")
	msg = strings.TrimPrefix(msg, "Error: ")
	// The CLI prefixes errors with their kind and source location.
	for _, prefix := range []string{"Parse error near line ", "Runtime error near line ", "Error near line "} {
		if strings.HasPrefix(msg, prefix) {
			if i := strings.Index(msg, ": "); i >= 0 {
				msg = msg[i+2:]
			}
			break
		}
	}
	// The CLI appends the SQLite result code, e.g. "bad number (19)".
	if i := strings.LastIndex(msg, " ("); i > 0 && strings.HasSuffix(msg, ")") {
		if _, err := strconv.Atoi(msg[i+2 : len(msg)-1]); err == nil {
			msg = msg[:i]
		}
	}
	if i := strings.LastIndex(msg, ": "); i > 0 && i > len(msg)-12 {
		if _, err := strconv.Atoi(msg[i+2:]); err == nil {
			msg = msg[:i]
		}
	}
	return msg
}

func TestSQLiteParityTriggers(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE main (id INTEGER PRIMARY KEY, v TEXT, n INTEGER)`,
		`CREATE TABLE audit (id INTEGER PRIMARY KEY, action TEXT, detail TEXT)`,
		`INSERT INTO main VALUES (1, 'a', 10)`,
		`INSERT INTO main VALUES (2, 'b', 20)`,
		`CREATE TRIGGER trg_ai AFTER INSERT ON main BEGIN INSERT INTO audit (action, detail) VALUES ('ins', NEW.v); END`,
		`CREATE TRIGGER trg_au AFTER UPDATE ON main BEGIN INSERT INTO audit (action, detail) VALUES ('upd', OLD.v || '>' || NEW.v); END`,
		`CREATE TRIGGER trg_ad AFTER DELETE ON main BEGIN INSERT INTO audit (action, detail) VALUES ('del', OLD.v); END`,
	}
	assertTriggerParity(t, setup, []string{
		`INSERT INTO main VALUES (3, 'c', 30)`,
		`SELECT id, action, detail FROM audit ORDER BY id`,
		`UPDATE main SET v = 'B' WHERE id = 2`,
		`SELECT id, action, detail FROM audit ORDER BY id`,
		`DELETE FROM main WHERE id = 1`,
		`SELECT id, action, detail FROM audit ORDER BY id`,
		`SELECT id, v, n FROM main ORDER BY id`,
	})
}

func TestSQLiteParityTriggersBeforeWhenRaise(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE m (id INTEGER PRIMARY KEY, v TEXT, n INTEGER)`,
		`CREATE TABLE log (id INTEGER PRIMARY KEY, msg TEXT)`,
		`INSERT INTO m VALUES (1, 'a', 10)`,
		`CREATE TRIGGER bi BEFORE INSERT ON m WHEN NEW.n < 0 BEGIN INSERT INTO log (msg) VALUES ('rejected'); SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER bw BEFORE UPDATE ON m WHEN NEW.n = 13 BEGIN SELECT RAISE(FAIL, 'bad number'); END`,
	}
	assertTriggerParity(t, setup, []string{
		`INSERT INTO m VALUES (2, 'b', 20)`,
		`INSERT INTO m VALUES (3, 'c', -5)`,
		`SELECT id, v, n FROM m ORDER BY id`,
		`SELECT msg FROM log ORDER BY id`,
		`UPDATE m SET n = 13 WHERE id = 1`,
		`SELECT id, v, n FROM m ORDER BY id`,
	})
}

func TestSQLiteParityTriggerUpdateOfAndDrop(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE m (id INTEGER PRIMARY KEY, a TEXT, b TEXT)`,
		`CREATE TABLE log (id INTEGER PRIMARY KEY, msg TEXT)`,
		`INSERT INTO m VALUES (1, 'a1', 'b1')`,
		`CREATE TRIGGER uo AFTER UPDATE OF a ON m BEGIN INSERT INTO log (msg) VALUES ('a-changed'); END`,
	}
	assertTriggerParity(t, setup, []string{
		`UPDATE m SET b = 'b2' WHERE id = 1`,
		`SELECT msg FROM log`,
		`UPDATE m SET a = 'a2' WHERE id = 1`,
		`SELECT msg FROM log`,
		`DROP TRIGGER uo`,
		`UPDATE m SET a = 'a3' WHERE id = 1`,
		`SELECT msg FROM log`,
		`SELECT id, a, b FROM m ORDER BY id`,
	})
}

func TestSQLiteParityTriggerMultiStatementAndNested(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE m (id INTEGER PRIMARY KEY, v TEXT, n INTEGER)`,
		`CREATE TABLE log (id INTEGER PRIMARY KEY, msg TEXT)`,
		`CREATE TABLE tally (k TEXT PRIMARY KEY, c INTEGER)`,
		`INSERT INTO m VALUES (1, 'a', 1)`,
		`CREATE TRIGGER multi AFTER INSERT ON m BEGIN INSERT INTO log (msg) VALUES ('one'); INSERT INTO log (msg) VALUES ('two'); END`,
		`CREATE TRIGGER nested AFTER INSERT ON log BEGIN INSERT OR IGNORE INTO tally (k, c) VALUES (NEW.msg, 1); END`,
	}
	assertTriggerParity(t, setup, []string{
		`INSERT INTO m VALUES (2, 'b', 2)`,
		`SELECT msg FROM log ORDER BY id`,
		`SELECT k, c FROM tally ORDER BY k`,
	})
}

func TestSQLiteParityTriggerInsteadOf(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE m (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE log (id INTEGER PRIMARY KEY, msg TEXT)`,
		`CREATE VIEW vw AS SELECT id, v FROM m`,
		`CREATE TRIGGER io INSTEAD OF INSERT ON vw BEGIN INSERT INTO m (id, v) VALUES (NEW.id, NEW.v); END`,
	}
	assertTriggerParity(t, setup, []string{
		`INSERT INTO vw VALUES (1, 'x')`,
		`SELECT id, v FROM m ORDER BY id`,
	})
}

func TestSQLiteParityTriggerInsteadOfUpdateDelete(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE m (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE log (id INTEGER PRIMARY KEY, msg TEXT)`,
		`CREATE VIEW vw AS SELECT id, v FROM m`,
		`INSERT INTO m VALUES (1, 'a'), (2, 'b')`,
		`CREATE TRIGGER io_i INSTEAD OF INSERT ON vw BEGIN INSERT INTO m (id, v) VALUES (NEW.id, NEW.v); END`,
		`CREATE TRIGGER io_u INSTEAD OF UPDATE ON vw BEGIN UPDATE m SET v = NEW.v WHERE id = NEW.id; END`,
		`CREATE TRIGGER io_d INSTEAD OF DELETE ON vw BEGIN DELETE FROM m WHERE id = OLD.id; END`,
	}
	assertTriggerParity(t, setup, []string{
		`UPDATE vw SET v = 'B' WHERE id = 2`,
		`SELECT id, v FROM m ORDER BY id`,
		`DELETE FROM vw WHERE id = 1`,
		`SELECT id, v FROM m ORDER BY id`,
		`INSERT INTO vw VALUES (3, 'c')`,
		`SELECT id, v FROM m ORDER BY id`,
	})
}
