//go:build linux || darwin || freebsd || netbsd || openbsd

package lock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Lock is a held process lock.
type Lock struct {
	f *os.File
}

func tryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errHeld
		}
		return nil, fmt.Errorf("lock: flock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. The lock file itself is left in place.
func (l *Lock) Release() error {
	// Closing the descriptor releases the flock.
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("lock: release: %w", err)
	}
	return nil
}
