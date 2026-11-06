package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"

	"astaroth/src/engine"
	"astaroth/src/storage"
)

// Compaction tests. They replace database files and kill processes, so they
// work on files under t.TempDir() and never touch the permanent database.

func generation(t *testing.T, path string) uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw := make([]byte, storage.HeaderSize)
	if _, err := f.ReadAt(raw, 0); err != nil {
		t.Fatal(err)
	}
	h, err := storage.DecodeHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	return h.Generation
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// opsInFile counts the PUT and DEL records in the whole file.
func opsInFile(t *testing.T, path string) (puts, dels int) {
	t.Helper()
	raw := readFile(t, path)
	r := bytes.NewReader(raw[storage.HeaderSize:])
	for r.Len() > 0 {
		f, err := storage.ReadFrame(r, int64(r.Len()))
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range f.Ops {
			if op.Type == storage.OpPut {
				puts++
			} else {
				dels++
			}
		}
	}
	return puts, dels
}

func runCompactChild(t *testing.T, path, failpointSpec string) childResult {
	t.Helper()
	return runChildEnv(t, path, 0, failpointSpec, "ASTAROTH_CHILD_COMPACT=1")
}

// --- crash scenarios -------------------------------------------------------

type compactCrash struct {
	name      string
	failpoint string
	replaced  bool // the new file must be the data file afterwards
}

var compactCrashes = []compactCrash{
	{"kill before compaction starts", "compact-start", false},
	{"kill after the temporary file is written, before it is synced", "compact-before-sync", false},
	{"kill after sync, before the new file is verified", "compact-before-verify", false},
	{"kill immediately before the rename (old file closed)", "compact-before-rename", false},
	{"kill right after the rename, before the directory sync", "compact-after-rename", true},
	{"kill after the directory sync, before reopening", "compact-after-dir-sync", true},
	{"kill after compaction finished", "compact-done", true},
}

// checkAfterCompactionCrash opens the database the way the next process would
// and checks: open succeeds, every acknowledged operation is intact, the
// temporary file is gone, and the expected file is the data file.
func checkAfterCompactionCrash(t *testing.T, path, label string, want map[string]string, keys []string, oldSize int64, replaced bool) {
	t.Helper()
	got := recoveredKeys(t, path, keys)
	if !sameState(got, want) {
		t.Fatalf("%s: acknowledged data changed\n got %v\nwant %v", label, got, want)
	}
	if exists(path + storage.CompactTempSuffix) {
		t.Fatalf("%s: temporary file left behind after open", label)
	}
	gen, size := generation(t, path), fileSize(t, path)
	switch {
	case replaced && (gen != 2 || size >= oldSize):
		t.Fatalf("%s: expected the compacted file (generation 2, smaller than %d), found generation %d, %d bytes", label, oldSize, gen, size)
	case !replaced && (gen != 1 || size != oldSize):
		t.Fatalf("%s: expected the untouched old file (generation 1, %d bytes), found generation %d, %d bytes", label, oldSize, gen, size)
	}
	frameCount(t, path)

	// The database keeps working: writes after recovery, and a new compaction.
	db := mustOpen(t, path)
	if err := db.Put([]byte("after"), []byte("x")); err != nil {
		t.Fatalf("%s: Put after recovery: %v", label, err)
	}
	if err := db.Compact(); err != nil {
		t.Fatalf("%s: compaction after recovery: %v", label, err)
	}
	db.Close()
	want2 := map[string]string{"after": "x"}
	for k, v := range want {
		want2[k] = v
	}
	if got := recoveredKeys(t, path, append(keys, "after")); !sameState(got, want2) {
		t.Fatalf("%s: data wrong after a later write and compaction\n got %v\nwant %v", label, got, want2)
	}
}

func TestKillDuringCompaction(t *testing.T) {
	const ops = 40 // five keys rewritten over and over: most of the log is garbage
	for _, sc := range compactCrashes {
		t.Run(sc.name, func(t *testing.T) {
			path := newPath(t)
			if res := runChild(t, path, ops, ""); res.acked != ops {
				t.Fatalf("workload acknowledged %d of %d", res.acked, ops)
			}
			oldSize := fileSize(t, path)

			res := runCompactChild(t, path, sc.failpoint)
			if !res.fired {
				t.Fatalf("failpoint %s did not fire (compacted=%v err=%q)", sc.failpoint, res.compacted, res.compactErr)
			}
			checkAfterCompactionCrash(t, path, sc.failpoint, stateAfter(ops), crashKeys, oldSize, sc.replaced)
		})
	}
}

