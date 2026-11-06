//go:build !windows

package storage

import (
	"errors"
	"syscall"
)

func isOSDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
