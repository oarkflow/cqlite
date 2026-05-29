package db

import (
	"time"

	isql "github.com/sujit/sqlite/internal/sql"
	"github.com/sujit/sqlite/internal/txn"
)

type SyncPolicy string

const (
	SyncFull   SyncPolicy = "FULL"
	SyncNormal SyncPolicy = "NORMAL"
	SyncOff    SyncPolicy = "OFF"
)

type Options struct {
	CompatibilityMode string
	PageSize          int
	CacheSize         int
	JournalMode       string
	Synchronous       string
	BufferPoolSize    int
	SyncPolicy        SyncPolicy
	DisableFileLock   bool
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
