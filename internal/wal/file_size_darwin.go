//go:build darwin

package wal

import (
	"os"
	"syscall"
)

func fileSize(f *os.File) (int64, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &stat); err != nil {
		return 0, err
	}
	return stat.Size, nil
}
