package storage

import (
	"fmt"
	"os"
)

// OpenFile opens the data file at path read-write, creating an empty file if
// it does not exist. It never uses O_APPEND: the engine owns the append
// offset and writes with WriteAt.
//
// A just-created file is empty. Writing its header is recovery's job, so a
// crash between creation and header write is handled by the same code path
// that handles every other torn start.
func OpenFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	return f, nil
}

// WriteHeader writes h at offset 0 and syncs it to disk.
func WriteHeader(f *os.File, h Header) error {
	b := h.Encode()
	if _, err := f.WriteAt(b[:], 0); err != nil {
		return fmt.Errorf("storage: write header: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("storage: sync header: %w", err)
	}
	return nil
}
