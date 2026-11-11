package btree

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func TestTreePutInt64PreservesSignedOrderAndValues(t *testing.T) {
	tr := NewWithOrder(4)
	keys := []int64{0, -1, math.MaxInt64, math.MinInt64, 17, -42}
	for i, key := range keys {
		tr.PutInt64(key, i+100)
	}
	tr.PutInt64(-1, 999)
	var got []int64
	tr.ViewScan(nil, nil, func(key, value []byte) bool {
		if len(key) != 8 || len(value) != 8 {
			t.Fatalf("integer entry widths = %d/%d, want 8/8", len(key), len(value))
		}
		got = append(got, int64(binary.BigEndian.Uint64(key)^(uint64(1)<<63)))
		return true
	})
	want := []int64{math.MinInt64, -42, -1, 0, 17, math.MaxInt64}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("integer key order = %v, want %v", got, want)
	}
	value, ok := tr.Get(encodeIntKeyForTest(-1))
	if !ok || len(value) != 8 {
		t.Fatalf("updated integer value = %v, ok=%v; want 8-byte value", value, ok)
	}
	if got := binary.LittleEndian.Uint64(value); got != 999 {
		t.Fatalf("updated integer value = %d, want 999", got)
	}
	for i := 0; i < 600; i++ {
		tr.PutInt64(int64(i), i*3)
	}
	for i := 0; i < 600; i++ {
		value, ok := tr.Get(encodeIntKeyForTest(int64(i)))
		if !ok || len(value) != 8 {
			t.Fatalf("integer key %d value = %v, ok=%v", i, value, ok)
		}
		if got := binary.LittleEndian.Uint64(value); got != uint64(i*3) {
			t.Fatalf("integer key %d value = %d", i, got)
		}
	}
}

func encodeIntKeyForTest(key int64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(key)^(uint64(1)<<63))
	return encoded[:]
}

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

func TestPutManyOwned(t *testing.T) {
	tree := NewWithOrder(8)
	tree.PutManyOwnedFunc(128, func(i int) ([]byte, []byte) {
		return []byte(fmt.Sprintf("%04d", i)), []byte(fmt.Sprintf("v%04d", i*3))
	})
	for i := 0; i < 128; i++ {
		got, ok := tree.Get([]byte(fmt.Sprintf("%04d", i)))
		want := fmt.Sprintf("v%04d", i*3)
		if !ok || string(got) != want {
			t.Fatalf("Get(%d) = %q, %v; want %q", i, got, ok, want)
		}
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
