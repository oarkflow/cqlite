//go:build !windows

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type databaseLock struct {
	shared *os.File
	writer *os.File
}

func acquireDatabaseLock(path string) (*databaseLock, error) {
	shared, err := os.OpenFile(filepath.Join(path, "db.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(shared.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		_ = shared.Close()
		return nil, fmt.Errorf("%w: %v", ErrDatabaseLocked, err)
	}
	writer, err := os.OpenFile(filepath.Join(path, "db.write.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = syscall.Flock(int(shared.Fd()), syscall.LOCK_UN)
		_ = shared.Close()
		return nil, err
	}
	return &databaseLock{shared: shared, writer: writer}, nil
}

func acquireDatabaseWriteLock(l *databaseLock) error {
	if l == nil || l.writer == nil {
		return nil
	}
	if err := syscall.Flock(int(l.writer.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseLocked, err)
	}
	return nil
}

func releaseDatabaseWriteLock(l *databaseLock) error {
	if l == nil || l.writer == nil {
		return nil
	}
	return syscall.Flock(int(l.writer.Fd()), syscall.LOCK_UN)
}

func releaseDatabaseLock(l *databaseLock) error {
	if l == nil {
		return nil
	}
	writeUnlockErr := releaseDatabaseWriteLock(l)
	var writeCloseErr error
	if l.writer != nil {
		writeCloseErr = l.writer.Close()
	}
	unlockErr := syscall.Flock(int(l.shared.Fd()), syscall.LOCK_UN)
	closeErr := l.shared.Close()
	if writeUnlockErr != nil {
		return writeUnlockErr
	}
	if writeCloseErr != nil {
		return writeCloseErr
	}
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