// Kill while the temporary file is being written: after 1, 2 or all 3 of its
// frames. 3,000 keys of which a quarter are deleted leave 2,250 live keys,
// which compaction writes as frames of 1,000, 1,000 and 250 records.
func TestKillWhileWritingTemporaryFile(t *testing.T) {
	const batches, size = 3, 3000
	if live := len(stateAfterBatches(batches, size)); live != 2250 {
		t.Fatalf("workload leaves %d live keys, the test expects 2,250 (3 frames)", live)
	}
	for _, point := range []string{"compact-write-frame:1", "compact-write-frame:2", "compact-write-frame:3"} {
		t.Run(point, func(t *testing.T) {
			path := newPath(t)
			if res := runBatchChild(t, path, batches, size, ""); res.acked != batches {
				t.Fatalf("workload acknowledged %d of %d batches", res.acked, batches)
			}
			oldSize := fileSize(t, path)

			res := runCompactChild(t, path, point)
			if !res.fired {
				t.Fatalf("%s did not fire", point)
			}
			if !exists(path + storage.CompactTempSuffix) {
				t.Fatalf("%s: the kill left no temporary file; the scenario did not happen", point)
			}
			checkAfterCompactionCrash(t, path, point, stateAfterBatches(batches, size), batchKeys(size), oldSize, false)
		})
	}
}

// The replacement itself is one atomic call (MoveFileEx with REPLACE_EXISTING
// on Windows, rename(2) elsewhere), so a kill "during" it leaves one of two
// states, both covered above: old file plus temporary file, or new file and no
// temporary file. This also checks the state a non-atomic replace ("remove
// old, then rename new") would leave if killed between its two steps: no data
// file at all and a complete temporary file. Open must finish the replace.
func TestReplacementInterruptedBetweenRemoveAndRename(t *testing.T) {
	const ops = 40
	path := newPath(t)
	runChild(t, path, ops, "")
	oldSize := fileSize(t, path)
	if res := runCompactChild(t, path, "compact-before-rename"); !res.fired {
		t.Fatal("setup: compaction not stopped before the rename")
	}
	if err := os.Remove(path); err != nil { // the "removed old, not yet renamed" moment
		t.Fatal(err)
	}
	checkAfterCompactionCrash(t, path, "data file missing, temporary file complete", stateAfter(ops), crashKeys, oldSize, true)
}

// Recovery's own cleanup can be killed too: removing an abandoned temporary
// file, and finishing an interrupted replace. The next open must still reach
// the same state.
func TestKillDuringCompactionCleanup(t *testing.T) {
	const ops = 40
	setup := func(t *testing.T, removeData bool) (string, int64) {
		path := newPath(t)
		runChild(t, path, ops, "")
		oldSize := fileSize(t, path)
		if res := runCompactChild(t, path, "compact-before-rename"); !res.fired {
			t.Fatal("setup failed")
		}
		if removeData {
			os.Remove(path)
		}
		return path, oldSize
	}

	t.Run("killed before removing an abandoned temporary file", func(t *testing.T) {
		path, oldSize := setup(t, false)
		if res := runChild(t, path, 0, "recover-before-tmp-remove"); !res.fired {
			t.Fatal("failpoint did not fire")
		}
		checkAfterCompactionCrash(t, path, "cleanup killed", stateAfter(ops), crashKeys, oldSize, false)
	})
	t.Run("killed before finishing an interrupted replace", func(t *testing.T) {
		path, oldSize := setup(t, true)
		if res := runChild(t, path, 0, "recover-before-tmp-promote"); !res.fired {
			t.Fatal("failpoint did not fire")
		}
		checkAfterCompactionCrash(t, path, "promote killed", stateAfter(ops), crashKeys, oldSize, true)
	})
}

