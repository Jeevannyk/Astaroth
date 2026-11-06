// Package lock provides the cross-process lock that keeps two processes from
// opening the same database. It lives on a separate file so it survives the
// data-file swap done by compaction. The OS releases it when the process dies,
// so a killed process never leaves a stale lock behind.
package lock

import (
	"errors"
	"time"
)

// ErrLocked means another handle or process holds the lock.
var ErrLocked = errors.New("lock: held by another process")

// errHeld is returned by tryLock when the lock is currently taken.
var errHeld = errors.New("lock: held")

const retryInterval = 10 * time.Millisecond

// Acquire takes an exclusive lock on the file at path, creating it if needed.
// It retries for up to timeout, because after a kill the OS may release the
// dead process's handle a moment late. It returns ErrLocked if still held.
func Acquire(path string, timeout time.Duration) (*Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := tryLock(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, errHeld) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, ErrLocked
		}
		time.Sleep(retryInterval)
	}
}
