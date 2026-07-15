package btree

import (
	"bytes"
	"sync"
)

const defaultOrder = 16

type Tree struct {
	mu    sync.RWMutex
	order int
	root  *node
}

type node struct {
	leaf     bool
	keys     [][]byte
	values   [][]byte
	children []*node
	next     *node
}

type splitResult struct {
	key   []byte
	right *node
}

func New() *Tree {
	return NewWithOrder(defaultOrder)
}

func NewWithOrder(order int) *Tree {
	if order < 4 {
		order = 4
	}
	return &Tree{order: order, root: &node{leaf: true}}
}

func (t *Tree) Put(key, value []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if res := t.insert(t.root, clone(key), clone(value)); res != nil {
		t.root = &node{
			keys:     [][]byte{res.key},
			children: []*node{t.root, res.right},
		}
	}
}

func (t *Tree) PutOwned(key, value []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if res := t.insert(t.root, key, value); res != nil {
		t.root = &node{
			keys:     [][]byte{res.key},
			children: []*node{t.root, res.right},
		}
	}
}

func (t *Tree) Get(key []byte) ([]byte, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	leaf := findLeaf(t.root, key)
	pos, found := searchKeys(leaf.keys, key)
	if !found {
		return nil, false
	}
	return clone(leaf.values[pos]), true
}

func (t *Tree) View(key []byte, fn func([]byte)) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	leaf := findLeaf(t.root, key)
	pos, found := searchKeys(leaf.keys, key)
	if !found {
		return false
	}
	fn(leaf.values[pos])
	return true
}

func (t *Tree) Delete(key []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := t.entriesLocked()
	found := false
	t.root = &node{leaf: true}
	for _, entry := range entries {
		if bytes.Equal(entry.key, key) {
			found = true
			continue
		}
		if res := t.insert(t.root, entry.key, entry.value); res != nil {
			t.root = &node{keys: [][]byte{res.key}, children: []*node{t.root, res.right}}
		}
	}
	return found
}

func (t *Tree) Scan(start, end []byte, fn func(key, value []byte) bool) {
	t.scan(start, end, func(key, value []byte) bool {
		return fn(clone(key), clone(value))
	})
}

func (t *Tree) ViewScan(start, end []byte, fn func(key, value []byte) bool) {
	t.scan(start, end, fn)
}

func (t *Tree) scan(start, end []byte, fn func(key, value []byte) bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	leaf := t.leftmostLeaf()
	if len(start) > 0 {
		leaf = findLeaf(t.root, start)
	}
	for leaf != nil {
		for i, key := range leaf.keys {
			if len(start) > 0 && bytes.Compare(key, start) < 0 {
				continue
			}
			if len(end) > 0 && bytes.Compare(key, end) >= 0 {
				return
			}
			if !fn(key, leaf.values[i]) {
				return
			}
		}
		leaf = leaf.next
	}
}

func (t *Tree) insert(n *node, key, value []byte) *splitResult {
	if n.leaf {
		pos, found := searchKeys(n.keys, key)
		if found {
			n.values[pos] = value
			return nil
		}
		n.keys = insertBytes(n.keys, pos, key)
		n.values = insertBytes(n.values, pos, value)
		if len(n.keys) <= t.order {
			return nil
		}
		return splitLeaf(n)
	}
	pos := childPos(n.keys, key)
	res := t.insert(n.children[pos], key, value)
	if res == nil {
		return nil
	}
	n.keys = insertBytes(n.keys, pos, res.key)
	n.children = insertChild(n.children, pos+1, res.right)
	if len(n.keys) <= t.order {
		return nil
	}
	return splitInternal(n)
}

func splitLeaf(left *node) *splitResult {
	mid := len(left.keys) / 2
	right := &node{
		leaf:   true,
		keys:   cloneKeys(left.keys[mid:]),
		values: cloneKeys(left.values[mid:]),
		next:   left.next,
	}
	left.keys = left.keys[:mid]
	left.values = left.values[:mid]
	left.next = right
	return &splitResult{key: clone(right.keys[0]), right: right}
}

func splitInternal(left *node) *splitResult {
	mid := len(left.keys) / 2
	promoted := clone(left.keys[mid])
	right := &node{
		keys:     cloneKeys(left.keys[mid+1:]),
		children: append([]*node(nil), left.children[mid+1:]...),
	}
	left.keys = left.keys[:mid]
	left.children = left.children[:mid+1]
	return &splitResult{key: promoted, right: right}
}

func findLeaf(n *node, key []byte) *node {
	for !n.leaf {
		n = n.children[childPos(n.keys, key)]
	}
	return n
}

func (t *Tree) leftmostLeaf() *node {
	n := t.root
	for n != nil && !n.leaf {
		n = n.children[0]
	}
	return n
}

type entry struct {
	key   []byte
	value []byte
}

func (t *Tree) entriesLocked() []entry {
	var out []entry
	for leaf := t.leftmostLeaf(); leaf != nil; leaf = leaf.next {
		for i := range leaf.keys {
			out = append(out, entry{key: clone(leaf.keys[i]), value: clone(leaf.values[i])})
		}
	}
	return out
}

func searchKeys(keys [][]byte, key []byte) (int, bool) {
	n := len(keys)
	if n <= 16 {
		for i, k := range keys {
			cmp := bytes.Compare(k, key)
			if cmp == 0 {
				return i, true
			}
			if cmp > 0 {
				return i, false
			}
		}
		return n, false
	}
	lo, hi := 0, n
	for lo < hi {
		mid := lo + (hi-lo)/2
		cmp := bytes.Compare(keys[mid], key)
		if cmp < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(keys) && bytes.Equal(keys[lo], key)
}

func childPos(keys [][]byte, key []byte) int {
	pos, found := searchKeys(keys, key)
	if found {
		return pos + 1
	}
	return pos
}

func insertBytes(in [][]byte, pos int, value []byte) [][]byte {
	in = append(in, nil)
	copy(in[pos+1:], in[pos:])
	in[pos] = value
	return in
}

func insertChild(in []*node, pos int, value *node) []*node {
	in = append(in, nil)
	copy(in[pos+1:], in[pos:])
	in[pos] = value
	return in
}

func cloneKeys(in [][]byte) [][]byte {
	n := len(in)
	if n == 0 {
		return nil
	}
	out := make([][]byte, n)
	copy(out, in)
	return out
}

func clone(in []byte) []byte {
	if in == nil {
		return nil
	}
	l := len(in)
	if l == 0 {
		return []byte{}
	}
	out := make([]byte, l)
	copy(out, in)
	return out
}