// The verification step must catch a bad temporary file and keep the old one.
func TestCompactionVerificationRejectsDamagedFile(t *testing.T) {
	const ops = 40
	path := newPath(t)
	runChild(t, path, ops, "")
	before := readFile(t, path)

	res := runCompactChild(t, path, "compact-corrupt-tmp")
	if res.fired || res.compacted || res.compactErr == "" {
		t.Fatalf("compaction of a damaged file did not fail cleanly: fired=%v compacted=%v err=%q", res.fired, res.compacted, res.compactErr)
	}
	t.Logf("compaction refused: %s", res.compactErr)
	if exists(path + storage.CompactTempSuffix) {
		t.Fatal("damaged temporary file left behind")
	}
	if !bytes.Equal(readFile(t, path), before) {
		t.Fatal("old data file changed although compaction failed")
	}
	if got := recoveredState(t, path); !sameState(got, stateAfter(ops)) {
		t.Fatalf("data changed: %v", got)
	}
}

// --- behaviour -------------------------------------------------------------

func TestCompactionRemovesObsoleteRecordsAndDeletedKeys(t *testing.T) {
	path := newPath(t)
	db := mustOpen(t, path)
	want := map[string]string{}
	for round := 0; round < 5; round++ {
		for i := 0; i < 50; i++ {
			k := fmt.Sprintf("k%02d", i)
			v := fmt.Sprintf("round%d-%s", round, pattern(i, byte(round)))
			put(t, db, k, v)
			want[k] = v
		}
	}
	for i := 0; i < 50; i += 3 {
		k := fmt.Sprintf("k%02d", i)
		del(t, db, k)
		delete(want, k)
	}
	put(t, db, "empty", "")
	want["empty"] = ""
	keys := append([]string{"empty"}, func() []string {
		var ks []string
		for i := 0; i < 50; i++ {
			ks = append(ks, fmt.Sprintf("k%02d", i))
		}
		return ks
	}()...)

	before := fileSize(t, path)
	putsBefore, delsBefore := opsInFile(t, path)
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after := fileSize(t, path)
	puts, dels := opsInFile(t, path)
	t.Logf("file %d -> %d bytes; records %d puts + %d deletes -> %d puts + %d deletes", before, after, putsBefore, delsBefore, puts, dels)

	if after >= before {
		t.Fatalf("file did not shrink: %d -> %d bytes", before, after)
	}
	if dels != 0 || puts != len(want) {
		t.Fatalf("compacted file holds %d puts and %d deletes, want exactly %d puts", puts, dels, len(want))
	}
	if generation(t, path) != 2 {
		t.Fatalf("generation = %d, want 2", generation(t, path))
	}
	if exists(path + storage.CompactTempSuffix) {
		t.Fatal("temporary file left behind")
	}
	for _, k := range keys { // same content, from the live handle
		if v, ok := want[k]; ok {
			wantValue(t, db, k, v)
		} else {
			wantMissing(t, db, k)
		}
	}
	// Writes continue on the new file, and survive a restart.
	put(t, db, "k00", "after compaction")
	want["k00"] = "after compaction"
	db.Close()
	if got := recoveredKeys(t, path, keys); !sameState(got, want) {
		t.Fatalf("after restart\n got %v\nwant %v", got, want)
	}
}

// Transaction ids keep increasing across a compaction.
func TestCompactionKeepsTransactionIDsIncreasing(t *testing.T) {
	path := newPath(t)
	db := mustOpen(t, path)
	for i := 0; i < 10; i++ {
		put(t, db, fmt.Sprintf("k%d", i%3), strconv.Itoa(i))
	}
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	put(t, db, "k0", "after")
	db.Close()

	raw := readFile(t, path)
	r := bytes.NewReader(raw[storage.HeaderSize:])
	var ids []uint64
	for r.Len() > 0 {
		f, err := storage.ReadFrame(r, int64(r.Len()))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.Seq)
	}
	// 10 writes used ids 1..10; the compacted frame is 11, the next write 12.
	if fmt.Sprint(ids) != fmt.Sprint([]uint64{11, 12}) {
		t.Fatalf("transaction ids in the file = %v, want [11 12]", ids)
	}
}

