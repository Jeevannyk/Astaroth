package tests

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"astaroth/src/engine"
)

// Concurrent writers killed at a random moment (spec D1, D2, D3). The child
// process runs four writers at once and is killed with Process.Kill
// (TerminateProcess on Windows, SIGKILL elsewhere) after a random delay, so
// the kill lands wherever the writers happen to be, including inside a write
// or an fsync. The parent then checks every writer's acknowledged operations.
//
// Writers (each prints "ACK <writer> <i>" after its operation i returns):
//
//	w0, w1  put their own keys c/w0/i, c/w1/i; every third op instead puts
//	        the shared key c/shared (D2: two writers on the same key)
//	b       batch n sets c/g/0..9 to token n (D3: a batch next to single puts)
//	d       even op i puts c/d/i, odd op i deletes c/d/i-1 (concurrent deletes)

const (
	concGroup  = 10
	concMaxOps = 1_000_000
)

func concurrentChild(db *engine.DB) {
	ack := func(w string, i int) { fmt.Printf("ACK %s %d\n", w, i) }
	die := func(err error) { fmt.Fprintln(os.Stderr, "child:", err); os.Exit(3) }
	putter := func(w string) {
		for i := 0; i < concMaxOps; i++ {
			k := fmt.Sprintf("c/%s/%d", w, i)
			if i%3 == 2 {
				k = "c/shared"
			}
			if err := db.Put([]byte(k), mkValue(k, w, i)); err != nil {
				die(err)
			}
			ack(w, i)
		}
	}
	go putter("w0")
	go putter("w1")
	go func() {
		for n := 0; n < concMaxOps; n++ {
			ops := make([]engine.Operation, concGroup)
			for j := range ops {
				k := fmt.Sprintf("c/g/%d", j)
				ops[j] = engine.Operation{Kind: engine.OpPut, Key: []byte(k), Value: mkValue(k, "b", n)}
			}
			if err := db.Batch(ops); err != nil {
				die(err)
			}
			ack("b", n)
		}
	}()
	go func() {
		for i := 0; i < concMaxOps; i++ {
			var err error
			if i%2 == 0 {
				k := fmt.Sprintf("c/d/%d", i)
				err = db.Put([]byte(k), mkValue(k, "d", i))
			} else {
				err = db.Delete([]byte(fmt.Sprintf("c/d/%d", i-1)))
			}
			if err != nil {
				die(err)
			}
			ack("d", i)
		}
	}()
	select {} // until killed
}

// killedRun starts the concurrent child, kills it after delay, and returns how
// many operations each writer acknowledged.
func killedRun(t *testing.T, path string, delay time.Duration) map[string]int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "ASTAROTH_CHILD=1", "ASTAROTH_CHILD_DB="+path, "ASTAROTH_CHILD_CONCURRENT=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(delay)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	cmd.Wait() // also drains stdout to EOF, so no ACK printed before the kill is missed
	if strings.Contains(errb.String(), "child:") {
		t.Fatalf("child reported an error before the kill: %s", errb.String())
	}

	acked := map[string]int{"w0": 0, "w1": 0, "b": 0, "d": 0}
	for _, line := range strings.Split(out.String(), "\n") {
		var w string
		var i int
		if _, err := fmt.Sscanf(line, "ACK %s %d", &w, &i); err != nil {
			continue
		}
		if i != acked[w] {
			t.Fatalf("writer %s acknowledged op %d out of order (expected %d)", w, i, acked[w])
		}
		acked[w]++
	}
	return acked
}

