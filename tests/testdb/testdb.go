// Package testdb gives tests the one permanent database file,
// data/astaroth.db in the project root. The file is never deleted or
// truncated by tests: they open it, write keys that are unique to the run, and
// close it again.
//
// Tests that destroy data on purpose (truncation, corruption, deleting the
// file) must NOT use this package; they work on files under t.TempDir().
package testdb

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"astaroth/src/lock"
)

// runID is unique to this test process: a timestamp plus the process id.
var runID = fmt.Sprintf("%s-p%d", time.Now().UTC().Format("20060102T150405.000000"), os.Getpid())

var (
	once    sync.Once
	dbPath  string
	initErr error
)

// RunID identifies this test process. Keys that contain it cannot collide with
// keys left in the permanent file by earlier runs.
func RunID() string { return runID }

// Prefix returns a key prefix unique to this run and this test.
func Prefix(t testing.TB) string {
	return runID + "|" + t.Name() + "|"
}

// Path returns the path of the permanent database file, creating the data/
// directory if needed. It does not create or open the file itself.
//
// The first call also takes a lock that is held until the test process exits.
// `go test ./...` runs packages as parallel processes; without this they would
// race for the database's own lock. The packages take turns instead.
func Path(t testing.TB) string {
	t.Helper()
	once.Do(func() { dbPath, initErr = setup() })
	if initErr != nil {
		t.Fatalf("testdb: %v", initErr)
	}
	return dbPath
}

func setup() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Deliberately never released: the OS drops it when the test process exits.
	if _, err := lock.Acquire(filepath.Join(dir, "tests.lock"), 10*time.Minute); err != nil {
		return "", fmt.Errorf("waiting for another test process: %w", err)
	}
	return filepath.Join(dir, "astaroth.db"), nil
}

// moduleRoot finds the directory holding go.mod, starting from the test's
// working directory (which `go test` sets to the package directory).
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above the working directory")
		}
		dir = parent
	}
}
