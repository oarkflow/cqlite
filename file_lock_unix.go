//go:build !windows

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireDatabaseLock(path string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(path, "db.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %v", ErrDatabaseLocked, err)
	}
	return f, nil
}

func releaseDatabaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
