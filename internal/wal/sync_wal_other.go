//go:build !linux && !darwin

package wal

import "os"

func syncWALData(f *os.File) error {
	return f.Sync()
}
