package cqlite

import (
	"time"

	isql "github.com/oarkflow/cqlite/internal/sql"
	"github.com/oarkflow/cqlite/internal/txn"
)

type SyncPolicy string

const (
	SyncFull   SyncPolicy = "FULL"
	SyncNormal SyncPolicy = "NORMAL"
	SyncOff    SyncPolicy = "OFF"
)

type Options struct {
	CompatibilityMode       string
	PageSize                int
	CacheSize               int
	JournalMode             string
	Synchronous             string
	BufferPoolSize          int
	SyncPolicy              SyncPolicy
	DisableFileLock         bool
	CheckpointIntervalBytes int64
	CheckpointIntervalTx    uint64
	// BusyTimeout bounds how long a writer waits for another process to release
	// the database writer lock. Zero waits until the lock is released; the
	// operation context can still cancel the wait.
	BusyTimeout time.Duration
}

type TxOptions struct {
	Isolation txn.Isolation
}

type Result = isql.Result
type Rows = isql.Rows

type Value any

type NamedArg struct {
	Name  string
	Value any
}

func Named(name string, value any) NamedArg {
	return NamedArg{Name: name, Value: value}
}

type Stats struct {
	Tables       int
	Transactions uint64
	OpenedAt     time.Time
}
