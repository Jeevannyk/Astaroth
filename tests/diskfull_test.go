package tests

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"astaroth/src/engine"
	"astaroth/src/storage"
)

// Disk-full tests (spec Group E). The file-size limit (Options.MaxBytes) is
// how the spec simulates a full disk; the real "no space left on device"
// error is exercised with the append-disk-full and compact-disk-full
// failpoints. These tests fill and replace files, so they use t.TempDir().

const capBytes = 64 << 10

func openCapped(t *testing.T, path string, max int64) *engine.DB {
	t.Helper()
	db, err := engine.Open(path, engine.Options{MaxBytes: max, LockTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Open(MaxBytes %d): %v", max, err)
	}
	return db
}

func kib(key string, i int) []byte { return fillValue(key, i) }

// underCap fails the test if the data file is over the limit.
func underCap(t *testing.T, path string, max int64) {
	t.Helper()
	if s := fileSize(t, path); s > max {
		t.Fatalf("data file is %d bytes, over the %d-byte limit", s, max)
	}
}

// fillUntilFull puts 1 KiB values under prefix/i until a put is rejected and
// returns the acknowledged keys and the rejecting error.
func fillUntilFull(t *testing.T, db *engine.DB, path, prefix string) ([]string, error) {
	t.Helper()
	var keys []string
	for i := 0; i < 100_000; i++ {
		k := fmt.Sprintf("%s/%d", prefix, i)
		err := db.Put([]byte(k), kib(k, i))
		underCap(t, path, capBytes)
		if err != nil {
			return keys, err
		}
		keys = append(keys, k)
	}
	t.Fatal("database never filled up")
	return nil, nil
}

func wantFilled(t *testing.T, db *engine.DB, keys []string) {
	t.Helper()
	for i, k := range keys {
		v, err := db.Get([]byte(k))
		if err != nil || !bytes.Equal(v, kib(k, i)) {
			t.Fatalf("acknowledged %s lost or changed: %v", k, err)
		}
	}
}

// E1: the limit is reached during a single put.
func TestPutRejectedAtLimit(t *testing.T) {
	path := newPath(t)
	db := openCapped(t, path, capBytes)
	defer db.Close()

	keys, err := fillUntilFull(t, db, path, "k")
	if !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull", err)
	}
	t.Logf("%d puts of 1 KiB accepted, then: %v", len(keys), err)
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error does not say what limit was hit: %v", err)
	}

	// The rejected put left nothing behind, and nothing else changed.
	size := fileSize(t, path)
	rejected := fmt.Sprintf("k/%d", len(keys))
	if err := db.Put([]byte(rejected), kib(rejected, len(keys))); !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("retry: err = %v", err)
	}
	if fileSize(t, path) != size {
		t.Fatal("a rejected put changed the file")
	}
	wantMissing(t, db, rejected)
	wantFilled(t, db, keys)
	if n, _ := db.Len(); n != len(keys) {
		t.Fatalf("Len = %d, want %d", n, len(keys))
	}

	// A value that could never fit is rejected the same way.
	if err := db.Put([]byte("huge"), make([]byte, 1<<20)); !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("1 MiB put under a 64 KiB limit: err = %v", err)
	}
	// Deletes still work at the limit (they may use the reserve).
	del(t, db, keys[0])
	underCap(t, path, capBytes)
	wantMissing(t, db, keys[0])
	frameCount(t, path)
}

