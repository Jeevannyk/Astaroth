package tests

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"astaroth/src/engine"
	"astaroth/src/storage"
	"astaroth/tests/testdb"
)

// Batch behaviour without crashes. Like the other API tests these run on the
// permanent database with keys unique to the run.

func opPut(k, v string) engine.Operation {
	return engine.Operation{Kind: engine.OpPut, Key: []byte(k), Value: []byte(v)}
}

func opDel(k string) engine.Operation {
	return engine.Operation{Kind: engine.OpDelete, Key: []byte(k)}
}

func batch(t *testing.T, db *engine.DB, ops ...engine.Operation) {
	t.Helper()
	if err := db.Batch(ops); err != nil {
		t.Fatalf("Batch: %v", err)
	}
}

// appendedFrames parses everything written to path since offset `from`.
func appendedFrames(t *testing.T, path string, from int64) []storage.Frame {
	t.Helper()
	r := bytes.NewReader(readFrom(t, path, from))
	var frames []storage.Frame
	for r.Len() > 0 {
		f, err := storage.ReadFrame(r, int64(r.Len()))
		if err != nil {
			t.Fatalf("appended bytes do not parse: %v", err)
		}
		frames = append(frames, f)
	}
	return frames
}

func TestBatchAppliesAllAndPersists(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	a, b, c, old := pk(t, "a"), pk(t, "b"), pk(t, "c"), pk(t, "old")
	put(t, db, old, "x")

	start := fileSize(t, path)
	batch(t, db, opPut(a, "1"), opPut(b, "2"), opDel(old), opPut(c, ""))
	wantValue(t, db, a, "1")
	wantValue(t, db, b, "2")
	wantValue(t, db, c, "")
	wantMissing(t, db, old)

	// One batch is one frame: one transaction id, one commit marker.
	frames := appendedFrames(t, path, start)
	if len(frames) != 1 || len(frames[0].Ops) != 4 {
		t.Fatalf("batch wrote %d frames (first has %d ops), want 1 frame of 4", len(frames), len(frames[0].Ops))
	}
	db.Close()

	db = openPerm(t)
	defer db.Close()
	wantValue(t, db, a, "1")
	wantValue(t, db, b, "2")
	wantValue(t, db, c, "")
	wantMissing(t, db, old)
}

// B5, defined: operations apply in order and the last one on a key wins.
func TestBatchRepeatedKeys(t *testing.T) {
	db := openPerm(t)
	k, a, b := pk(t, "k"), pk(t, "a"), pk(t, "b")

	batch(t, db, opPut(k, "1"), opPut(k, "2"), opPut(k, "3"))
	wantValue(t, db, k, "3")
	batch(t, db, opPut(a, "1"), opPut(b, "1"), opPut(a, "2"), opPut(b, "2"), opPut(a, "final"))
	wantValue(t, db, a, "final")
	wantValue(t, db, b, "2")
	db.Close()

	db = openPerm(t)
	defer db.Close()
	wantValue(t, db, k, "3")
	wantValue(t, db, a, "final")
	wantValue(t, db, b, "2")
}

func TestBatchPutThenDeleteSameKey(t *testing.T) {
	db := openPerm(t)
	fresh, existing, revived := pk(t, "fresh"), pk(t, "existing"), pk(t, "revived")
	put(t, db, existing, "old")

	batch(t, db, opPut(fresh, "v"), opDel(fresh))                                  // put then delete: absent
	batch(t, db, opPut(existing, "new"), opDel(existing))                          // also when it existed before
	batch(t, db, opDel(revived), opPut(revived, "v"))                              // delete then put: present
	batch(t, db, opDel(fresh), opPut(fresh, "1"), opDel(fresh), opPut(fresh, "2")) // last wins
	check := func(db *engine.DB) {
		wantValue(t, db, fresh, "2")
		wantMissing(t, db, existing)
		wantValue(t, db, revived, "v")
	}
	check(db)
	db.Close()

	db = openPerm(t)
	defer db.Close()
	check(db)
}

func TestBatchTenThousandOperations(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	prefix := pk(t, "")
	key := func(i int) string { return prefix + strconv.Itoa(i) }

	ops := make([]engine.Operation, 0, storage.MaxOpsPerFrame)
	for i := 0; i < storage.MaxOpsPerFrame; i++ {
		switch {
		case i%10 == 9: // delete a key written earlier in the same batch
			ops = append(ops, opDel(key(i-1)))
		default:
			ops = append(ops, opPut(key(i), fmt.Sprintf("v%d", i)))
		}
	}
	start := fileSize(t, path)
	batch(t, db, ops...)

	frames := appendedFrames(t, path, start)
	if len(frames) != 1 || len(frames[0].Ops) != storage.MaxOpsPerFrame {
		t.Fatalf("10,000-op batch wrote %d frames", len(frames))
	}
	check := func(db *engine.DB) {
		for i := 0; i < storage.MaxOpsPerFrame; i++ {
			switch {
			case i%10 == 9: // the delete itself; key(i) was never written
				wantMissing(t, db, key(i))
			case i%10 == 8: // deleted by the next op
				wantMissing(t, db, key(i))
			default:
				wantValue(t, db, key(i), fmt.Sprintf("v%d", i))
			}
		}
	}
	check(db)
	db.Close()
	db = openPerm(t)
	defer db.Close()
	check(db)
}

