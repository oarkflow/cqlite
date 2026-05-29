package btree

import "testing"

func TestTreePutGetDeleteScan(t *testing.T) {
	tr := New()
	tr.Put([]byte("b"), []byte("2"))
	tr.Put([]byte("a"), []byte("1"))
	tr.Put([]byte("c"), []byte("3"))
	if v, ok := tr.Get([]byte("b")); !ok || string(v) != "2" {
		t.Fatalf("get b = %q %v", v, ok)
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
