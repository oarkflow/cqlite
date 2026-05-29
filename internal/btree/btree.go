package btree

import (
	"bytes"
	"sort"
	"sync"
)

type Tree struct {
	mu   sync.RWMutex
	keys [][]byte
	vals map[string][]byte
}

func New() *Tree {
	return &Tree{vals: make(map[string][]byte)}
}

func (t *Tree) Put(key, value []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := string(key)
	if _, ok := t.vals[k]; !ok {
		cp := append([]byte(nil), key...)
		t.keys = append(t.keys, cp)
		sort.Slice(t.keys, func(i, j int) bool { return bytes.Compare(t.keys[i], t.keys[j]) < 0 })
	}
	t.vals[k] = append([]byte(nil), value...)
}

func (t *Tree) Get(key []byte) ([]byte, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.vals[string(key)]
	return append([]byte(nil), v...), ok
}

func (t *Tree) Delete(key []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := string(key)
	if _, ok := t.vals[k]; !ok {
		return false
	}
	delete(t.vals, k)
	for i := range t.keys {
		if bytes.Equal(t.keys[i], key) {
			t.keys = append(t.keys[:i], t.keys[i+1:]...)
			break
		}
	}
	return true
}

func (t *Tree) Scan(start, end []byte, fn func(key, value []byte) bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, key := range t.keys {
		if len(start) > 0 && bytes.Compare(key, start) < 0 {
			continue
		}
		if len(end) > 0 && bytes.Compare(key, end) >= 0 {
			continue
		}
		if !fn(append([]byte(nil), key...), append([]byte(nil), t.vals[string(key)]...)) {
			return
		}
	}
}
