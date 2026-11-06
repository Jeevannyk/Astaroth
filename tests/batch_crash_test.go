package tests

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"

	"astaroth/src/storage"
)

// Batch crash scenarios (spec Group B). A child process runs batches of the
// batchScript workload and is killed at a named point of the write of one
// batch. Every batch rewrites the same keys, so a partly applied batch would
// show up as a mix of two batches' values; the recovered content must equal
// the state after a whole number of batches.
//
// These tests kill processes and work on files under t.TempDir().

func batchKeys(size int) []string {
	keys := make([]string, size)
	for j := range keys {
		keys[j] = fmt.Sprintf("bk%d", j)
	}
	return keys
}

var batchScenarios = []crashScenario{
	{"B1 kill after 3 of 10 records are written", "append-after-3-records", onlyAcked},
	{"B2 kill after all 10 records, before the commit marker", "append-before-commit-marker", onlyAcked},
	{"kill in the middle of the batch frame", "append-partial-write", onlyAcked},
	{"B3 kill after the commit marker is written, before fsync", "append-after-write", ackedOrMore},
	{"kill after fsync, before the batch is acknowledged", "append-after-sync", mustHaveMore},
}

func TestKillDuringBatch(t *testing.T) {
	const batches, size = 6, 10
	hits := batches
	if testing.Short() {
		hits = 2
	}
	keys := batchKeys(size)

	for _, sc := range batchScenarios {
		t.Run(sc.name, func(t *testing.T) {
			for hit := 1; hit <= hits; hit++ {
				path := newPath(t)
				res := runBatchChild(t, path, batches, size, fmt.Sprintf("%s:%d", sc.failpoint, hit))
				if !res.fired {
					t.Fatalf("hit %d: failpoint %s did not fire; the scenario was not tested", hit, sc.failpoint)
				}
				if res.acked != hit-1 {
					t.Fatalf("hit %d: child acknowledged %d batches, want %d", hit, res.acked, hit-1)
				}
				label := fmt.Sprintf("killed during batch %d (%d acknowledged)", hit-1, res.acked)

				before, after := stateAfterBatches(res.acked, size), stateAfterBatches(res.acked+1, size)
				got := recoveredKeys(t, path, keys)
				switch sc.want {
				case onlyAcked:
					if !sameState(got, before) {
						t.Fatalf("%s: none of the in-flight batch may be visible\n got %v\nwant %v", label, got, before)
					}
				case ackedOrMore:
					if !sameState(got, before) && !sameState(got, after) {
						t.Fatalf("%s: batch must be all or nothing\n got %v\nwant %v\n  or %v", label, got, before, after)
					}
				case mustHaveMore:
					if !sameState(got, after) {
						t.Fatalf("%s: the durable batch must be visible in full\n got %v\nwant %v", label, got, after)
					}
				}
				// Nothing torn may remain, and the next transaction id follows on.
				frames := frameCount(t, path)
				db := mustOpen(t, path)
				if err := db.Batch(toOperations(batchScript(99, size))); err != nil {
					t.Fatalf("%s: batch after recovery: %v", label, err)
				}
				db.Close()
				if frameCount(t, path) != frames+1 {
					t.Fatalf("%s: batch after recovery did not append exactly one frame", label)
				}
			}
		})
	}
}

// B4: every acknowledged batch is present after the process dies right after
// the last acknowledgement.
func TestKillAfterBatchAcknowledged(t *testing.T) {
	for _, n := range []int{1, 3, 7} {
		path := newPath(t)
		res := runBatchChild(t, path, n, 10, "")
		if res.acked != n {
			t.Fatalf("child acknowledged %d of %d batches", res.acked, n)
		}
		if got, want := recoveredKeys(t, path, batchKeys(10)), stateAfterBatches(n, 10); !sameState(got, want) {
			t.Fatalf("%d batches: acknowledged batch lost\n got %v\nwant %v", n, got, want)
		}
	}
}

// B6: a batch of 10,000 operations killed halfway is invisible, and recovery
// still completes well inside the 2-second budget.
func TestKillDuringTenThousandOpBatch(t *testing.T) {
	const size = 10_000
	for _, point := range []string{"append-partial-write:2", "append-after-3-records:2", "append-before-commit-marker:2"} {
		path := newPath(t)
		res := runBatchChild(t, path, 3, size, point)
		if !res.fired || res.acked != 1 {
			t.Fatalf("%s: fired=%v acked=%d, want the second batch killed", point, res.fired, res.acked)
		}
		start := time.Now()
		got := recoveredKeys(t, path, batchKeys(size))
		elapsed := time.Since(start)
		if !sameState(got, stateAfterBatches(1, size)) {
			t.Fatalf("%s: part of the killed 10,000-op batch is visible", point)
		}
		t.Logf("%s: open + %d reads took %v", point, size, elapsed)
		if elapsed > 2*time.Second {
			t.Fatalf("%s: recovery took %v", point, elapsed)
		}
	}
}

// The commit marker is the last 4 bytes of the frame. Every truncation inside
// a batch frame, including one that keeps every record and drops only the
// marker, must make the whole batch disappear.
func TestBatchWithoutCommitMarkerIsAbsent(t *testing.T) {
	path := newPath(t)
	db := mustOpen(t, path)
	put(t, db, "before", "kept")
	start := fileSize(t, path)
	if err := db.Batch(toOperations(batchScript(0, 10))); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw := readFile(t, path)
	frame := raw[start:]

	fr, err := storage.ReadFrame(bytes.NewReader(frame), int64(len(frame)))
	if err != nil || len(fr.Ops) != 10 {
		t.Fatalf("batch frame: %d ops, %v", len(fr.Ops), err)
	}
	for cut := 1; cut < len(frame); cut++ {
		p := newPath(t)
		if err := os.WriteFile(p, raw[:int(start)+cut], 0o644); err != nil {
			t.Fatal(err)
		}
		got := recoveredKeys(t, p, append(batchKeys(10), "before"))
		if !sameState(got, map[string]string{"before": "kept"}) {
			t.Fatalf("frame cut to %d of %d bytes (marker is the last 4): got %v", cut, len(frame), got)
		}
	}
}

// Transaction ids: each batch gets the next id, and recovery continues the
// sequence so the batch after a crash gets the id after the last committed one.
func TestBatchTransactionIDsAreConsecutive(t *testing.T) {
	path := newPath(t)
	res := runBatchChild(t, path, 5, 10, "append-before-commit-marker:4")
	if !res.fired || res.acked != 3 {
		t.Fatalf("fired=%v acked=%d", res.fired, res.acked)
	}
	db := mustOpen(t, path) // recovery drops the uncommitted batch (id 4)
	if err := db.Batch(toOperations(batchScript(3, 10))); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw := readFile(t, path)
	r := bytes.NewReader(raw[storage.HeaderSize:])
	var ids []uint64
	for r.Len() > 0 {
		fr, err := storage.ReadFrame(r, int64(r.Len()))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, fr.Seq)
	}
	want := []uint64{1, 2, 3, 4} // the dropped batch's id is reused by the next commit
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("transaction ids = %v, want %v", ids, want)
	}
	if got := recoveredKeys(t, path, batchKeys(10)); !sameState(got, stateAfterBatches(4, 10)) {
		t.Fatalf("state after recommit: %v", got)
	}
}
