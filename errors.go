package cqlite

import "errors"

var (
	ErrDatabaseLocked       = errors.New("database is locked")
	ErrTransactionActive    = errors.New("transaction already active")
	ErrReadOnlyUnsupported  = errors.New("read-only transactions are not supported")
	ErrIsolationUnsupported = errors.New("unsupported transaction isolation level")
)
