//go:build windows

package lock

import (
	"errors"
	"fmt"
	"syscall"
)

// Lock is a held process lock.
type Lock struct {
	h syscall.Handle
}

// A handle opened with share mode 0 excludes every other open of the file.
const errSharingViolation = syscall.Errno(32)

func tryLock(path string) (*Lock, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, errHeld
		}
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}
	return &Lock{h: h}, nil
}

// Release drops the lock. The lock file itself is left in place.
func (l *Lock) Release() error {
	if err := syscall.CloseHandle(l.h); err != nil {
		return fmt.Errorf("lock: release: %w", err)
	}
	return nil
}
