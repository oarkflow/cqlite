//go:build windows

package db

type databaseLock struct{}

func acquireDatabaseLock(path string) (*databaseLock, error) {
	return &databaseLock{}, nil
}

func acquireDatabaseWriteLock(l *databaseLock) error {
	return nil
}

func releaseDatabaseWriteLock(l *databaseLock) error {
	return nil
}

func releaseDatabaseLock(l *databaseLock) error {
	return nil
}
