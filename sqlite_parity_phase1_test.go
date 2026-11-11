package cqlite

import (
	"os/exec"
	"strconv"
	"testing"
)

func requireSQLite3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 binary not available")
	}
}

func parityPhase1Setup() []string {
	return []string{
		`CREATE TABLE a (id INTEGER PRIMARY KEY, g TEXT, n INTEGER, s TEXT)`,
		`INSERT INTO a VALUES (1, 'x', 10, 'alpha')`,
		`INSERT INTO a VALUES (2, 'x', 20, 'beta')`,
		`INSERT INTO a VALUES (3, 'y', 30, 'gamma')`,
		`INSERT INTO a VALUES (4, 'y', 40, 'delta')`,
		`INSERT INTO a VALUES (5, 'y', 50, 'epsilon')`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, aid INTEGER, n INTEGER)`,
		`INSERT INTO b VALUES (1, 1, 100)`,
		`INSERT INTO b VALUES (2, 1, 200)`,
		`INSERT INTO b VALUES (3, 2, 300)`,
		`INSERT INTO b VALUES (4, 6, 400)`,
	}
}

func assertParity(t *testing.T, queries []string) {
	t.Helper()
	custom := runCustomDB(t, parityPhase1Setup(), queries)
	sqlite := runSQLite(t, parityPhase1Setup(), queries)
	for i, q := range queries {
		if got, want := formatParity(custom[i]), formatParity(sqlite[i]); got != want {
			t.Errorf("query %d mismatch: %s\n  custom: %s\n  sqlite: %s", i, q, got, want)
		}
	}
}

func formatParity(rows [][]string) string {
	if len(rows) == 0 {
		return "<empty>"
	}
	s := ""
	for _, r := range rows {
		s += "["
		for i, c := range r {
			if i > 0 {
				s += " "
			}
			// Quote cells so empty strings and embedded whitespace stay visible
			// when a mismatch is reported.
			s += strconv.Quote(c)
		}
		s += "] "
	}
	return s
}

// --- set operations ---

func TestSQLiteParityCompoundSelect(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT id FROM a WHERE id <= 3 UNION SELECT id FROM b WHERE id <= 2`,
		`SELECT id FROM a WHERE id <= 3 UNION ALL SELECT id FROM b WHERE id <= 2`,
		`SELECT id FROM a INTERSECT SELECT id FROM b`,
		`SELECT id FROM a EXCEPT SELECT id FROM b`,
		`SELECT id FROM a UNION SELECT id FROM b UNION SELECT id FROM a`,
		`SELECT id FROM a UNION ALL SELECT id FROM b UNION ALL SELECT id FROM a`,
		`SELECT id FROM a INTERSECT SELECT id FROM b UNION SELECT id FROM a WHERE id = 9`,
		`SELECT id FROM a EXCEPT SELECT id FROM b EXCEPT SELECT id FROM a WHERE id = 1`,
		`SELECT id FROM a WHERE id <= 2 UNION SELECT id FROM b WHERE id <= 3 ORDER BY 1`,
		`SELECT id FROM a WHERE id <= 2 UNION SELECT id FROM b WHERE id <= 3 ORDER BY 1 DESC`,
		`SELECT id FROM a WHERE id <= 3 UNION ALL SELECT id FROM b ORDER BY 1 LIMIT 4`,
		`SELECT id FROM a WHERE id <= 3 UNION ALL SELECT id FROM b ORDER BY 1 DESC LIMIT 3 OFFSET 1`,
		`SELECT s FROM a WHERE id <= 2 UNION SELECT s FROM a WHERE id >= 4 ORDER BY 1`,
		`SELECT id, n FROM a WHERE id <= 1 UNION SELECT id, n FROM b WHERE id <= 1 ORDER BY 1`,
		`SELECT id FROM a WHERE id < 0 UNION SELECT id FROM b WHERE id < 0`,
		`SELECT id FROM a WHERE id < 0 UNION ALL SELECT id FROM b WHERE id < 0`,
	})
}