// E2: the limit is reached partway through a batch. The batch fails whole:
// not one of its operations is applied, although its first ones would fit.
func TestBatchRejectedAtLimit(t *testing.T) {
	path := newPath(t)
	db := openCapped(t, path, capBytes)
	defer db.Close()
	for i := 0; i < 40; i++ { // about 42 KiB: room for about 17 more KiB of puts
		k := fmt.Sprintf("old/%d", i)
		if err := db.Put([]byte(k), kib(k, i)); err != nil {
			t.Fatal(err)
		}
	}
	before := readFile(t, path)

	ops := []engine.Operation{
		{Kind: engine.OpDelete, Key: []byte("old/0")},                            // delete an existing key
		{Kind: engine.OpPut, Key: []byte("old/1"), Value: []byte("overwritten")}, // overwrite one
	}
	for i := 0; i < 30; i++ { // 30 KiB of new keys: the first ones would fit, the rest not
		k := fmt.Sprintf("new/%d", i)
		ops = append(ops, engine.Operation{Kind: engine.OpPut, Key: []byte(k), Value: kib(k, i)})
	}
	err := db.Batch(ops)
	if !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull", err)
	}
	t.Logf("batch of %d operations rejected: %v", len(ops), err)
	if !bytes.Equal(readFile(t, path), before) {
		t.Fatal("a rejected batch changed the file")
	}
	check := func(db *engine.DB) {
		for i := 0; i < 40; i++ {
			k := fmt.Sprintf("old/%d", i)
			v, err := db.Get([]byte(k))
			if err != nil || !bytes.Equal(v, kib(k, i)) {
				t.Fatalf("%s changed by a rejected batch: %q, %v", k, v, err)
			}
		}
		for i := 0; i < 30; i++ {
			wantMissing(t, db, fmt.Sprintf("new/%d", i))
		}
	}
	check(db)

	// A batch that fits still succeeds; a delete-only batch may use the reserve.
	batch(t, db, opPut("small", "1"), opDel("old/39"))
	batch(t, db, opDel("old/38"), opDel("old/37"))
	underCap(t, path, capBytes)
	db.Close()

	db = openCapped(t, path, capBytes) // and after a restart, still none of the rejected batch
	defer db.Close()
	for i := 0; i < 30; i++ {
		wantMissing(t, db, fmt.Sprintf("new/%d", i))
	}
	wantValue(t, db, "small", "1")
	wantMissing(t, db, "old/39")
}

// E3: the limit is reached, then the process is killed. Restart must succeed
// with every acknowledged put intact, and the limit still enforced.
func TestRestartAfterReachingLimit(t *testing.T) {
	path := newPath(t)
	res := runChildEnv(t, path, 0, "", "ASTAROTH_CHILD_FILL=1", fmt.Sprintf("ASTAROTH_CHILD_MAXBYTES=%d", capBytes))
	if res.fullAt < 0 || res.acked != res.fullAt {
		t.Fatalf("child: acked %d, full at %d", res.acked, res.fullAt)
	}
	t.Logf("child acknowledged %d puts, put %d was rejected, then the child exited without Close", res.acked, res.fullAt)
	underCap(t, path, capBytes)

	db := openCapped(t, path, capBytes)
	var keys []string
	for i := 0; i < res.acked; i++ {
		keys = append(keys, fmt.Sprintf("fill/%d", i))
	}
	wantFilled(t, db, keys)
	wantMissing(t, db, fmt.Sprintf("fill/%d", res.fullAt))
	k := fmt.Sprintf("fill/%d", res.fullAt)
	if err := db.Put([]byte(k), kib(k, res.fullAt)); !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("after restart, a put that does not fit: err = %v, want ErrDiskFull", err)
	}
	del(t, db, keys[0])
	db.Close()
	underCap(t, path, capBytes)
	frameCount(t, path)
}

// Filled to the exact byte: not even the smallest put fits, yet deletes must
// still be accepted. That is what the delete reserve is for; without it a
// full database could never delete its way back to free space.
func TestDeletesAcceptedWhenFileIsExactlyFull(t *testing.T) {
	path := newPath(t)
	db := openCapped(t, path, capBytes)
	defer db.Close()
	keys, _ := fillUntilFull(t, db, path, "k")

	// Find the largest value that still fits: after it the file is exactly at
	// the put limit.
	fitted := -1
	for n := 1100; n >= 0; n-- {
		if err := db.Put([]byte("last"), make([]byte, n)); err == nil {
			fitted = n
			break
		} else if !errors.Is(err, engine.ErrDiskFull) {
			t.Fatal(err)
		}
	}
	if fitted < 0 {
		t.Fatal("no value fitted at all")
	}
	if err := db.Put([]byte("x"), nil); !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("smallest possible put after filling to the byte: err = %v, want ErrDiskFull", err)
	}
	t.Logf("filled to %d bytes with a final %d-byte value; the smallest put is rejected", fileSize(t, path), fitted)
	for _, k := range keys[:10] {
		if err := db.Delete([]byte(k)); err != nil {
			t.Fatalf("delete in a full database: %v", err)
		}
		underCap(t, path, capBytes)
	}
	batch(t, db, opDel(keys[10]), opDel(keys[11])) // delete-only batch too
	underCap(t, path, capBytes)
	for _, k := range keys[:12] {
		wantMissing(t, db, k)
	}
}

