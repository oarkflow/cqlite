package cqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runCustomAttach runs a script against a main database that starts with an
// attached schema, so schema-qualified statements can be compared.
func runCustomAttach(t *testing.T, mainFile, otherFile string, setup, statements []string) [][][]string {
	t.Helper()
	ctx := context.Background()
	d, err := Open(mainFile, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	attach := fmt.Sprintf(`ATTACH DATABASE '%s' AS other`, otherFile)
	if _, err := d.Exec(ctx, attach); err != nil {
		t.Fatalf("attach: %v", err)
	}
	for _, stmt := range setup {
		if _, err := d.Exec(ctx, stmt); err != nil {
			t.Fatalf("custom setup %q: %v", stmt, err)
		}
	}
	var out [][][]string
	for _, stmt := range statements {
		trimmed := strings.TrimLeft(strings.ToLower(strings.TrimSpace(stmt)), " \t\r\n")
		isQuery := strings.HasPrefix(trimmed, "select") || strings.HasPrefix(trimmed, "with") ||
			strings.HasPrefix(trimmed, "pragma")
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

func runSQLiteAttach(t *testing.T, mainFile, otherFile string, setup, statements []string) [][][]string {
	t.Helper()
	// ATTACH is per-connection, so the setup runs once and every later
	// statement re-attaches in its own short-lived connection.
	attach := fmt.Sprintf(`ATTACH DATABASE '%s' AS other;`, otherFile)
	runSQLiteScript(t, mainFile, append([]string{attach}, setup...))
	// ATTACH is per-connection, so the harness mirrors the connection's own
	// attach state: a DETACH stops later statements from re-attaching.
	attached := true
	var out [][][]string
	for _, stmt := range statements {
		lower := strings.ToLower(strings.TrimSpace(stmt))
		script := []string{stmt}
		if attached {
			script = []string{attach, stmt}
		}
		raw, runErr := trySQLiteScriptQuery(mainFile, script)
		if runErr != nil {
			out = append(out, [][]string{{"ERROR", normalizeSQLiteError(runErr.Error())}})
			continue
		}
		// Only update the connection's attach state after the statement ran.
		switch {
		case strings.HasPrefix(lower, "detach"):
			attached = false
		case strings.HasPrefix(lower, "attach "):
			attached = true
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

func assertAttachParity(t *testing.T, setup, statements []string) {
	t.Helper()
	mk := func() (string, string) {
		dir := t.TempDir()
		return filepath.Join(dir, "main.cq"), filepath.Join(dir, "other.cq")
	}
	m1, o1 := mk()
	custom := runCustomAttach(t, m1, o1, setup, statements)
	m2, o2 := mk()
	sqlite := runSQLiteAttach(t, m2, o2, setup, statements)
	for i, q := range statements {
		if got, want := formatParity(custom[i]), formatParity(sqlite[i]); got != want {
			t.Errorf("query %d mismatch: %s\n  custom: %s\n  sqlite: %s", i, q, got, want)
		}
	}
}

func TestSQLiteParityAttach(t *testing.T) {
	requireSQLite3(t)
	assertAttachParity(t, []string{
		`CREATE TABLE main_t (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE other.other_t (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO main_t VALUES (1, 'm1')`,
		`INSERT INTO other.other_t VALUES (10, 'o1')`,
	}, []string{
		`SELECT id, v FROM main_t ORDER BY id`,
		`SELECT id, v FROM other.other_t ORDER BY id`,
		`INSERT INTO other.other_t VALUES (20, 'o2')`,
		`SELECT id, v FROM other.other_t ORDER BY id`,
		`UPDATE other.other_t SET v = 'o2b' WHERE id = 20`,
		`SELECT id, v FROM other.other_t ORDER BY id`,
		`DELETE FROM other.other_t WHERE id = 10`,
		`SELECT id, v FROM other.other_t ORDER BY id`,
		`SELECT id, v FROM main_t ORDER BY id`,
		`SELECT count(*) FROM other.other_t`,
	})
}

// trySQLiteScriptQuery runs a whole script in one sqlite3 process and returns
// the output of the last statement, so ATTACH stays in effect for the query.
func trySQLiteScriptQuery(dbPath string, script []string) (string, error) {
	cmd := exec.Command("sqlite3", "-batch", "-noheader", "-separator", "\t", "-nullvalue", "NULL", dbPath)
	cmd.Stdin = strings.NewReader(strings.Join(script, ";\n") + ";\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New(strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func TestSQLiteParityAttachDetachError(t *testing.T) {
	requireSQLite3(t)
	assertAttachParity(t, []string{
		`CREATE TABLE main_t (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE other.other_t (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO other.other_t VALUES (10, 'o1')`,
	}, []string{
		`DETACH DATABASE other`,
		`SELECT id, v FROM other.other_t`,
		`ATTACH DATABASE ':memory:' AS mem`,
		`SELECT 1`,
	})
}

func TestSQLiteAttachIsFileBacked(t *testing.T) {
	requireSQLite3(t)
	dir := t.TempDir()
	mainFile := filepath.Join(dir, "main.cq")
	otherFile := filepath.Join(dir, "other.cq")
	ctx := context.Background()
	d, err := Open(mainFile, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, fmt.Sprintf(`ATTACH DATABASE '%s' AS other`, otherFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE other.t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO other.t VALUES (1, 'persisted')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen the attached file directly: its rows must have been written to
	// the attached file, not to main.
	reopened, err := Open(otherFile, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(ctx, `SELECT id, v FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || len(row) != 2 || row[1] != "persisted" {
		t.Fatalf("attached database did not persist its own rows: %v ok=%v", row, ok)
	}
	_ = bytes.MinRead
	_ = exec.Command
}

func TestCrossDatabaseSelectReadsQualifiedTablesFromEachFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "main.cq"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, fmt.Sprintf(`ATTACH DATABASE '%s' AS other`, filepath.Join(dir, "other.cq"))); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE main.same_name (v TEXT)`,
		`CREATE TABLE other.same_name (v TEXT)`,
		`INSERT INTO main.same_name VALUES ('main')`,
		`INSERT INTO other.same_name VALUES ('other')`,
	} {
		if _, err := d.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows, err := d.Query(ctx, `SELECT main.same_name.v, other.same_name.v FROM main.same_name JOIN other.same_name ON 1=1`)
	if err != nil {
		t.Fatalf("cross-database select: %v", err)
	}
	row, ok := rows.Next()
	if !ok || len(row) != 2 || row[0] != "main" || row[1] != "other" {
		t.Fatalf("cross-database row = %#v, ok=%v", row, ok)
	}
	rows, err = d.Query(ctx, `SELECT same_name.v, other.same_name.v FROM same_name JOIN other.same_name ON 1=1`)
	if err != nil {
		t.Fatalf("cross-database select with implicit main schema: %v", err)
	}
	row, ok = rows.Next()
	if !ok || len(row) != 2 || row[0] != "main" || row[1] != "other" {
		t.Fatalf("implicit-main cross-database row = %#v, ok=%v", row, ok)
	}
}

func TestCrossDatabaseSelectConcurrentWithAttachedWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "main.cq"), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, fmt.Sprintf(`ATTACH DATABASE '%s' AS other`, filepath.Join(dir, "other.cq"))); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE main.counter (id INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE other.counter (id INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO main.counter VALUES (1, 0)`,
		`INSERT INTO other.counter VALUES (1, 0)`,
	} {
		if _, err := d.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			table := "main.counter"
			if worker == 1 {
				table = "other.counter"
			}
			for i := 0; i < 40; i++ {
				if _, err := d.Exec(ctx, `UPDATE `+table+` SET v = v + 1 WHERE id = 1`); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 40; i++ {
			rows, err := d.Query(ctx, `SELECT main.counter.v, other.counter.v FROM main.counter JOIN other.counter ON main.counter.id = other.counter.id`)
			if err != nil {
				errs <- err
				return
			}
			row, ok := rows.Next()
			if !ok || len(row) != 2 {
				errs <- fmt.Errorf("cross-database query returned row %#v, ok=%v", row, ok)
				return
			}
			for _, value := range row {
				if n, ok := value.(int64); !ok || n < 0 || n > 40 {
					errs <- fmt.Errorf("unexpected concurrent snapshot value %#v", value)
					return
				}
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