func TestSQLiteParityCompoundSelectTypeEquality(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT 1 UNION SELECT 1.0 ORDER BY 1`,
		`SELECT 1 UNION SELECT '1' ORDER BY 1`,
		`SELECT NULL UNION SELECT NULL ORDER BY 1`,
		`SELECT NULL UNION ALL SELECT NULL ORDER BY 1`,
		`SELECT 1 UNION SELECT NULL UNION SELECT 1 ORDER BY 1`,
		`SELECT n FROM a WHERE id = 1 UNION SELECT 10.0 ORDER BY 1`,
	})
}

// --- CTEs ---

func TestSQLiteParityCTE(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`WITH t AS (SELECT id FROM a WHERE id <= 3) SELECT * FROM t ORDER BY 1`,
		`WITH t AS (SELECT id AS x FROM a WHERE id <= 3) SELECT x FROM t WHERE x > 1 ORDER BY 1`,
		`WITH t AS (SELECT id, n FROM a) SELECT count(*) FROM t`,
		`WITH t AS (SELECT id, n FROM a) SELECT sum(n) FROM t WHERE id > 1`,
		`WITH t AS (SELECT id FROM a), u AS (SELECT id FROM b) SELECT * FROM t UNION SELECT * FROM u ORDER BY 1`,
		`WITH t AS (SELECT id, g FROM a) SELECT g, count(*) FROM t GROUP BY g ORDER BY g`,
		`WITH t AS (SELECT id, g, n FROM a) SELECT t.g, b.n FROM t JOIN b ON t.id = b.aid ORDER BY t.g, b.n`,
		`WITH t AS (SELECT id FROM a WHERE id > 100) SELECT * FROM t`,
		`WITH t AS (SELECT id, n FROM a ORDER BY n DESC LIMIT 3) SELECT * FROM t ORDER BY n`,
		`WITH t AS (SELECT a.id AS aid, b.n AS bn FROM a JOIN b ON a.id = b.aid) SELECT * FROM t ORDER BY aid, bn`,
	})
}

func TestSQLiteParityRecursiveCTE(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 5) SELECT x FROM c ORDER BY 1`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 5) SELECT sum(x) FROM c`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 10) SELECT count(*) FROM c`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 1) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 0) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION SELECT x+1 FROM c WHERE x < 3) SELECT x FROM c ORDER BY 1`,
		`WITH RECURSIVE c(x, y) AS (SELECT 1, 10 UNION ALL SELECT x+1, y+10 FROM c WHERE x < 4) SELECT x, y FROM c ORDER BY 1`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x*2 FROM c WHERE x < 100) SELECT x FROM c ORDER BY 1`,
		`WITH RECURSIVE c(x) AS (SELECT id FROM a WHERE id = 1 UNION ALL SELECT x+1 FROM c WHERE x < 4) SELECT x FROM c ORDER BY 1`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 5) SELECT x FROM c ORDER BY 1 DESC`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 20) SELECT count(*), sum(x) FROM c`,
	})
}

// --- DISTINCT ---

func TestSQLiteParityDistinct(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT DISTINCT g FROM a ORDER BY g`,
		`SELECT DISTINCT g FROM a ORDER BY g`,
		`SELECT DISTINCT g FROM a ORDER BY g DESC`,
		`SELECT DISTINCT g, n FROM a WHERE id <= 3 ORDER BY g, n`,
		`SELECT DISTINCT n FROM a ORDER BY n`,
		`SELECT DISTINCT s FROM a ORDER BY s`,
		`SELECT DISTINCT id FROM a ORDER BY id`,
		`SELECT DISTINCT g FROM a ORDER BY g LIMIT 1`,
	})
}

// --- window functions ---

func TestSQLiteParityWindowNumbering(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT row_number() OVER (ORDER BY id), id FROM a`,
		`SELECT row_number() OVER (ORDER BY id DESC), id FROM a ORDER BY id`,
		`SELECT row_number() OVER (ORDER BY g, id), id FROM a ORDER BY id`,
		`SELECT row_number() OVER (PARTITION BY g ORDER BY id), g, id FROM a ORDER BY id`,
		`SELECT rank() OVER (ORDER BY n), id, n FROM a ORDER BY n`,
		`SELECT dense_rank() OVER (ORDER BY n), id, n FROM a ORDER BY n`,
		`SELECT rank() OVER (PARTITION BY g ORDER BY n), g, n FROM a ORDER BY g, n`,
		`SELECT dense_rank() OVER (PARTITION BY g ORDER BY n), g, n FROM a ORDER BY g, n`,
		`SELECT percent_rank() OVER (ORDER BY n), n FROM a ORDER BY n`,
		`SELECT cume_dist() OVER (ORDER BY n), n FROM a ORDER BY n`,
		`SELECT percent_rank() OVER (PARTITION BY g ORDER BY n), g, n FROM a ORDER BY g, n`,
		`SELECT ntile(2) OVER (ORDER BY id), id FROM a ORDER BY id`,
		`SELECT ntile(3) OVER (ORDER BY id), id FROM a ORDER BY id`,
		`SELECT ntile(5) OVER (ORDER BY id), id FROM a ORDER BY id`,
		`SELECT ntile(7) OVER (ORDER BY id), id FROM a ORDER BY id`,
	})
}