// E5: after deletes free space, writes succeed again with no manual step:
// the rejected write compacts the file to make room.
func TestDeleteThenWriteAgainWithoutManualCompaction(t *testing.T) {
	path := newPath(t)
	db := openCapped(t, path, capBytes)
	keys, err := fillUntilFull(t, db, path, "k")
	if !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("err = %v", err)
	}
	want := map[string][]byte{}
	for i, k := range keys {
		want[k] = kib(k, i)
	}
	for i := 0; i < len(keys); i += 2 { // delete half; each delete is accepted at the limit
		del(t, db, keys[i])
		delete(want, keys[i])
		underCap(t, path, capBytes)
	}
	for i := 0; i < 20; i++ { // these did not fit before; now they must
		k := fmt.Sprintf("again/%d", i)
		if err := db.Put([]byte(k), kib(k, i)); err != nil {
			t.Fatalf("put %d after deleting half the keys: %v", i, err)
		}
		want[k] = kib(k, i)
		underCap(t, path, capBytes)
	}
	if g := generation(t, path); g < 2 {
		t.Fatalf("generation %d: the file was never compacted", g)
	}
	check := func(db *engine.DB) {
		for _, k := range keys {
			v, err := db.Get([]byte(k))
			if w, ok := want[k]; ok {
				if err != nil || !bytes.Equal(v, w) {
					t.Fatalf("%s: %v", k, err)
				}
			} else if !errors.Is(err, engine.ErrNotFound) {
				t.Fatalf("%s should be deleted: %v", k, err)
			}
		}
		for i := 0; i < 20; i++ {
			k := fmt.Sprintf("again/%d", i)
			if v, err := db.Get([]byte(k)); err != nil || !bytes.Equal(v, want[k]) {
				t.Fatalf("%s: %v", k, err)
			}
		}
	}
	check(db)
	db.Close()
	db = openCapped(t, path, capBytes)
	defer db.Close()
	check(db)
}

