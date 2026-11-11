//go:build linux

package wal

import (
	"os"
	"syscall"
)

func syncWALData(f *os.File) error {
	return syscall.Fdatasync(int(f.Fd()))
}