func TestSQLiteParityWindowNavigation(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT id, n, lag(n) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lag(n, 1, 0) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lag(n, 2) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lag(n, 2, -1) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lead(n) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lead(n, 1, 0) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lead(n, 2) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lag(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, lead(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, first_value(n) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, first_value(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, last_value(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, nth_value(n, 2) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, nth_value(n, 2) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, n, first_value(s) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
	})
}

func TestSQLiteParityWindowAggregates(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT g, n, sum(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT g, n, sum(n) OVER (PARTITION BY g) FROM a ORDER BY id`,
		`SELECT g, n, sum(n) OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT g, n, sum(n) OVER () FROM a ORDER BY id`,
		`SELECT g, n, avg(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT g, n, count(*) OVER (PARTITION BY g) FROM a ORDER BY id`,
		`SELECT g, n, count(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT g, n, min(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT g, n, max(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, avg(n) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM a ORDER BY id`,
		`SELECT id, count(*) OVER (ORDER BY id ROWS BETWEEN 1 FOLLOWING AND 2 FOLLOWING) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY id ROWS BETWEEN 2 FOLLOWING AND 3 FOLLOWING) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY g GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, count(*) OVER (ORDER BY g GROUPS BETWEEN CURRENT ROW AND 1 FOLLOWING) FROM a ORDER BY id`,
		`SELECT id, first_value(n) OVER (ORDER BY g GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW), last_value(n) OVER (ORDER BY g GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY g RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY n RANGE BETWEEN 15 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY n RANGE BETWEEN 12.5 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY n DESC RANGE BETWEEN 15 PRECEDING AND 5 FOLLOWING) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY g RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY printf('%d', n) RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY g ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE GROUP) FROM a ORDER BY id`,
		`SELECT id, sum(n) OVER (ORDER BY g ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE TIES) FROM a ORDER BY id`,
		`SELECT id, count(*) OVER (ORDER BY id ROWS BETWEEN CURRENT ROW AND CURRENT ROW EXCLUDE NO OTHERS) FROM a ORDER BY id`,
		`SELECT id, first_value(n) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW), last_value(n) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW), nth_value(n, 1) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW) FROM a ORDER BY id`,
		`SELECT id, first_value(n) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW), last_value(n) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW), nth_value(n, 2) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM a ORDER BY id`,
		`SELECT g, n, total(n) OVER (PARTITION BY g ORDER BY id) FROM a ORDER BY id`,
		`SELECT g, group_concat(s) OVER (PARTITION BY g) FROM a ORDER BY id`,
		`SELECT g, n, sum(n) OVER (PARTITION BY g ORDER BY n DESC) FROM a ORDER BY g, n`,
	})
}

func TestSQLiteParityWindowWithGroupBy(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT g, sum(n) AS t, row_number() OVER (ORDER BY g) FROM a GROUP BY g ORDER BY g`,
		`SELECT g, sum(n) AS t, sum(sum(n)) OVER (ORDER BY g) FROM a GROUP BY g ORDER BY g`,
		`SELECT g, count(*) AS c, rank() OVER (ORDER BY count(*) DESC) FROM a GROUP BY g ORDER BY g`,
		`SELECT g, sum(n) AS t FROM a GROUP BY g HAVING sum(n) > 30 ORDER BY g`,
		`SELECT g, sum(n) AS t, first_value(sum(n)) OVER (ORDER BY g) FROM a GROUP BY g ORDER BY g`,
	})
}

func TestSQLiteParityWindowNamedAndChained(t *testing.T) {
	requireSQLite3(t)
	assertParity(t, []string{
		`SELECT id, sum(n) OVER w FROM a WINDOW w AS (PARTITION BY g) ORDER BY id`,
		`SELECT id, sum(n) OVER w, row_number() OVER w FROM a WINDOW w AS (ORDER BY id) ORDER BY id`,
		`SELECT id, sum(n) OVER (w) FROM a WINDOW w AS (PARTITION BY g ORDER BY id) ORDER BY id`,
		`SELECT id, sum(n) OVER w FROM a WINDOW w AS (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY id`,
		`SELECT id, n, sum(n) OVER (PARTITION BY g ORDER BY id), row_number() OVER (ORDER BY id) FROM a ORDER BY id`,
		`SELECT row_number() OVER (ORDER BY id) + 10, id FROM a ORDER BY id`,
		`SELECT id * 2, rank() OVER (ORDER BY n) FROM a ORDER BY n`,
	})
}
