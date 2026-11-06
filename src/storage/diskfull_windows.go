//go:build windows

package storage

import (
	"errors"
	"syscall"
)

const (
	errHandleDiskFull = syscall.Errno(39)  // ERROR_HANDLE_DISK_FULL
	errDiskFull       = syscall.Errno(112) // ERROR_DISK_FULL
)

func isOSDiskFull(err error) bool {
	return errors.Is(err, errDiskFull) || errors.Is(err, errHandleDiskFull)
}
