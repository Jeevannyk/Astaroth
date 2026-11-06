package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"astaroth/src/engine"
	"astaroth/src/storage"
)

// These tests destroy files on purpose and kill processes, so they work on
// files under t.TempDir() and never touch the permanent database.

var crashKeys = []string{"key0", "key1", "key2", "key3", "key4", "after"}

// recoveredState opens the database in this process and returns its content, then
// closes it. It fails the test if Open fails (invariant 5: open never fails on
// a file our own database produced).
func recoveredState(t *testing.T, path string) map[string]string {
	t.Helper()
	return recoveredKeys(t, path, crashKeys)
}

// recoveredKeys is recoveredState for an explicit set of known keys.
func recoveredKeys(t *testing.T, path string, keys []string) map[string]string {
	t.Helper()
	db, err := engine.Open(path, fast)
	if err != nil {
		t.Fatalf("open after crash failed: %v", err)
	}
	defer db.Close()
	m := map[string]string{}
	for _, k := range keys {
		v, err := db.Get([]byte(k))
		if errors.Is(err, engine.ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatalf("Get(%s): %v", k, err)
		}
		m[k] = string(v)
	}
	if n, _ := db.Len(); n != len(m) {
		t.Fatalf("database holds %d keys, only %d are known keys: unexpected keys present", n, len(m))
	}
	return m
}

// frameCount parses the whole file and returns how many frames it holds. The
// file must consist of frames only: no torn tail left behind.
func frameCount(t *testing.T, path string) int {
	t.Helper()
	raw := readFile(t, path)
	r := bytes.NewReader(raw[storage.HeaderSize:])
	n := 0
	for r.Len() > 0 {
		if _, err := storage.ReadFrame(r, int64(r.Len())); err != nil {
			t.Fatalf("after recovery the file still has an invalid frame (#%d): %v", n+1, err)
		}
		n++
	}
	return n
}

func sameState(a, b map[string]string) bool { return reflect.DeepEqual(a, b) }

// What a scenario allows the recovered state to be.
type outcome int

const (
	onlyAcked    outcome = iota // the in-flight operation must be absent
	ackedOrMore                 // in-flight operation may be fully present or fully absent
	mustHaveMore                // in-flight operation was durable and must be present
)

type crashScenario struct {
	name      string
	failpoint string // point name; the hit count is appended
	want      outcome
}

// The first six rows of the spec's Group A, as processes dying at the named
// points of the write path.
var writeScenarios = []crashScenario{
	{"A1 kill before any byte of the record is written", "append-before-write", onlyAcked},
	{"A2 kill inside the frame header", "append-partial-header", onlyAcked},
	{"A2 kill in the middle of the record", "append-partial-write", onlyAcked},
	{"A3 kill after the full write, before fsync", "append-after-write", ackedOrMore},
	{"A4 kill after fsync, before success is returned", "append-after-sync", mustHaveMore},
}

func TestKillDuringWritePath(t *testing.T) {
	const ops = 12
	hitsToTry := 9 // every write the script makes; ones past the last write do not fire
	if testing.Short() {
		hitsToTry = 3
	}

	for _, sc := range writeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			fired := 0
			for hit := 1; hit <= hitsToTry; hit++ {
				path := newPath(t)
				res := runChild(t, path, ops, fmt.Sprintf("%s:%d", sc.failpoint, hit))
				if !res.fired {
					continue // the script has fewer than `hit` writes
				}
				fired++
				label := fmt.Sprintf("%s hit %d (ops 0..%d acknowledged)", sc.failpoint, hit, res.acked-1)

				acked, inflight := stateAfter(res.acked), stateAfter(res.acked+1)
				got := recoveredState(t, path)
				switch sc.want {
				case onlyAcked:
					if !sameState(got, acked) {
						t.Fatalf("%s:\n got %v\nwant exactly the acknowledged state %v", label, got, acked)
					}
				case ackedOrMore:
					if !sameState(got, acked) && !sameState(got, inflight) {
						t.Fatalf("%s:\n got %v\nwant acknowledged %v\n  or acknowledged+in-flight %v", label, got, acked, inflight)
					}
				case mustHaveMore:
					if !sameState(got, inflight) {
						t.Fatalf("%s:\n got %v\nwant the durable in-flight op applied: %v", label, got, inflight)
					}
				}

				// Recovery must leave a log that ends cleanly and accepts new writes.
				frames := frameCount(t, path)
				db := mustOpen(t, path)
				if err := db.Put([]byte("after"), []byte("x")); err != nil {
					t.Fatalf("%s: Put after recovery: %v", label, err)
				}
				db.Close()
				want := map[string]string{"after": "x"}
				for k, v := range got {
					want[k] = v
				}
				if got2 := recoveredState(t, path); !sameState(got2, want) {
					t.Fatalf("%s: write after recovery lost or changed data\n got %v\nwant %v", label, got2, want)
				}
				if frameCount(t, path) != frames+1 {
					t.Fatalf("%s: write after recovery did not append exactly one frame", label)
				}
			}
			if fired < 2 {
				t.Fatalf("failpoint %s fired in only %d runs; the scenario is not being tested", sc.failpoint, fired)
			}
		})
	}
}

