package db

import (
	"context"
	"strings"
	"testing"
)

func TestSQLiteParityScalarStrings(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT substr('hello',2,3)`,
		`SELECT substr('hello',2)`,
		`SELECT substr('hello',-2)`,
		`SELECT substr('hello',-2,1)`,
		`SELECT substr('hello',2,0)`,
		`SELECT substr('hello',2,-1)`,
		`SELECT substr('hello',-2,-1)`,
		`SELECT substr('hello',10)`,
		`SELECT substr('héllo',2,2)`,
		`SELECT substr('日本語',2,1)`,
		`SELECT substr(NULL,1,1)`,
		`SELECT substr('hello',NULL,1)`,
		`SELECT length('hello'), length('héllo'), length('日本'), length(x'6162'), length(NULL), length(123)`,
		`SELECT octet_length('héllo'), octet_length('日本'), octet_length(x'6162')`,
		`SELECT replace('hello','l','L'), replace('hello','l',''), replace(NULL,'a','b'), replace('aaa','a','bb')`,
		`SELECT instr('hello','ll'), instr('hello','z'), instr('hello',''), instr('héllo','l'), instr(NULL,'a')`,
		`SELECT trim('  hi  '), trim('xxhixx','x'), ltrim('xxhi','x'), rtrim('hixx','x'), trim(NULL), trim('xaxbax','ab')`,
		`SELECT lower('ÉxAB'), upper('éxAb'), lower(NULL)`,
		`SELECT char(65,66,67), char(65,NULL,67), char(), unicode('A'), unicode('é'), unicode(NULL)`,
		`SELECT hex('AB'), hex('é'), hex(x'01'), hex(unhex('4142')), unhex('zz'), unhex('4'), typeof(unhex('4142'))`,
		`SELECT quote('a''b'), quote(3), quote(3.5), quote(NULL), quote(x'01'), quote('it''s')`,
		`SELECT printf('%d-%s',5,'a'), printf('%.2f',3.14159), printf('%05d',42), printf('%q','a''b'), printf('%%')`,
		`SELECT concat('a','b',NULL,'c'), concat_ws('-','a',NULL,'b'), concat_ws(NULL,'a')`,
		`SELECT octet_length(zeroblob(3)), typeof(zeroblob(3)), hex(zeroblob(2)), octet_length(zeroblob(4))`,
	})
}

func TestSQLiteParityScalarNumeric(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT abs(-3), typeof(abs(-3)), abs(-3.5), typeof(abs(-3.5)), abs(NULL), abs(0)`,
		`SELECT round(2.5), round(3.5), round(-2.5), round(2.345,2), round(2.0), typeof(round(2.0)), round(NULL)`,
		`SELECT sign(-3), sign(0), sign(2.5), sign(NULL), typeof(sign(-3))`,
		`SELECT min(3,1,2), max(3,1,2), min('b','a'), min(3,'a'), max(NULL,5), min(NULL,NULL), typeof(min(3,1))`,
		`SELECT iif(1,'y','n'), iif(0,'y','n'), iif(NULL,'y','n')`,
		`SELECT mod(5,3), typeof(mod(5,3)), mod(5.5,2), mod(5,0), mod(NULL,2)`,
		`SELECT power(2,3), typeof(power(2,3)), sqrt(2), typeof(sqrt(2))`,
		`SELECT floor(2.7), typeof(floor(2.7)), ceil(2.1), trunc(2.9), trunc(-2.9)`,
		`SELECT pi(), typeof(pi())`,
		`SELECT likelihood(3, 0.5), likely('a'), unlikely(1.5)`,
		`SELECT 1+1, typeof(1+1), 1+1.0, typeof(1+1.0), 5-8, 2*3, 1/2, typeof(1/2), 1/2.0, 5%3, 1/0`,
		`SELECT -3, typeof(-3), -3.5, typeof(-3.5), -(-3)`,
		`SELECT '3'+1, typeof('3'+1), '3.0'+1, typeof('3.0'+1)`,
	})
}

