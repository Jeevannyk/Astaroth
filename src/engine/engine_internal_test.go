package engine

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"astaroth/src/storage"
)

func openInternal(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := Open(path, Options{LockTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return db, path
}

// abandon drops the handle the way a killed process does: no Sync, no
// bookkeeping, just the OS releasing the file and the lock.
func abandon(db *DB) {
	db.f.Close()
	db.lock.Release()
}

// A write that returned nil is on disk even if the process dies right after.
func TestAckedWritesSurviveAbandonedHandle(t *testing.T) {
	db, path := openInternal(t)
	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"a", "3"}} {
		if err := db.Put([]byte(kv[0]), []byte(kv[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Delete([]byte("b")); err != nil {
		t.Fatal(err)
	}
	abandon(db)

	db2, err := Open(path, Options{LockTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("reopen after abandoned handle: %v", err)
	}
	defer db2.Close()
	if v, err := db2.Get([]byte("a")); err != nil || string(v) != "3" {
		t.Fatalf("a = %q, %v; want 3", v, err)
	}
	if _, err := db2.Get([]byte("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("b: err = %v, want ErrNotFound", err)
	}
}

// If the file cannot be written, the write fails, nothing becomes visible, and
// the handle refuses further writes instead of risking the file.
func TestWriteFailurePoisonsHandle(t *testing.T) {
	db, _ := openInternal(t)
	defer db.lock.Release()

	db.f.Close() // pull the file out from under the engine

	err := db.Put([]byte("k"), []byte("v"))
	if err == nil {
		t.Fatal("Put succeeded on a closed file")
	}
	if n, _ := db.Len(); n != 0 {
		t.Fatalf("failed write became visible: Len = %d", n)
	}
	if err := db.Put([]byte("k2"), []byte("v")); !errors.Is(err, ErrFailed) {
		t.Fatalf("second Put = %v, want ErrFailed", err)
	}
	if err := db.Delete([]byte("k")); !errors.Is(err, ErrFailed) {
		t.Fatalf("Delete = %v, want ErrFailed", err)
	}
}

// If the index ever pointed at the wrong record, Get must say so rather than
// hand back another key's value.
func TestGetDetectsIndexMismatch(t *testing.T) {
	db, _ := openInternal(t)
	defer db.Close()
	if err := db.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("b"), []byte("2")); err != nil {
		t.Fatal(err)
	}
	eb, _ := db.idx.Get([]byte("b"))

	// Point key "a" at b's record.
	db.idx.Apply(eb.RecOff-storage.FrameHeaderSize, []storage.Op{
		{Type: storage.OpPut, Key: []byte("a"), Value: []byte("2"), Off: storage.FrameHeaderSize},
	})
	if _, err := db.Get([]byte("a")); !errors.Is(err, ErrIndexMismatch) {
		t.Fatalf("Get = %v, want ErrIndexMismatch", err)
	}
	if v, err := db.Get([]byte("b")); err != nil || string(v) != "2" {
		t.Fatalf("b = %q, %v", v, err)
	}
}
