package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"astaroth/src/engine"
	"astaroth/src/failpoint"
)

// Crash tests need a process that really dies. The test binary starts a copy of
// itself in "child mode" (selected by environment variables); the child runs a
// scripted workload against a database file and ends abruptly: either at an
// armed failpoint (see package failpoint) or by os.Exit right after its last
// acknowledged operation. Neither path calls Close, so the file is left exactly
// as a killed process would leave it.

func TestMain(m *testing.M) {
	if os.Getenv("ASTAROTH_CHILD") != "" {
		childMain()
	}
	code := m.Run()
	removeBinaries()
	os.Exit(code)
}

// step is operation i of the scripted workload. The parent and the child both
// derive it from i, so the parent knows what every acknowledged operation was.
type step struct {
	del      bool
	key, val string
}

// scriptStep cycles over five keys so that overwrites and deletes of existing
// keys are exercised; every fourth operation is a delete.
func scriptStep(i int) step {
	key := fmt.Sprintf("key%d", i%5)
	if i%4 == 3 {
		return step{del: true, key: key}
	}
	return step{key: key, val: string(pattern((i*37)%200, byte(i)))}
}

// stateAfter is the database content after operations 0..n-1.
func stateAfter(n int) map[string]string {
	m := map[string]string{}
	for i := 0; i < n; i++ {
		s := scriptStep(i)
		if s.del {
			delete(m, s.key)
		} else {
			m[s.key] = s.val
		}
	}
	return m
}

// batchScript is batch b of the batch workload. Every batch writes the same
// keys bk0..bk{size-1} with values naming the batch, and deletes a different
// quarter of them, so a partly applied batch shows up as a mix of two batches.
func batchScript(b, size int) []step {
	ops := make([]step, size)
	for j := range ops {
		key := fmt.Sprintf("bk%d", j)
		if (b+j)%4 == 3 {
			ops[j] = step{del: true, key: key}
		} else {
			ops[j] = step{key: key, val: fmt.Sprintf("b%d.j%d", b, j)}
		}
	}
	return ops
}

// stateAfterBatches is the content after batches 0..n-1 of batchScript.
func stateAfterBatches(n, size int) map[string]string {
	m := map[string]string{}
	for b := 0; b < n; b++ {
		for _, s := range batchScript(b, size) {
			if s.del {
				delete(m, s.key)
			} else {
				m[s.key] = s.val
			}
		}
	}
	return m
}

func toOperations(steps []step) []engine.Operation {
	ops := make([]engine.Operation, len(steps))
	for i, s := range steps {
		if s.del {
			ops[i] = engine.Operation{Kind: engine.OpDelete, Key: []byte(s.key)}
		} else {
			ops[i] = engine.Operation{Kind: engine.OpPut, Key: []byte(s.key), Value: []byte(s.val)}
		}
	}
	return ops
}

// fillValue is the 1 KiB self-describing value fill mode writes under key.
func fillValue(key string, i int) []byte {
	v := mkValue(key, "fill", i)
	return append(v, bytes.Repeat([]byte{'f'}, 1024-len(v)%1024)...)
}

// childFinish ends the child after its workload. With ASTAROTH_CHILD_COMPACT
// set it first compacts the database and reports "COMPACTED" or
// "COMPACT-ERR <error>". It exits without Close either way.
func childFinish(db *engine.DB) {
	if os.Getenv("ASTAROTH_CHILD_COMPACT") != "" {
		if err := db.Compact(); err != nil {
			fmt.Println("COMPACT-ERR", err)
		} else {
			fmt.Println("COMPACTED")
		}
	}
	os.Exit(0)
}