func TestConcurrentWritersKilledAtRandom(t *testing.T) {
	iterations := 15
	if testing.Short() {
		iterations = 4
	}
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	totalAcked := 0

	for it := 0; it < iterations; it++ {
		path := newPath(t)
		delay := time.Duration(80+rng.Intn(400)) * time.Millisecond
		acked := killedRun(t, path, delay)
		label := fmt.Sprintf("iteration %d, killed after %v, acked %v", it, delay, acked)
		for _, n := range acked {
			totalAcked += n
		}

		db, err := engine.Open(path, engine.Options{LockTimeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("%s: open after kill failed: %v", label, err)
		}
		found := 0
		get := func(k string) ([]byte, bool) {
			v, err := db.Get([]byte(k))
			if errors.Is(err, engine.ErrNotFound) {
				return nil, false
			}
			if err != nil {
				t.Fatalf("%s: Get(%s): %v", label, k, err)
			}
			if _, _, err := checkValue(k, v); err != nil {
				t.Fatalf("%s: %s: %v", label, k, err)
			}
			found++
			return v, true
		}

		// D1: each putter's own keys. Acknowledged: present. The one op in
		// flight: either. Later ones: absent.
		for _, w := range []string{"w0", "w1"} {
			a := acked[w]
			for i := 0; i <= a+2; i++ {
				if i%3 == 2 {
					continue
				}
				k := fmt.Sprintf("c/%s/%d", w, i)
				_, ok := get(k)
				switch {
				case i < a && !ok:
					t.Fatalf("%s: acknowledged %s lost", label, k)
				case i > a && ok:
					t.Fatalf("%s: %s present but writer %s never got that far", label, k, w)
				}
			}
		}

		// D2: the shared key holds a value one of the two writers actually
		// wrote, no later than its op in flight, and it exists if any write
		// to it was acknowledged.
		if v, ok := get("c/shared"); ok {
			w, n, _ := checkValue("c/shared", v)
			if a, known := acked[w]; !known || n%3 != 2 || n > a {
				t.Fatalf("%s: c/shared holds op %d of %q, which was never sent", label, n, w)
			}
		} else if acked["w0"] > 2 || acked["w1"] > 2 {
			t.Fatalf("%s: an acknowledged write to c/shared was lost", label)
		}

		// D3: the batch group is one whole batch: the last acknowledged one or
		// the one in flight, never a mix and never older.
		a := acked["b"]
		tokens := map[int]bool{}
		present := 0
		for j := 0; j < concGroup; j++ {
			k := fmt.Sprintf("c/g/%d", j)
			if v, ok := get(k); ok {
				_, n, _ := checkValue(k, v)
				tokens[n] = true
				present++
			}
		}
		switch {
		case present != 0 && present != concGroup:
			t.Fatalf("%s: %d of %d batch keys present: partial batch", label, present, concGroup)
		case len(tokens) > 1:
			t.Fatalf("%s: batch keys from different batches %v: partial batch", label, tokens)
		case present == 0 && a > 0:
			t.Fatalf("%s: %d acknowledged batches lost", label, a)
		}
		for n := range tokens {
			if n != a && n != a-1 {
				t.Fatalf("%s: group holds batch %d, want %d or %d", label, n, a-1, a)
			}
		}

		// Concurrent deletes: put c/d/i at op i, delete it at op i+1.
		ad := acked["d"]
		for i := 0; i <= ad+2; i += 2 {
			k := fmt.Sprintf("c/d/%d", i)
			_, ok := get(k)
			switch {
			case i+1 < ad && ok:
				t.Fatalf("%s: acknowledged delete of %s lost", label, k)
			case i < ad && i+1 > ad && !ok:
				t.Fatalf("%s: acknowledged put of %s lost", label, k)
			case i > ad && ok:
				t.Fatalf("%s: %s present but the deleter never got that far", label, k)
			}
		}

		// Nothing else may exist, and the log must end cleanly.
		if n, _ := db.Len(); n != found {
			t.Fatalf("%s: database holds %d keys, the checks found %d: unknown keys present", label, n, found)
		}
		db.Close()
		frameCount(t, path)
		t.Logf("%s: ok", label)
	}
	if totalAcked == 0 {
		t.Fatal("no operation was ever acknowledged: the kills all landed before the writers started")
	}
}
