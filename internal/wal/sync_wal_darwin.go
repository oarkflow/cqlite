//go:build darwin

package wal

import (
	"os"
	"syscall"
)

func syncWALData(f *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FDATASYNC, f.Fd(), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
