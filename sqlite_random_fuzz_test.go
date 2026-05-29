package db

import (
	"fmt"
	"os/exec"
	"testing"
)

func FuzzSQLiteRandomOperationSequences(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{9, 0, 9, 1, 9, 2, 9, 3})
	f.Fuzz(func(t *testing.T, program []byte) {
		if _, err := exec.LookPath("sqlite3"); err != nil {
			t.Skip("sqlite3 binary not available")
		}
		if len(program) > 64 {
			program = program[:64]
		}
		setup := randomOperationScript(program)
		queries := []string{
			`SELECT id, bucket, name FROM fuzz_items ORDER BY id`,
			`SELECT id, name FROM fuzz_items WHERE bucket = 2 ORDER BY id`,
			`SELECT bucket, count(*) FROM fuzz_items GROUP BY bucket ORDER BY bucket`,
		}
		custom := runCustomDB(t, setup, queries)
		sqlite := runSQLite(t, setup, queries)
		if fmt.Sprint(custom) != fmt.Sprint(sqlite) {
			t.Fatalf("random operation mismatch\nprogram: %#v\ncustom: %#v\nsqlite: %#v", program, custom, sqlite)
		}
	})
}

func randomOperationScript(program []byte) []string {
	setup := []string{
		`CREATE TABLE fuzz_items (id INTEGER PRIMARY KEY, bucket INTEGER, name TEXT)`,
		`CREATE INDEX fuzz_items_bucket_idx ON fuzz_items (bucket)`,
	}
	live := make(map[int]bool)
	for i, b := range program {
		id := int(b%16) + 1
		bucket := int((b / 16) % 4)
		switch b % 4 {
		case 0:
			setup = append(setup, fmt.Sprintf(`DELETE FROM fuzz_items WHERE id = %d`, id))
			delete(live, id)
		case 1:
			if live[id] {
				setup = append(setup, fmt.Sprintf(`UPDATE fuzz_items SET bucket = %d, name = 'u%d' WHERE id = %d`, bucket, i, id))
			}
		case 2:
			if !live[id] {
				setup = append(setup, fmt.Sprintf(`INSERT INTO fuzz_items (id, bucket, name) VALUES (%d, %d, 'n%d')`, id, bucket, i))
				live[id] = true
			}
		default:
			if live[id] {
				setup = append(setup, fmt.Sprintf(`INSERT OR REPLACE INTO fuzz_items (id, bucket, name) VALUES (%d, %d, 'r%d')`, id, bucket, i))
			} else {
				setup = append(setup, fmt.Sprintf(`INSERT INTO fuzz_items (id, bucket, name) VALUES (%d, %d, 'r%d')`, id, bucket, i))
				live[id] = true
			}
		}
	}
	return setup
}
