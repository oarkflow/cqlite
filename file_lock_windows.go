//go:build windows

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	lockFileFailImmediately = 0x00000001
	lockFileExclusive       = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32LockFileEx   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	kernel32UnlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

type databaseLock struct {
	shared       *os.File
	writer       *os.File
	sharedOvl    syscall.Overlapped
	writerOvl    syscall.Overlapped
	sharedLocked bool
	writerLocked bool
}

func acquireDatabaseLock(path string) (*databaseLock, error) {
	shared, err := os.OpenFile(filepath.Join(path, "db.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &databaseLock{shared: shared}
	if err := lockFileRange(shared, &lock.sharedOvl, 0); err != nil {
		_ = shared.Close()
		return nil, err
	}
	lock.sharedLocked = true
	writer, err := os.OpenFile(filepath.Join(path, "db.write.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = unlockFileRange(shared, &lock.sharedOvl)
		_ = shared.Close()
		return nil, err
	}
	lock.writer = writer
	return lock, nil
}

func acquireDatabaseWriteLock(lock *databaseLock) error {
	if lock == nil || lock.writer == nil {
		return nil
	}
	if err := lockFileRange(lock.writer, &lock.writerOvl, lockFileFailImmediately|lockFileExclusive); err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == errorLockViolation {
			return fmt.Errorf("%w: %v", ErrDatabaseLocked, err)
		}
		return err
	}
	lock.writerLocked = true
	return nil
}

func releaseDatabaseWriteLock(lock *databaseLock) error {
	if lock == nil || lock.writer == nil || !lock.writerLocked {
		return nil
	}
	err := unlockFileRange(lock.writer, &lock.writerOvl)
	if err == nil {
		lock.writerLocked = false
	}
	return err
}

func releaseDatabaseLock(lock *databaseLock) error {
	if lock == nil {
		return nil
	}
	writeUnlockErr := releaseDatabaseWriteLock(lock)
	var writeCloseErr error
	if lock.writer != nil {
		writeCloseErr = lock.writer.Close()
	}
	var sharedUnlockErr error
	if lock.shared != nil && lock.sharedLocked {
		sharedUnlockErr = unlockFileRange(lock.shared, &lock.sharedOvl)
		if sharedUnlockErr == nil {
			lock.sharedLocked = false
		}
	}
	var sharedCloseErr error
	if lock.shared != nil {
		sharedCloseErr = lock.shared.Close()
	}
	for _, err := range []error{writeUnlockErr, writeCloseErr, sharedUnlockErr, sharedCloseErr} {
		if err != nil {
			return err
		}
	}
	return nil
}

func lockFileRange(file *os.File, overlapped *syscall.Overlapped, flags uintptr) error {
	r1, _, callErr := kernel32LockFileEx.Call(
		file.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(overlapped)),
	)
	if r1 != 0 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}

func unlockFileRange(file *os.File, overlapped *syscall.Overlapped) error {
	r1, _, callErr := kernel32UnlockFileEx.Call(
		file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(overlapped)),
	)
	if r1 != 0 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}