func TestEmptyBatchWritesNothing(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	defer db.Close()
	before := fileSize(t, path)
	if err := db.Batch(nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Batch([]engine.Operation{}); err != nil {
		t.Fatal(err)
	}
	if fileSize(t, path) != before {
		t.Fatal("an empty batch wrote to the file")
	}
}

// One invalid operation anywhere rejects the whole batch before anything is
// written: no earlier operation of the batch may become visible.
func TestInvalidBatchIsRejectedWhole(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	defer db.Close()
	a, b := pk(t, "a"), pk(t, "b")
	put(t, db, a, "old")

	tooMany := make([]engine.Operation, storage.MaxOpsPerFrame+1)
	for i := range tooMany {
		tooMany[i] = opPut(a, "x")
	}
	huge := bytes.Repeat([]byte{1}, storage.MaxValueLen)
	tooBig := make([]engine.Operation, 65) // 65 values of 1 MiB > 64 MiB frame limit
	for i := range tooBig {
		tooBig[i] = engine.Operation{Kind: engine.OpPut, Key: []byte(a), Value: huge}
	}

	cases := []struct {
		name string
		ops  []engine.Operation
		want error
	}{
		{"empty key in the middle", []engine.Operation{opPut(a, "new"), opPut("", "v"), opPut(b, "v")}, engine.ErrInvalidKey},
		{"1025-byte key last", []engine.Operation{opPut(a, "new"), opPut(string(make([]byte, 1025)), "v")}, engine.ErrInvalidKey},
		{"value over 1 MiB", []engine.Operation{opPut(a, "new"), {Kind: engine.OpPut, Key: []byte(b), Value: make([]byte, storage.MaxValueLen+1)}}, engine.ErrValueTooLarge},
		{"delete with a value", []engine.Operation{opPut(a, "new"), {Kind: engine.OpDelete, Key: []byte(b), Value: []byte("v")}}, engine.ErrInvalidOp},
		{"unknown kind", []engine.Operation{opPut(a, "new"), {Kind: 0, Key: []byte(b)}}, engine.ErrInvalidOp},
		{"10,001 operations", tooMany, engine.ErrTooManyOps},
		{"over 64 MiB in total", tooBig, engine.ErrBatchTooLarge},
	}
	for _, c := range cases {
		before := fileSize(t, path)
		if err := db.Batch(c.ops); !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if fileSize(t, path) != before {
			t.Fatalf("%s: rejected batch wrote to the file", c.name)
		}
		wantValue(t, db, a, "old")
		wantMissing(t, db, b)
	}
}

func TestBatchAfterClose(t *testing.T) {
	db := openPerm(t)
	db.Close()
	if err := db.Batch([]engine.Operation{opPut(pk(t, "k"), "v")}); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if err := db.Batch(nil); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("empty batch after close: err = %v, want ErrClosed", err)
	}
}

// D4: a reader never observes a partially applied batch. Every batch sets
// keys k0..k9 to the batch number, applied in that order, so a half-applied
// batch looks like "k0 new, k9 old". Values only grow, so if batches are
// atomic, k9 read after k0 is at least k0. A reader that reads k0 and then
// finds k9 smaller has seen a batch half applied.
func TestReadersNeverSeePartialBatch(t *testing.T) {
	db := openPerm(t)
	defer db.Close()
	prefix := pk(t, "")
	keys := make([][]byte, 10)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("%sk%d", prefix, i))
	}
	setAll := func(n int) []engine.Operation {
		ops := make([]engine.Operation, len(keys))
		for i, k := range keys {
			ops[i] = engine.Operation{Kind: engine.OpPut, Key: k, Value: []byte(strconv.Itoa(n))}
		}
		return ops
	}
	if err := db.Batch(setAll(0)); err != nil {
		t.Fatal(err)
	}

	var done atomic.Bool
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	reads := atomic.Int64{}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				v0, err := db.Get(keys[0])
				if err != nil {
					errc <- err
					return
				}
				v9, err := db.Get(keys[9])
				if err != nil {
					errc <- err
					return
				}
				n0, _ := strconv.Atoi(string(v0))
				n9, _ := strconv.Atoi(string(v9))
				if n9 < n0 {
					errc <- fmt.Errorf("read k0 = %d, then k9 = %d: a batch was visible half applied", n0, n9)
					return
				}
				reads.Add(1)
			}
		}()
	}
	const batches = 150
	for n := 1; n <= batches; n++ {
		if err := db.Batch(setAll(n)); err != nil {
			t.Fatal(err)
		}
	}
	done.Store(true)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	for _, k := range keys {
		if v, _ := db.Get(k); string(v) != strconv.Itoa(batches) {
			t.Fatalf("%s = %q after the last batch, want %d", k, v, batches)
		}
	}
	t.Logf("%d consistent read pairs during %d batches", reads.Load(), batches)
}

// Batches and single puts from concurrent writers interleave as whole units.
func TestConcurrentBatchesAndSinglePuts(t *testing.T) {
	db := openPerm(t)
	prefix := pk(t, "")
	var wg sync.WaitGroup
	errc := make(chan error, 2)

	wg.Add(2)
	go func() { // batch writer: every batch rewrites b0..b49 with its number
		defer wg.Done()
		for n := 0; n < 40; n++ {
			ops := make([]engine.Operation, 50)
			for i := range ops {
				ops[i] = opPut(fmt.Sprintf("%sb%d", prefix, i), strconv.Itoa(n))
			}
			if err := db.Batch(ops); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() { // single-put writer
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if err := db.Put([]byte(fmt.Sprintf("%ss%d", prefix, i)), []byte(strconv.Itoa(i))); err != nil {
				errc <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}

	check := func(db *engine.DB) {
		for i := 0; i < 50; i++ {
			wantValue(t, db, fmt.Sprintf("%sb%d", prefix, i), "39")
		}
		for i := 0; i < 100; i++ {
			wantValue(t, db, fmt.Sprintf("%ss%d", prefix, i), strconv.Itoa(i))
		}
	}
	check(db)
	db.Close()
	db = openPerm(t)
	defer db.Close()
	check(db)
}
