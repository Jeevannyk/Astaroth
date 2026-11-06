//go:build windows

package storage

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var procMoveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8

	errAccessDenied     = syscall.Errno(5)
	errSharingViolation = syscall.Errno(32)
)

// ReplaceFile atomically replaces dst with src, in one MoveFileEx call with
// MOVEFILE_REPLACE_EXISTING: dst names either the old file or the new one,
// never neither. MOVEFILE_WRITE_THROUGH makes the call return only after the
// rename has been flushed to disk; it stands in for the directory fsync that
// Windows does not offer through os.
//
// Both files must be closed. Another program (an indexer or virus scanner)
// briefly opening one of them makes the call fail with a sharing or access
// error, so those are retried for a short while.
func ReplaceFile(src, dst string) error {
	from, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		r, _, e := procMoveFileExW.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)),
			moveFileReplaceExisting|moveFileWriteThrough)
		if r != 0 {
			return nil
		}
		if attempt < 20 && (errors.Is(e, errSharingViolation) || errors.Is(e, errAccessDenied)) {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		return fmt.Errorf("storage: replace %s with %s: %w", dst, src, e)
	}
}

// SyncDir is a no-op on Windows: a directory cannot be fsynced through os, and
// ReplaceFile already makes the rename durable with MOVEFILE_WRITE_THROUGH.
func SyncDir(dir string) error { return nil }