// Spec: after 1,000,000 puts followed by 1,000,000 deletes the file must be
// able to shrink.
func TestCompactionAfterMillionPutsAndDeletes(t *testing.T) {
	batches := 100
	if testing.Short() {
		batches = 10
	}
	const size = 10_000
	path := newPath(t)
	db := mustOpen(t, path)
	ops := make([]engine.Operation, size)
	for _, kind := range []engine.OpKind{engine.OpPut, engine.OpDelete} {
		for b := 0; b < batches; b++ {
			for i := range ops {
				ops[i] = engine.Operation{Kind: kind, Key: []byte(fmt.Sprintf("key%07d", b*size+i))}
				if kind == engine.OpPut {
					ops[i].Value = []byte("value")
				}
			}
			if err := db.Batch(ops); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Automatic compaction may already have run; the file must end up tiny.
	if n, _ := db.Len(); n != 0 {
		t.Fatalf("Len = %d after deleting everything", n)
	}
	before := fileSize(t, path)
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, path)
	t.Logf("%d puts + %d deletes: file %d bytes before the explicit Compact, %d after (generation %d)",
		batches*size, batches*size, before, after, generation(t, path))
	if after != storage.HeaderSize {
		t.Fatalf("file is %d bytes after compacting an empty database, want %d", after, storage.HeaderSize)
	}
	db.Close()
	if db := mustOpen(t, path); true {
		if n, _ := db.Len(); n != 0 {
			t.Fatalf("Len = %d after reopen", n)
		}
		db.Close()
	}
}

// The database must not grow forever without anyone calling Compact.
func TestAutomaticCompaction(t *testing.T) {
	path := newPath(t)
	db := mustOpen(t, path)
	big := pattern(64<<10, 1)
	peak := int64(0)
	for i := 0; i < 160; i++ { // 160 x 64 KiB of overwrites of one key: about 10 MiB written
		v := append([]byte(strconv.Itoa(i)+":"), big...)
		if err := db.Put([]byte("hot"), v); err != nil {
			t.Fatal(err)
		}
		if s := fileSize(t, path); s > peak {
			peak = s
		}
	}
	final := fileSize(t, path)
	gen := generation(t, path)
	t.Logf("about %d bytes written; file peaked at %d bytes, ends at %d, generation %d", 160*len(big), peak, final, gen)
	if gen < 2 {
		t.Fatal("automatic compaction never ran")
	}
	if peak > 6<<20 || final > 5<<20 {
		t.Fatalf("file grew to %d bytes (final %d): automatic compaction did not bound it", peak, final)
	}
	want := string(append([]byte("159:"), big...))
	wantValue(t, db, "hot", want)
	db.Close()
	db = mustOpen(t, path)
	defer db.Close()
	wantValue(t, db, "hot", want)
}

func TestCompactEmptyAndRepeated(t *testing.T) {
	path := newPath(t)
	db := mustOpen(t, path)
	defer db.Close()
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if fileSize(t, path) != storage.HeaderSize || generation(t, path) != 2 {
		t.Fatalf("empty compaction: %d bytes, generation %d", fileSize(t, path), generation(t, path))
	}
	put(t, db, "a", "1")
	put(t, db, "b", "")
	for i := 0; i < 3; i++ {
		if err := db.Compact(); err != nil {
			t.Fatal(err)
		}
		wantValue(t, db, "a", "1")
		wantValue(t, db, "b", "")
	}
	if generation(t, path) != 5 {
		t.Fatalf("generation = %d after four compactions, want 5", generation(t, path))
	}
}

func TestCompactAfterClose(t *testing.T) {
	db := mustOpen(t, newPath(t))
	db.Close()
	if err := db.Compact(); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// Compaction runs while other goroutines read and write; run with -race too.
func TestCompactionConcurrentWithReadersAndWriters(t *testing.T) {
	path := newPath(t)
	db := mustOpen(t, path)
	defer db.Close()
	const writers, perWriter = 3, 80
	var wg sync.WaitGroup
	errc := make(chan error, writers+2)
	stop := make(chan struct{})

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				k := fmt.Sprintf("w%d/%d", w, i%10) // overwrites, so compaction has work
				if err := db.Put([]byte(k), mkValue(k, fmt.Sprintf("w%d", w), i)); err != nil {
					errc <- err
					return
				}
			}
		}(w)
	}
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { // compactor
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := db.Compact(); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() { // reader
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for w := 0; w < writers; w++ {
				k := fmt.Sprintf("w%d/%d", w, 3)
				v, err := db.Get([]byte(k))
				if errors.Is(err, engine.ErrNotFound) {
					continue
				}
				if err != nil {
					errc <- err
					return
				}
				if _, _, err := checkValue(k, v); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	wg.Wait()
	close(stop)
	bg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	for w := 0; w < writers; w++ {
		for j := 0; j < 10; j++ {
			k := fmt.Sprintf("w%d/%d", w, j)
			last := perWriter - 10 + j
			wantValue(t, db, k, string(mkValue(k, fmt.Sprintf("w%d", w), last)))
		}
	}
}
