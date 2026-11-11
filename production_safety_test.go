package cqlite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoBenchmarkShapedRuntimeShortcuts(t *testing.T) {
	root := "."
	banned := []string{
		"execHotDirect",
		"fastHotQuery",
		"bench_insert",
		"items set score",
	}
	allowed := map[string]bool{
		"benchmark_test.go":         true,
		"production_safety_test.go": true,
		"crash_data_test.go":        true,
		"Tasks.md":                  true,
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".gocache":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		base := filepath.Base(path)
		if allowed[base] || !strings.HasSuffix(base, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := strings.ToLower(string(data))
		for _, token := range banned {
			if strings.Contains(text, strings.ToLower(token)) {
				t.Fatalf("benchmark-shaped runtime shortcut %q found in %s", token, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