// childMain is the child process. It never returns.
func childMain() {
	path := os.Getenv("ASTAROTH_CHILD_DB")
	maxBytes, _ := strconv.ParseInt(os.Getenv("ASTAROTH_CHILD_MAXBYTES"), 10, 64)
	db, err := engine.Open(path, engine.Options{LockTimeout: 10 * time.Second, MaxBytes: maxBytes})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child: open:", err)
		os.Exit(2)
	}
	n, _ := strconv.Atoi(os.Getenv("ASTAROTH_CHILD_OPS"))
	if os.Getenv("ASTAROTH_CHILD_CONCURRENT") != "" {
		concurrentChild(db) // runs until the parent kills the process
	}
	if os.Getenv("ASTAROTH_CHILD_FILL") != "" {
		// Fill mode: put fill/i (1 KiB values) until the database is full,
		// print "FULL i" for the rejected put, and exit without Close.
		for i := 0; ; i++ {
			k := fmt.Sprintf("fill/%d", i)
			if err := db.Put([]byte(k), fillValue(k, i)); errors.Is(err, engine.ErrDiskFull) {
				fmt.Printf("FULL %d\n", i)
				childFinish(db)
			} else if err != nil {
				fmt.Fprintf(os.Stderr, "child: fill %d: %v\n", i, err)
				os.Exit(3)
			}
			fmt.Printf("ACK %d\n", i)
		}
	}
	if size, _ := strconv.Atoi(os.Getenv("ASTAROTH_CHILD_BATCH")); size > 0 {
		// Batch mode: n batches of `size` operations; "ACK b" after batch b.
		for b := 0; b < n; b++ {
			if err := db.Batch(toOperations(batchScript(b, size))); errors.Is(err, engine.ErrDiskFull) {
				fmt.Printf("DISKFULL %d\n", b)
				continue
			} else if err != nil {
				fmt.Fprintf(os.Stderr, "child: batch %d: %v\n", b, err)
				os.Exit(3)
			}
			fmt.Printf("ACK %d\n", b)
		}
		childFinish(db)
	}
	for i := 0; i < n; i++ {
		s := scriptStep(i)
		if s.del {
			err = db.Delete([]byte(s.key))
		} else {
			err = db.Put([]byte(s.key), []byte(s.val))
		}
		if errors.Is(err, engine.ErrDiskFull) {
			fmt.Printf("DISKFULL %d\n", i) // rejected cleanly; the workload goes on
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "child: op %d: %v\n", i, err)
			os.Exit(3)
		}
		fmt.Printf("ACK %d\n", i) // os.Stdout is unbuffered: the parent sees this at once
	}
	childFinish(db) // no Close: the same as being killed right after the last ACK
}

// childResult is what the parent learns about a finished child.
type childResult struct {
	diskFull   []int  // operations rejected with ErrDiskFull ("DISKFULL i")
	fullAt     int    // fill mode: the put that found the database full, or -1
	acked      int    // operations 0..acked-1 returned success
	fired      bool   // the child died at its armed failpoint
	compacted  bool   // Compact returned success
	compactErr string // Compact returned this error
}

// runChild runs the workload (ops operations, 0 means just open) in a child
// process with the failpoint armed (empty means none) and waits for it to end.
func runChild(t *testing.T, path string, ops int, failpointSpec string) childResult {
	t.Helper()
	return runChildEnv(t, path, ops, failpointSpec)
}

// runBatchChild runs `batches` batches of `size` operations each (batchScript).
func runBatchChild(t *testing.T, path string, batches, size int, failpointSpec string) childResult {
	t.Helper()
	return runChildEnv(t, path, batches, failpointSpec, "ASTAROTH_CHILD_BATCH="+strconv.Itoa(size))
}

func runChildEnv(t *testing.T, path string, ops int, failpointSpec string, extraEnv ...string) childResult {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"ASTAROTH_CHILD=1",
		"ASTAROTH_CHILD_DB="+path,
		"ASTAROTH_CHILD_OPS="+strconv.Itoa(ops),
		"ASTAROTH_FAILPOINT="+failpointSpec,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()

	res := childResult{fullAt: -1}
	next := 0 // the operation the next ACK or DISKFULL line must name
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "COMPACTED" {
			res.compacted = true
		}
		if msg, ok := strings.CutPrefix(line, "COMPACT-ERR "); ok {
			res.compactErr = msg
		}
		var j int
		if _, serr := fmt.Sscanf(line, "DISKFULL %d", &j); serr == nil {
			if j != next {
				t.Fatalf("child reported op %d out of order (expected %d)", j, next)
			}
			res.diskFull = append(res.diskFull, j)
			next++
			continue
		}
		if _, serr := fmt.Sscanf(line, "FULL %d", &j); serr == nil {
			res.fullAt = j
			continue
		}
		var i int
		if _, serr := fmt.Sscanf(line, "ACK %d", &i); serr == nil {
			if i != next {
				t.Fatalf("child acknowledged op %d out of order (expected %d)", i, next)
			}
			next++
			res.acked++
		}
	}
	switch {
	case err == nil:
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == failpoint.ExitCode:
		res.fired = true
	default:
		t.Fatalf("child failed: %v\nstderr: %s", err, errb.String())
	}
	if failpointSpec == "" && res.fired {
		t.Fatal("child died with no failpoint armed")
	}
	return res
}
