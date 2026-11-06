package tests

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"astaroth/src/engine"
	"astaroth/src/storage"
)

// A snapshot of what the database must contain after some number of
// acknowledged operations, and the file size at that point.
type snapshot struct {
	end   int64
	state map[string]string
}

var universe = []string{"k0", "k1", "k2", "k3", "k4", "k5", "after"}

func copyState(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// buildLog runs ops random operations and returns the raw file plus a snapshot
// after the header and after every operation. The last operation is always a
// put, so the final snapshot is a distinct state.
func buildLog(t *testing.T, ops int) ([]byte, []snapshot) {
	t.Helper()
	path := newPath(t)
	db := mustOpen(t, path)
	rng := rand.New(rand.NewSource(1))

	state := map[string]string{}
	snaps := []snapshot{{end: fileSize(t, path), state: copyState(state)}}
	for i := 0; i < ops; i++ {
		k := universe[rng.Intn(6)]
		if i < ops-1 && rng.Intn(3) == 0 {
			if err := db.Delete([]byte(k)); err != nil {
				t.Fatal(err)
			}
			delete(state, k)
		} else {
			v := string(pattern(rng.Intn(40), byte(i)))
			if err := db.Put([]byte(k), []byte(v)); err != nil {
				t.Fatal(err)
			}
			state[k] = v
		}
		snaps = append(snaps, snapshot{end: fileSize(t, path), state: copyState(state)})
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return readFile(t, path), snaps
}

// expected returns the snapshot a file cut to length L must recover to: the
// last one whose frames lie entirely within the first L bytes.
func expected(snaps []snapshot, L int) snapshot {
	best := snaps[0]
	for _, s := range snaps {
		if s.end <= int64(L) {
			best = s
		}
	}
	return best
}

func verify(t *testing.T, db *engine.DB, want map[string]string, label string) {
	t.Helper()
	live := 0
	for _, k := range universe {
		got, err := db.Get([]byte(k))
		w, present := want[k]
		switch {
		case present && err != nil:
			t.Fatalf("%s: Get(%s): %v, want %q", label, k, err, w)
		case present && string(got) != w:
			t.Fatalf("%s: Get(%s) = %q, want %q", label, k, got, w)
		case !present && !errors.Is(err, engine.ErrNotFound):
			t.Fatalf("%s: Get(%s) = %q, %v; want ErrNotFound", label, k, got, err)
		}
		if present {
			live++
		}
	}
	if n, _ := db.Len(); n != live {
		t.Fatalf("%s: Len = %d, want %d", label, n, live)
	}
}

// The core crash-safety test. A kill during a write can leave the file cut at
// any byte (or, with extra bytes of the next frame, anywhere inside a frame).
// Cut the log at every possible length and demand that open() succeeds and
// reproduces exactly the operations that were fully on disk, and that new
// writes after recovery are not lost behind leftover bytes.
func TestOpenAfterTruncationAtEveryByte(t *testing.T) {
	ops := 25
	if testing.Short() {
		ops = 8
	}
	raw, snaps := buildLog(t, ops)
	boundaries := map[int]bool{}
	for _, s := range snaps {
		boundaries[int(s.end)] = true
	}
	dir := t.TempDir()

	for L := 0; L <= len(raw); L++ {
		path := filepath.Join(dir, fmt.Sprintf("cut%d.db", L))
		if err := os.WriteFile(path, raw[:L], 0o644); err != nil {
			t.Fatal(err)
		}
		want := expected(snaps, L)
		label := fmt.Sprintf("cut at %d of %d", L, len(raw))

		db, err := engine.Open(path, fast)
		if err != nil {
			t.Fatalf("%s: open failed: %v", label, err)
		}
		verify(t, db, want.state, label)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if got := fileSize(t, path); got != want.end {
			t.Fatalf("%s: file is %d bytes after recovery, want %d", label, got, want.end)
		}

		// Opening again must change nothing (recovery is idempotent).
		db, err = engine.Open(path, fast)
		if err != nil {
			t.Fatalf("%s: second open failed: %v", label, err)
		}
		verify(t, db, want.state, label+" (second open)")

		// Append after recovery, restart, and make sure it is still there.
		// fsync makes this slow, so sample the cut points.
		if boundaries[L] || boundaries[L-1] || L%9 == 0 {
			if err := db.Put([]byte("after"), []byte("x")); err != nil {
				t.Fatalf("%s: Put after recovery: %v", label, err)
			}
			db.Close()
			db, err = engine.Open(path, fast)
			if err != nil {
				t.Fatalf("%s: open after append failed: %v", label, err)
			}
			after := copyState(want.state)
			after["after"] = "x"
			verify(t, db, after, label+" (after append)")
		}
		db.Close()
	}
}

// Damage anywhere in the last frame must drop exactly that frame: never
// return its (possibly altered) contents, never lose earlier operations.
func TestDamagedLastFrameIsDropped(t *testing.T) {
	raw, snaps := buildLog(t, 12)
	prev := snaps[len(snaps)-2]
	dir := t.TempDir()

	for i := int(prev.end); i < len(raw); i++ {
		for _, mask := range []byte{0x01, 0x80} {
			damaged := append([]byte(nil), raw...)
			damaged[i] ^= mask
			path := filepath.Join(dir, fmt.Sprintf("flip%d_%x.db", i, mask))
			if err := os.WriteFile(path, damaged, 0o644); err != nil {
				t.Fatal(err)
			}
			db, err := engine.Open(path, fast)
			if err != nil {
				t.Fatalf("byte %d ^ %#x: open failed: %v", i, mask, err)
			}
			verify(t, db, prev.state, fmt.Sprintf("byte %d ^ %#x", i, mask))
			db.Close()
			if got := fileSize(t, path); got != prev.end {
				t.Fatalf("byte %d ^ %#x: file is %d bytes, want %d", i, mask, got, prev.end)
			}
		}
	}
}

// The format's own limit on how much of a file open() will cut off.
func TestOpenRefusesToTruncateMoreThanOneFrame(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 64 MiB")
	}
	path := newPath(t)
	h := storage.NewHeader().Encode()
	content := append(h[:], make([]byte, storage.MaxFrameSize+1)...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Open(path, fast); err == nil {
		t.Fatal("open succeeded on an oversized invalid region")
	}
	if fileSize(t, path) != int64(len(content)) {
		t.Fatal("open truncated an oversized invalid region")
	}
}