func TestSQLiteParityScalarDateTime(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT date('2024-03-15')`,
		`SELECT date('2024-03-15 14:30:45')`,
		`SELECT time('2024-03-15 14:30:45')`,
		`SELECT time('14:30:45')`,
		`SELECT datetime('2024-03-15 14:30:45')`,
		`SELECT datetime('2024-03-15')`,
		`SELECT julianday('2024-03-15')`,
		`SELECT typeof(julianday('2024-03-15'))`,
		`SELECT unixepoch('2024-03-15 12:00:00')`,
		`SELECT typeof(unixepoch('2024-03-15 12:00:00'))`,
		`SELECT date('2024-03-15','+1 day')`,
		`SELECT date('2024-03-15','+1 month')`,
		`SELECT date('2024-03-15','+1 year')`,
		`SELECT date('2024-03-15','-3 days')`,
		`SELECT date('2024-03-15','start of month')`,
		`SELECT date('2024-03-15','start of year')`,
		`SELECT date('2024-03-15','start of day')`,
		`SELECT date('2024-03-15','weekday 1')`,
		`SELECT date('2024-01-31','+1 month')`,
		`SELECT date('2024-03-15 14:30:45','+2 hours')`,
		`SELECT date('2024-03-15 14:30:45','+90 minutes')`,
		`SELECT strftime('%Y-%m-%d','2024-03-15')`,
		`SELECT strftime('%H:%M:%S','2024-03-15 14:30:45')`,
		`SELECT strftime('%d/%m/%Y','2024-03-05')`,
		`SELECT strftime('%s','2024-03-15 12:00:00')`,
		`SELECT strftime('%J','2024-03-15')`,
		`SELECT strftime('%w','2024-03-15')`,
		`SELECT strftime('%F %T','2024-03-15 14:30:45')`,
		`SELECT strftime('%Y','2024-03-15','-1 year')`,
		`SELECT date('not a date')`,
		`SELECT date(NULL)`,
		`SELECT timediff('2024-03-15 12:00:00','2024-03-15 10:00:00')`,
		`SELECT date(2460384)`,
	})
}

func TestSQLiteParityScalarJSON(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT json_valid('{"a":5}'), json_valid('{a:5}'), json_valid(NULL)`,
		`SELECT json('{"a":5}')`,
		`SELECT json_array(1,'a',NULL)`,
		`SELECT json_object('a',1,'b','x')`,
		`SELECT json_quote('a'), json_quote(3), json_quote(3.5), json_quote(NULL)`,
		`SELECT json_type('{"a":5}'), json_type('[1,2]'), json_type('5'), json_type('"x"'), json_type('null')`,
		`SELECT json_type('{"a":5}','$.a')`,
		`SELECT json_array_length('[1,2,3]'), json_array_length('{"a":1}'), json_array_length('[1,2,3]','$')`,
		`SELECT json_extract('{"a":5}','$.a')`,
		`SELECT typeof(json_extract('{"a":5}','$.a'))`,
		`SELECT json_extract('{"a":{"b":1}}','$.a')`,
		`SELECT json_extract('[10,20,30]','$[1]')`,
		`SELECT json_extract('{"a":1,"b":2}','$.a','$.b')`,
		`SELECT json_extract('{"a":5}','$.z')`,
		`SELECT json_insert('{"a":1}','$.b',2)`,
		`SELECT json_insert('{"a":1}','$.a',2)`,
		`SELECT json_replace('{"a":1}','$.a',2)`,
		`SELECT json_replace('{"a":1}','$.b',2)`,
		`SELECT json_set('{"a":1}','$.b',2)`,
		`SELECT json_set('{"a":1}','$.a',2)`,
		`SELECT json_remove('{"a":1,"b":2}','$.a')`,
		`SELECT json_remove('[1,2,3]','$[0]')`,
		`SELECT json_patch('{"a":1,"b":2}','{"b":3,"c":4}')`,
		`SELECT json_patch('{"a":1}','{"a":null}')`,
		`SELECT json_error_position('{"a":5}')`,
		`SELECT json_error_position('{a:5}')`,
	})
}

func TestSQLiteParityAlterColumn(t *testing.T) {
	requireSQLite3(t)
	setup := []string{
		`CREATE TABLE alt (id INTEGER PRIMARY KEY, x TEXT, y INTEGER)`,
		`INSERT INTO alt VALUES (1, 'a', 10)`,
		`INSERT INTO alt VALUES (2, 'b', 20)`,
	}
	// SQLite's ALTER TABLE surface is RENAME TO, RENAME COLUMN, ADD COLUMN and
	// DROP COLUMN; the PostgreSQL-style ALTER COLUMN actions are accepted by
	// the engine but are not comparable against sqlite3.
	queries := []string{
		`ALTER TABLE alt RENAME COLUMN x TO renamed`,
		`SELECT renamed, y FROM alt ORDER BY id`,
		`SELECT y FROM alt WHERE renamed = 'a'`,
		`SELECT id, renamed, y FROM alt ORDER BY id`,
		`ALTER TABLE alt RENAME COLUMN renamed TO final`,
		`SELECT id, final, y FROM alt ORDER BY id`,
		`ALTER TABLE alt ADD COLUMN z TEXT`,
		`SELECT id, final, y, z FROM alt ORDER BY id`,
		`UPDATE alt SET z = 'set' WHERE id = 1`,
		`SELECT id, final, y, z FROM alt ORDER BY id`,
		`ALTER TABLE alt DROP COLUMN z`,
		`SELECT id, final, y FROM alt ORDER BY id`,
	}
	custom := runCustomMixed(t, setup, queries)
	sqlite := runSQLite(t, setup, queries)
	for i, q := range queries {
		if got, want := formatParity(custom[i]), formatParity(sqlite[i]); got != want {
			t.Errorf("query %d mismatch: %s\n  custom: %s\n  sqlite: %s", i, q, got, want)
		}
	}
}

// runCustomMixed runs a script whose entries may be DDL/DML or SELECT, so that
// schema-mutating statements can be interleaved with parity checks the way the
// sqlite3 harness does.
func runCustomMixed(t *testing.T, setup, statements []string) [][][]string {
	t.Helper()
	ctx := context.Background()
	d, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, stmt := range setup {
		if _, err := d.Exec(ctx, stmt); err != nil {
			t.Fatalf("custom exec %q: %v", stmt, err)
		}
	}
	var out [][][]string
	for _, stmt := range statements {
		trimmed := strings.TrimLeft(strings.ToLower(strings.TrimSpace(stmt)), " \t\r\n")
		if !strings.HasPrefix(trimmed, "select") && !strings.HasPrefix(trimmed, "with") {
			if _, err := d.Exec(ctx, stmt); err != nil {
				t.Fatalf("custom exec %q: %v", stmt, err)
			}
			out = append(out, nil)
			continue
		}
		rows, err := d.Query(ctx, stmt)
		if err != nil {
			t.Fatalf("custom query %q: %v", stmt, err)
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