// A5: the process dies right after the last success was returned, without Close.
func TestKillImmediatelyAfterSuccess(t *testing.T) {
	for _, ops := range []int{1, 2, 5, 12, 40} {
		path := newPath(t)
		res := runChild(t, path, ops, "")
		if res.fired || res.acked != ops {
			t.Fatalf("child acknowledged %d of %d ops (fired=%v)", res.acked, ops, res.fired)
		}
		if got, want := recoveredState(t, path), stateAfter(ops); !sameState(got, want) {
			t.Fatalf("%d ops: acknowledged operation lost\n got %v\nwant %v", ops, got, want)
		}
	}
}

// Kill during recovery, and restart after recovery was interrupted.
//
// A recovery that is killed must leave a file that the next recovery handles
// exactly as if the first had never run: same data, same final bytes as an
// uninterrupted recovery of the same starting file.

type recoveryStart struct {
	name  string
	build func(t *testing.T, path string) (acked int)
}

var recoveryStarts = []recoveryStart{
	{"torn tail (killed mid-record)", func(t *testing.T, path string) int {
		res := runChild(t, path, 8, "append-partial-write:3")
		if !res.fired {
			t.Fatal("setup: child did not die mid-write")
		}
		return res.acked
	}},
	{"torn tail (killed inside frame header)", func(t *testing.T, path string) int {
		res := runChild(t, path, 8, "append-partial-header:4")
		if !res.fired {
			t.Fatal("setup: child did not die in the frame header")
		}
		return res.acked
	}},
	{"clean log, several frames", func(t *testing.T, path string) int {
		return runChild(t, path, 10, "").acked
	}},
	{"complete frame never fsynced", func(t *testing.T, path string) int {
		res := runChild(t, path, 8, "append-after-write:3")
		if !res.fired {
			t.Fatal("setup: child did not die after the write")
		}
		return res.acked
	}},
	{"torn header", func(t *testing.T, path string) int {
		h := storage.NewHeader().Encode()
		if err := os.WriteFile(path, h[:10], 0o644); err != nil {
			t.Fatal(err)
		}
		return 0
	}},
	{"no file yet", func(t *testing.T, path string) int { return 0 }},
}

var recoveryPoints = []string{
	"recover-before-header:1",
	"recover-after-header:1",
	"recover-replay-frame:1",
	"recover-replay-frame:2",
	"recover-replay-frame:4",
	"recover-before-truncate:1",
	"recover-after-truncate:1",
	"recover-after-truncate-sync:1",
}

func TestKillDuringRecovery(t *testing.T) {
	for _, start := range recoveryStarts {
		t.Run(start.name, func(t *testing.T) {
			// Build the starting file once, then give every run its own copy.
			base := newPath(t)
			acked := start.build(t, base)
			var baseBytes []byte
			if _, err := os.Stat(base); err == nil {
				baseBytes = readFile(t, base)
			}
			copyTo := func(name string) string {
				p := filepath.Join(t.TempDir(), name)
				if baseBytes != nil {
					if err := os.WriteFile(p, baseBytes, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return p
			}

			// Control: one uninterrupted recovery.
			ctrl := copyTo("control.db")
			ctrlState := recoveredState(t, ctrl)
			ctrlBytes := readFile(t, ctrl)
			if acked > 0 {
				for k, v := range stateAfter(acked) { // nothing acknowledged is lost
					if ctrlState[k] != v {
						t.Fatalf("control recovery lost acknowledged data: %s = %q, want %q", k, ctrlState[k], v)
					}
				}
			}

			fired := 0
			for _, point := range recoveryPoints {
				p := copyTo("interrupted.db")
				res := runChild(t, p, 0, point)
				if !res.fired {
					continue // this point is not on the path for this starting file
				}
				fired++

				got := recoveredState(t, p)
				if !sameState(got, ctrlState) {
					t.Fatalf("%s: after an interrupted recovery\n got %v\nwant %v", point, got, ctrlState)
				}
				if !bytes.Equal(readFile(t, p), ctrlBytes) {
					t.Fatalf("%s: file differs from an uninterrupted recovery", point)
				}
			}
			t.Logf("%d of %d recovery failpoints are on this file's recovery path", fired, len(recoveryPoints))
		})
	}
}

// Recovery killed again and again: each restart is itself interrupted, at a
// different place, before one finally completes.
func TestRecoveryKilledRepeatedly(t *testing.T) {
	path := newPath(t)
	res := runChild(t, path, 10, "append-partial-write:4")
	if !res.fired {
		t.Fatal("setup: child did not die mid-write")
	}
	want := recoveredStateOfCopy(t, path)

	for _, point := range []string{
		"recover-replay-frame:2",
		"recover-before-truncate:1",
		"recover-after-truncate:1",
		"recover-replay-frame:3", // truncate already happened: this point is still on the path
		"recover-after-truncate-sync:1",
	} {
		r := runChild(t, path, 0, point)
		t.Logf("recovery with %-32s fired=%v", point, r.fired)
	}
	if got := recoveredState(t, path); !sameState(got, want) {
		t.Fatalf("after repeated interrupted recoveries\n got %v\nwant %v", got, want)
	}
	for k, v := range stateAfter(res.acked) {
		if got := recoveredState(t, path)[k]; got != v {
			t.Fatalf("acknowledged %s = %q lost (got %q)", k, v, got)
		}
	}
	frameCount(t, path)
}

// recoveredStateOfCopy recovers a copy of path and returns its state, leaving path alone.
func recoveredStateOfCopy(t *testing.T, path string) map[string]string {
	t.Helper()
	cp := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(cp, readFile(t, path), 0o644); err != nil {
		t.Fatal(err)
	}
	return recoveredState(t, cp)
}
