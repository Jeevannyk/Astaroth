// Package tests holds black-box tests that use only the public engine API and
// the on-disk bytes. Per-package unit tests live next to their code.
//
// Tests of normal behaviour use the one permanent database, data/astaroth.db
// (see testdb), and only assert on keys unique to the run. Tests that damage,
// copy or delete files work on t.TempDir() and never touch it.
package tests

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"astaroth/src/engine"
	"astaroth/src/lock"
	"astaroth/src/recovery"
	"astaroth/src/storage"
	"astaroth/tests/testdb"
)

// Short lock timeout so "already open" cases do not slow the suite.
var fast = engine.Options{LockTimeout: 50 * time.Millisecond}

func newPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

func mustOpen(t *testing.T, path string) *engine.DB {
	t.Helper()
	db, err := engine.Open(path, fast)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The permanent file already holds data from earlier runs, so these tests make
// no assumption about its size or key count.
func TestOpenPermanentFileHasValidHeader(t *testing.T) {
	db := openPerm(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(testdb.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw := make([]byte, storage.HeaderSize)
	if _, err := f.ReadAt(raw, 0); err != nil {
		t.Fatalf("file is shorter than a header: %v", err)
	}
	h, err := storage.DecodeHeader(raw)
	if err != nil {
		t.Fatalf("header invalid: %v", err)
	}
	if h.Version != storage.Version || h.Generation < 1 {
		t.Fatalf("header = %+v", h)
	}
}

func TestReopenLeavesFileUnchanged(t *testing.T) {
	openPerm(t).Close()
	path := testdb.Path(t)
	first := fileHash(t, path)

	for i := 0; i < 3; i++ {
		db := openPerm(t)
		if err := db.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
	if first != fileHash(t, path) {
		t.Fatal("reopening changed the file")
	}
}

func TestCloseTwiceReturnsErrClosed(t *testing.T) {
	db := openPerm(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
}

func TestUseAfterCloseReturnsErrClosed(t *testing.T) {
	db := openPerm(t)
	db.Close()
	if _, err := db.Len(); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("Len after Close = %v, want ErrClosed", err)
	}
}

func TestSecondOpenWhileOpenIsLocked(t *testing.T) {
	db := openPerm(t)
	path := testdb.Path(t)

	if _, err := engine.Open(path, fast); !errors.Is(err, lock.ErrLocked) {
		t.Fatalf("second Open = %v, want lock.ErrLocked", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	openPerm(t).Close() // lock released by Close
}

// Deletes the file on purpose, so it runs on a temp dir, never on data/.
func TestCloseReleasesFileForDeletion(t *testing.T) {
	path := newPath(t)
	mustOpen(t, path).Close()
	if err := os.Remove(path); err != nil {
		t.Fatalf("data file still held after Close: %v", err)
	}
}

func TestOpenRepairsTornHeader(t *testing.T) {
	want := storage.NewHeader().Encode()
	for _, n := range []int{0, 1, 8, 31} {
		path := newPath(t)
		if err := os.WriteFile(path, want[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		mustOpen(t, path).Close()
		if !bytes.Equal(readFile(t, path), want[:]) {
			t.Fatalf("torn header of %d bytes not repaired", n)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("empty path", func(t *testing.T) {
		if _, err := engine.Open("", fast); !errors.Is(err, engine.ErrInvalidPath) {
			t.Fatalf("err = %v, want ErrInvalidPath", err)
		}
	})

	t.Run("negative MaxBytes", func(t *testing.T) {
		_, err := engine.Open(filepath.Join(dir, "a"), engine.Options{MaxBytes: -1})
		if !errors.Is(err, engine.ErrInvalidOptions) {
			t.Fatalf("err = %v, want ErrInvalidOptions", err)
		}
	})

	t.Run("negative LockTimeout", func(t *testing.T) {
		_, err := engine.Open(filepath.Join(dir, "b"), engine.Options{LockTimeout: -1})
		if !errors.Is(err, engine.ErrInvalidOptions) {
			t.Fatalf("err = %v, want ErrInvalidOptions", err)
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		if _, err := engine.Open(filepath.Join(dir, "nope", "db"), fast); err == nil {
			t.Fatal("Open succeeded in a missing directory")
		}
	})

	t.Run("max bytes accepted", func(t *testing.T) {
		db, err := engine.Open(testdb.Path(t), engine.Options{MaxBytes: 50_000_000, LockTimeout: permOpts.LockTimeout})
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	})
}

// A rejected file must stay untouched, and a failed Open must not leave the
// process lock held.
func TestFailedOpenReleasesLockAndKeepsFile(t *testing.T) {
	path := newPath(t)
	foreign := bytes.Repeat([]byte{0xAB}, 100)
	if err := os.WriteFile(path, foreign, 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // a second failure would be ErrLocked if the lock leaked
		if _, err := engine.Open(path, fast); !errors.Is(err, recovery.ErrNotOurFile) {
			t.Fatalf("attempt %d: err = %v, want ErrNotOurFile", i, err)
		}
	}
	if !bytes.Equal(foreign, readFile(t, path)) {
		t.Fatal("foreign file was modified")
	}
}

func TestOpenTruncatesGarbageAfterHeader(t *testing.T) {
	path := newPath(t)
	h := storage.NewHeader().Encode()
	if err := os.WriteFile(path, append(h[:], 1, 2, 3), 0o644); err != nil {
		t.Fatal(err)
	}
	db := mustOpen(t, path)
	if n, _ := db.Len(); n != 0 {
		t.Fatalf("Len = %d", n)
	}
	db.Close()
	if !bytes.Equal(readFile(t, path), h[:]) {
		t.Fatal("garbage after the header was not removed")
	}
}
