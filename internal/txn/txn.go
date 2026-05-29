package txn

import "sync/atomic"

type Isolation string

const (
	SnapshotIsolation     Isolation = "SNAPSHOT"
	SerializableIsolation Isolation = "SERIALIZABLE"
)

type Manager struct {
	next atomic.Uint64
}

type Tx struct {
	ID        uint64
	Snapshot  uint64
	Isolation Isolation
	closed    bool
}

func NewManager() *Manager {
	m := &Manager{}
	m.next.Store(1)
	return m
}

func (m *Manager) Begin(iso Isolation) *Tx {
	id := m.next.Add(1)
	if iso == "" {
		iso = SnapshotIsolation
	}
	return &Tx{ID: id, Snapshot: id - 1, Isolation: iso}
}

func (t *Tx) Closed() bool { return t.closed }
func (t *Tx) Close()       { t.closed = true }
