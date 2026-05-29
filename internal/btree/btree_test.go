package btree

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func TestTreePutGetDeleteScan(t *testing.T) {
	tr := NewWithOrder(4)
	tr.Put([]byte("b"), []byte("2"))
	tr.Put([]byte("a"), []byte("1"))
	tr.Put([]byte("c"), []byte("3"))
	if v, ok := tr.Get([]byte("b")); !ok || string(v) != "2" {
		t.Fatalf("get b = %q %v", v, ok)
	}
	if ok := tr.View([]byte("b"), func(v []byte) {
		if string(v) != "2" {
			t.Fatalf("view b = %q", v)
		}
	}); !ok {
		t.Fatal("view b missing")
	}
	var keys string
	tr.Scan([]byte("a"), []byte("c"), func(k, v []byte) bool {
		keys += string(k)
		return true
	})
	if keys != "ab" {
		t.Fatalf("scan order = %q", keys)
	}
	if !tr.Delete([]byte("b")) {
		t.Fatal("delete failed")
	}
	if _, ok := tr.Get([]byte("b")); ok {
		t.Fatal("deleted key still present")
	}
}

func TestTreeSplitsAndScansInOrder(t *testing.T) {
	tr := NewWithOrder(4)
	for i := 99; i >= 0; i-- {
		k := []byte(fmt.Sprintf("%03d", i))
		tr.Put(k, []byte(fmt.Sprintf("v%d", i)))
	}
	for i := 0; i < 100; i++ {
		k := []byte(fmt.Sprintf("%03d", i))
		v, ok := tr.Get(k)
		if !ok || string(v) != fmt.Sprintf("v%d", i) {
			t.Fatalf("get %s = %q %v", k, v, ok)
		}
	}
	var got []string
	tr.Scan([]byte("020"), []byte("030"), func(k, v []byte) bool {
		got = append(got, string(k))
		return true
	})
	if len(got) != 10 || got[0] != "020" || got[9] != "029" {
		t.Fatalf("range scan = %#v", got)
	}
}

func TestTreeSplitsThenCompactsAfterDeletes(t *testing.T) {
	tr := NewWithOrder(4)
	for i := 0; i < 80; i++ {
		key := []byte(fmt.Sprintf("%03d", i))
		tr.Put(key, []byte(fmt.Sprintf("v%03d", i)))
	}
	if tr.root.leaf {
		t.Fatal("expected inserts to split root")
	}
	for i := 0; i < 76; i++ {
		key := []byte(fmt.Sprintf("%03d", i))
		if !tr.Delete(key) {
			t.Fatalf("delete %s failed", key)
		}
	}
	if !tr.root.leaf {
		t.Fatal("expected delete compaction to collapse tree to one leaf")
	}
	want := map[string]string{
		"076": "v076",
		"077": "v077",
		"078": "v078",
		"079": "v079",
	}
	assertTreeMatchesMap(t, tr, want)
}

func TestTreeRandomOperations(t *testing.T) {
	tr := NewWithOrder(5)
	want := make(map[string]string)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("%03d", rng.Intn(150))
		switch rng.Intn(3) {
		case 0:
			deleted := tr.Delete([]byte(key))
			_, existed := want[key]
			if deleted != existed {
				t.Fatalf("delete %s = %v, want %v", key, deleted, existed)
			}
			delete(want, key)
		default:
			value := fmt.Sprintf("v%03d", i)
			tr.Put([]byte(key), []byte(value))
			want[key] = value
		}
		assertTreeMatchesMap(t, tr, want)
	}
}

func assertTreeMatchesMap(t *testing.T, tr *Tree, want map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v, ok := tr.Get([]byte(key))
		if !ok || string(v) != want[key] {
			t.Fatalf("get %s = %q %v, want %q", key, v, ok, want[key])
		}
	}
	var scanned []string
	tr.Scan(nil, nil, func(k, v []byte) bool {
		scanned = append(scanned, string(k))
		if string(v) != want[string(k)] {
			t.Fatalf("scan value for %s = %q, want %q", k, v, want[string(k)])
		}
		return true
	})
	if fmt.Sprint(scanned) != fmt.Sprint(keys) {
		t.Fatalf("scan keys = %#v, want %#v", scanned, keys)
	}
}
