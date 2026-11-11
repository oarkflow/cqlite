//go:build !darwin && !linux

package wal

import "os"

func fileSize(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
