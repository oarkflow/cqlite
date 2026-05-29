//go:build windows

package db

import "os"

func acquireDatabaseLock(path string) (*os.File, error) {
	return nil, nil
}

func releaseDatabaseLock(f *os.File) error {
	return nil
}
