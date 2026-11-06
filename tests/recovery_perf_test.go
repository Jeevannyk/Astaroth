package tests

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"astaroth/src/engine"
	"astaroth/src/storage"
)

// Recovery-time measurement for the spec's target: open() with 1,000,000 keys
// in under 2 seconds. It builds real log files with the frame codec (writing
// 1,000,000 fsynced puts one by one would take hours), then times engine.Open,
// which includes taking the lock, replaying every frame and building the index.
//
//	go test -run TestRecoveryTime -v ./tests
//
// Run it alone: other test processes running in parallel skew the numbers.
// Set ASTAROTH_BENCH_KEEP=<dir> to keep the generated files there, so the same
// files can be opened with `kvdb open`.
//
// What it cannot show: the files were just written, so they sit in the OS page
// cache. A first open after a reboot reads from disk and is slower.

const budget = 2 * time.Second

type perfShape struct {
	name       string
	keys       int
	opsPerFrm  int
	keyLen     int
	valLen     int
	enforce    bool // fail the test if the median is over the budget
	runs       int
	approxSize string
	optIn      bool // only runs when ASTAROTH_BENCH_BIG=1 (file is over 1 GB)
}

var perfShapes = []perfShape{
	{"1M keys, 1M single-op frames (14B key, 64B value)", 1_000_000, 1, 14, 64, true, 5, "~105 MB", false},
	{"1M keys, 100 frames of 10,000 ops (14B key, 64B value)", 1_000_000, 10_000, 14, 64, true, 5, "~85 MB", false},
	{"1M keys, 100 frames of 10,000 ops (14B key, 256B value)", 1_000_000, 10_000, 14, 256, false, 3, "~280 MB", false},
	{"1M keys, 100 frames of 10,000 ops (14B key, 1024B value)", 1_000_000, 10_000, 14, 1024, false, 3, "~1.1 GB", true},
}

// buildBigFile writes a valid database file holding keys distinct keys,
// opsPerFrame to a frame.
func buildBigFile(t *testing.T, path string, s perfShape) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 4<<20)
	h := storage.NewHeader().Encode()
	w.Write(h[:])

	val := pattern(s.valLen, 7)
	ops := make([]storage.Op, 0, s.opsPerFrm)
	seq := uint64(1)
	flush := func() {
		buf, err := storage.EncodeFrame(seq, ops)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(buf)
		seq++
		ops = ops[:0]
	}
	for i := 0; i < s.keys; i++ {
		key := []byte(fmt.Sprintf("key-%0*d", s.keyLen-4, i))
		ops = append(ops, storage.Op{Type: storage.OpPut, Key: key, Value: val})
		if len(ops) == s.opsPerFrm {
			flush()
		}
	}
	if len(ops) > 0 {
		flush()
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestRecoveryTimeOneMillionKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("builds files of 100+ MB; run without -short")
	}
	if raceEnabled {
		t.Skip("timings are meaningless under -race (instrumented code runs several times slower)")
	}
	t.Logf("machine: %s/%s, %d logical CPUs, %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())

	for _, s := range perfShapes {
		t.Run(s.name, func(t *testing.T) {
			if s.optIn && os.Getenv("ASTAROTH_BENCH_BIG") == "" {
				t.Skip("over 1 GB; set ASTAROTH_BENCH_BIG=1 to run")
			}
			dir := t.TempDir()
			if keep := os.Getenv("ASTAROTH_BENCH_KEEP"); keep != "" {
				dir = keep
				os.MkdirAll(dir, 0o755)
			}
			path := filepath.Join(dir, fmt.Sprintf("bench-%dops-%dB.db", s.opsPerFrm, s.valLen))
			buildBigFile(t, path, s)
			st, _ := os.Stat(path)
			t.Logf("file %.1f MB (%s expected)", float64(st.Size())/1e6, s.approxSize)

			var times []time.Duration
			for i := 0; i < s.runs; i++ {
				start := time.Now()
				db, err := engine.Open(path, engine.Options{LockTimeout: time.Second})
				elapsed := time.Since(start)
				if err != nil {
					t.Fatal(err)
				}
				n, _ := db.Len()
				if n != s.keys {
					t.Fatalf("recovered %d keys, want %d", n, s.keys)
				}
				// Spot-check that the index really points at the right values.
				for _, i := range []int{0, s.keys / 2, s.keys - 1} {
					key := fmt.Sprintf("key-%0*d", s.keyLen-4, i)
					if v, err := db.Get([]byte(key)); err != nil || len(v) != s.valLen {
						t.Fatalf("Get(%s) = %d bytes, %v", key, len(v), err)
					}
				}
				db.Close()
				times = append(times, elapsed)
			}

			sorted := append([]time.Duration(nil), times...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			median := sorted[len(sorted)/2]
			t.Logf("open() times: %v", times)
			t.Logf("min %v   median %v   max %v   (budget %v)", sorted[0], median, sorted[len(sorted)-1], budget)
			if s.enforce && median > budget {
				t.Fatalf("median recovery time %v is over the %v budget", median, budget)
			}
		})
	}
}
