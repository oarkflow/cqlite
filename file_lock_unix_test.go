//go:build !windows

package cqlite

import (
	"errors"
	"syscall"
	"testing"
)

func TestClassifyFlockError(t *testing.T) {
	if err := classifyFlockError(syscall.EWOULDBLOCK); !errors.Is(err, ErrDatabaseLocked) {
		t.Fatalf("contention error = %v, want ErrDatabaseLocked", err)
	}
	if err := classifyFlockError(syscall.EBADF); !errors.Is(err, syscall.EBADF) || errors.Is(err, ErrDatabaseLocked) {
		t.Fatalf("permanent error = %v, want EBADF without ErrDatabaseLocked", err)
	}
}