// E5 with an explicit compaction.
func TestDeleteCompactWriteAgain(t *testing.T) {
	path := newPath(t)
	db := openCapped(t, path, capBytes)
	defer db.Close()
	keys, _ := fillUntilFull(t, db, path, "k")
	for _, k := range keys[:len(keys)/2] {
		del(t, db, k)
	}
	before := fileSize(t, path)
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	t.Logf("after deleting %d of %d keys: compaction %d -> %d bytes", len(keys)/2, len(keys), before, fileSize(t, path))
	for i := 0; i < 15; i++ {
		k := fmt.Sprintf("new/%d", i)
		if err := db.Put([]byte(k), kib(k, i)); err != nil {
			t.Fatalf("put after compaction: %v", err)
		}
	}
	for i := 0; i < 15; i++ {
		k := fmt.Sprintf("new/%d", i)
		if v, err := db.Get([]byte(k)); err != nil || !bytes.Equal(v, kib(k, i)) {
			t.Fatalf("%s: %v", k, err)
		}
	}
	underCap(t, path, capBytes)
	for i, k := range keys[len(keys)/2:] {
		v, err := db.Get([]byte(k))
		if err != nil || !bytes.Equal(v, kib(k, len(keys)/2+i)) {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

// The limit is reached by overwrites alone: every time the file fills up,
// compaction makes room and the write goes ahead. The file never passes the
// limit.
func TestOverwritesNeverPassTheLimit(t *testing.T) {
	path := newPath(t)
	db := openCapped(t, path, capBytes)
	for i := 0; i < 500; i++ { // 500 KiB written into a 64 KiB file
		k := fmt.Sprintf("hot/%d", i%5)
		if err := db.Put([]byte(k), kib(k, i)); err != nil {
			t.Fatalf("overwrite %d: %v", i, err)
		}
		underCap(t, path, capBytes)
	}
	t.Logf("500 overwrites of 1 KiB under a 64 KiB limit: file %d bytes, generation %d", fileSize(t, path), generation(t, path))
	for j := 0; j < 5; j++ {
		k := fmt.Sprintf("hot/%d", j)
		v, err := db.Get([]byte(k))
		if err != nil || !bytes.Equal(v, kib(k, 495+j)) {
			t.Fatalf("%s: %v", k, err)
		}
	}
	db.Close()
}

// E4: the limit is reached during compaction. Opening an existing file with a
// lower limit than its live data: the compacted file cannot fit, so
// compaction fails cleanly and the old file stays exactly as it was.
func TestCompactionFailsCleanlyAtLimit(t *testing.T) {
	path := newPath(t)
	db := mustOpen(t, path) // no limit while writing 100 KiB of live data
	var keys []string
	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("k/%d", i)
		if err := db.Put([]byte(k), kib(k, i)); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	db.Close()
	before := readFile(t, path)

	db = openCapped(t, path, capBytes) // the file is already over the new limit: open still works
	wantFilled(t, db, keys)
	err := db.Compact()
	if !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("compaction to a file over the limit: err = %v, want ErrDiskFull", err)
	}
	t.Logf("compaction refused: %v", err)
	if exists(path + storage.CompactTempSuffix) {
		t.Fatal("temporary file left behind")
	}
	if !bytes.Equal(readFile(t, path), before) {
		t.Fatal("old file changed although compaction failed")
	}
	if err := db.Put([]byte("x"), []byte("y")); !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("put on a file over its limit: err = %v", err)
	}
	wantFilled(t, db, keys) // still fully readable
	db.Close()
	db = mustOpen(t, path) // and fine again with the limit lifted
	wantFilled(t, db, keys)
	db.Close()
}

// E4 with a real disk-full error while writing the temporary file.
func TestDiskFullWhileWritingCompactedFile(t *testing.T) {
	const ops = 40
	path := newPath(t)
	runChild(t, path, ops, "")
	before := readFile(t, path)

	res := runCompactChild(t, path, "compact-disk-full")
	if res.compacted || !strings.Contains(res.compactErr, "full") {
		t.Fatalf("compaction did not fail with a disk-full error: compacted=%v err=%q fired=%v", res.compacted, res.compactErr, res.fired)
	}
	t.Logf("compaction failed: %s", res.compactErr)
	if exists(path + storage.CompactTempSuffix) {
		t.Fatal("temporary file left behind")
	}
	if !bytes.Equal(readFile(t, path), before) {
		t.Fatal("old file changed")
	}
	if got := recoveredState(t, path); !sameState(got, stateAfter(ops)) {
		t.Fatalf("data changed: %v", got)
	}
}

// A real disk-full error halfway through writing a frame: the partial frame is
// cut off, the operation is rejected with ErrDiskFull, and the database goes
// on working. Single operations and batches.
func TestDiskFullDuringWriteIsRolledBack(t *testing.T) {
	t.Run("failed write is the last one: no bytes left behind", func(t *testing.T) {
		// Inspect the raw file before any recovery runs: it must end exactly at
		// the last complete frame, not with half of the rejected one.
		path := newPath(t)
		runChild(t, path, 6, "")
		res := runChild(t, path, 1, "append-disk-full:1") // op 0 of the next run fails
		if len(res.diskFull) != 1 {
			t.Fatalf("diskFull %v", res.diskFull)
		}
		frameCount(t, path) // fails on any trailing partial frame
		if got := recoveredState(t, path); !sameState(got, stateAfter(6)) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("single operations", func(t *testing.T) {
		const ops = 14
		for _, hit := range []int{1, 4, 7} {
			path := newPath(t)
			res := runChild(t, path, ops, fmt.Sprintf("append-disk-full:%d", hit))
			if len(res.diskFull) != 1 || res.acked != ops-1 {
				t.Fatalf("hit %d: diskFull %v, acked %d", hit, res.diskFull, res.acked)
			}
			failed := res.diskFull[0]
			want := map[string]string{}
			for i := 0; i < ops; i++ {
				if i == failed {
					continue
				}
				if s := scriptStep(i); s.del {
					delete(want, s.key)
				} else {
					want[s.key] = s.val
				}
			}
			if got := recoveredState(t, path); !sameState(got, want) {
				t.Fatalf("hit %d (op %d failed): got %v\nwant %v", hit, failed, got, want)
			}
			frameCount(t, path) // the half-written frame was removed
		}
	})
	t.Run("batches", func(t *testing.T) {
		const batches, size = 5, 10
		path := newPath(t)
		res := runBatchChild(t, path, batches, size, "append-disk-full:3")
		if len(res.diskFull) != 1 || res.diskFull[0] != 2 || res.acked != batches-1 {
			t.Fatalf("diskFull %v, acked %d", res.diskFull, res.acked)
		}
		want := map[string]string{}
		for b := 0; b < batches; b++ {
			if b == 2 {
				continue // rejected: none of it
			}
			for _, s := range batchScript(b, size) {
				if s.del {
					delete(want, s.key)
				} else {
					want[s.key] = s.val
				}
			}
		}
		if got := recoveredKeys(t, path, batchKeys(size)); !sameState(got, want) {
			t.Fatalf("got %v\nwant %v", got, want)
		}
		frameCount(t, path)
	})
}

func TestMaxBytesValidation(t *testing.T) {
	for _, bad := range []int64{-1, 1, 31} {
		if _, err := engine.Open(newPath(t), engine.Options{MaxBytes: bad}); !errors.Is(err, engine.ErrInvalidOptions) {
			t.Fatalf("MaxBytes %d: err = %v, want ErrInvalidOptions", bad, err)
		}
	}
	path := newPath(t)
	db := openCapped(t, path, storage.HeaderSize) // room for the header and nothing else
	defer db.Close()
	if err := db.Put([]byte("k"), nil); !errors.Is(err, engine.ErrDiskFull) {
		t.Fatalf("put into a header-only limit: err = %v", err)
	}
	if err := db.Delete([]byte("missing")); err != nil {
		t.Fatalf("delete of a missing key writes nothing and must succeed: %v", err)
	}
	if fileSize(t, path) != storage.HeaderSize {
		t.Fatalf("file is %d bytes", fileSize(t, path))
	}
}

// Several writers racing for the last free space: the file never passes the
// limit and every acknowledged put survives. Run with -race too.
func TestConcurrentWritersAtLimit(t *testing.T) {
	path := newPath(t)
	const max = 256 << 10
	db := openCapped(t, path, max)
	var wg sync.WaitGroup
	acked := make([][]string, 4)
	errc := make(chan error, 4)
	for w := range acked {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				k := fmt.Sprintf("w%d/%d", w, i)
				err := db.Put([]byte(k), kib(k, i))
				if errors.Is(err, engine.ErrDiskFull) {
					return
				}
				if err != nil {
					errc <- err
					return
				}
				acked[w] = append(acked[w], k)
			}
		}(w)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	underCap(t, path, max)
	check := func(db *engine.DB) {
		for w := range acked {
			wantFilled(t, db, acked[w])
		}
	}
	check(db)
	db.Close()
	db = openCapped(t, path, max)
	defer db.Close()
	check(db)
	total := 0
	for w := range acked {
		total += len(acked[w])
	}
	t.Logf("%d puts acknowledged across 4 writers before the 256 KiB limit; file %d bytes", total, fileSize(t, path))
	if total == 0 {
		t.Fatal("nothing was written")
	}
}
