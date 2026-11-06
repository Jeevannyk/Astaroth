package tests

import (
	"crypto/sha256"
	"io"
	"os"
	"testing"
	"time"

	"astaroth/src/engine"
	"astaroth/tests/testdb"
)

// The permanent file is shared with other test processes, which take turns
// (see testdb.Path), so a generous timeout only covers slow handle release.
var permOpts = engine.Options{LockTimeout: 10 * time.Second}

// openPerm opens the permanent database data/astaroth.db. If the test does not
// close it, cleanup does, so the lock is always released. Nothing here ever
// removes or truncates the file.
func openPerm(t *testing.T) *engine.DB {
	t.Helper()
	db, err := engine.Open(testdb.Path(t), permOpts)
	if err != nil {
		t.Fatalf("Open permanent database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // ErrClosed if the test already closed it
	return db
}

// pk returns a key that is unique to this run and this test.
func pk(t *testing.T, name string) string {
	return testdb.Prefix(t) + name
}

// keyOfLen returns a key of exactly n bytes that ends in this run's id, so
// edge-length keys do not collide with earlier runs either (for very short
// keys only the tail of the id fits; callers write before they read).
func keyOfLen(n int) []byte {
	id := testdb.RunID()
	if n <= len(id) {
		return []byte(id[len(id)-n:])
	}
	k := make([]byte, n)
	copy(k, id)
	for i := len(id); i < n; i++ {
		k[i] = 'x'
	}
	return k
}

// fileHash returns the SHA-256 of the whole file, streamed.
func fileHash(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// readFrom reads the bytes of path from off to the end of the file.
func readFrom(t *testing.T, path string, off int64) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
