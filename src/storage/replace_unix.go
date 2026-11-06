//go:build !windows

package storage

import (
	"fmt"
	"os"
)

// ReplaceFile atomically replaces dst with src using rename(2): dst names
// either the old file or the new one, never neither. The rename is durable
// only after SyncDir on the containing directory.
func ReplaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("storage: replace %s with %s: %w", dst, src, err)
	}
	return nil
}

// SyncDir fsyncs a directory, making a rename or file creation in it durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("storage: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("storage: sync dir %s: %w", dir, err)
	}
	return nil
}
