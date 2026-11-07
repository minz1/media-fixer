package db

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var (
	ErrLocked      = errors.New("database locked by a migration; refusing to start")
	ErrDownRefused = errors.New("migrate down refused")
)

const lockFileMode = 0o600

func lockPath(path string) string { return path + ".lock" }

func takeLock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(lockPath(path), os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if exclusive {
				return nil, fmt.Errorf("%w: database in use; stop media-fixer first", ErrDownRefused)
			}
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", lockPath(path), err)
	}
	return f, nil
}
