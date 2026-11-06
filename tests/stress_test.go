package tests

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"astaroth/src/engine"
	"astaroth/tests/testdb"
)

// Concurrency stress test: several putters, deleters and batch writers and
// several readers work on one database at the same time, partly on shared
// keys. Run it with the race detector too:
//
//	go test -race -run TestConcurrencyStress -v ./tests
//
// It checks the five guarantees:
//  1. no corrupted records: the bytes appended during the test parse as
//     complete frames with consecutive transaction ids, and Get never fails;
//  2. no mixed values: every value read, during and after the run, is one a
//     writer wrote for that key (see checkValue);
//  3. no partially visible batches: readers never see a batch half applied;
//  4. no races in the index: run under -race;
//  5. every acknowledged operation is durable: after close and reopen the
//     database holds exactly what the acknowledged operations imply.
//
// It runs on the permanent database with keys unique to the run.

type stressConfig struct {
	putters, deleters, batchers, readers int
	putOps, delOps, batches              int
}

// lastAction records a writer's last acknowledged action on a key: a value, or
// nil for a delete.
type lastAction map[string][]byte

func TestConcurrencyStress(t *testing.T) {
	cfg := stressConfig{putters: 4, deleters: 2, batchers: 2, readers: 4, putOps: 150, delOps: 100, batches: 40}
	if testing.Short() {
		cfg = stressConfig{putters: 2, deleters: 2, batchers: 2, readers: 2, putOps: 40, delOps: 30, batches: 12}
	}
	path := testdb.Path(t)
	db := openPerm(t)
	pre := pk(t, "")
	shared := make([]string, 8)
	for i := range shared {
		shared[i] = fmt.Sprintf("%ssh%d", pre, i)
	}
	ownKey := func(w, i int) string { return fmt.Sprintf("%sown%d/%d", pre, w, i) }
	delKey := func(d, i int) string { return fmt.Sprintf("%sdel%d/%d", pre, d, i) }
	groupKey := func(b, j int) string { return fmt.Sprintf("%sg%d/%d", pre, b, j) }
	const groupSize = 10

	// Each batch writer's group starts at token 0, so readers always find it.
	groupBatch := func(b, n int, extra ...engine.Operation) []engine.Operation {
		ops := make([]engine.Operation, 0, groupSize+len(extra))
		for j := 0; j < groupSize; j++ {
			k := groupKey(b, j)
			ops = append(ops, engine.Operation{Kind: engine.OpPut, Key: []byte(k), Value: mkValue(k, fmt.Sprintf("b%d", b), n)})
		}
		return append(ops, extra...)
	}
	for b := 0; b < cfg.batchers; b++ {
		if err := db.Batch(groupBatch(b, 0)); err != nil {
			t.Fatal(err)
		}
	}
	startSize := fileSize(t, path)

	var (
		wg       sync.WaitGroup
		failOnce sync.Once
		failure  error        // set once, read after every goroutine has finished
		hasFail  atomic.Bool  // what goroutines poll while running
		writing  atomic.Int32 // writers still running
		reads    = make([]atomic.Int64, cfg.readers)
		ownAcked = make([]atomic.Int64, cfg.putters) // putter w: own keys 0..n-1 acknowledged
		actions  = make([]lastAction, cfg.putters+cfg.deleters+cfg.batchers)
	)
	fail := func(err error) {
		failOnce.Do(func() { failure = err })
		hasFail.Store(true)
	}
	failed := hasFail.Load

	// Putters: even ops write the writer's own key, odd ops a shared key.
	for w := 0; w < cfg.putters; w++ {
		actions[w] = lastAction{}
		wg.Add(1)
		writing.Add(1)
		go func(w int) {
			defer wg.Done()
			defer writing.Add(-1)
			rng := rand.New(rand.NewSource(int64(w)))
			name := fmt.Sprintf("p%d", w)
			for i := 0; i < cfg.putOps && !failed(); i++ {
				k := ownKey(w, i)
				if i%2 == 1 {
					k = shared[rng.Intn(len(shared))]
				}
				v := mkValue(k, name, i)
				if err := db.Put([]byte(k), v); err != nil {
					fail(fmt.Errorf("%s put: %w", name, err))
					return
				}
				actions[w][k] = v
				if i%2 == 0 {
					ownAcked[w].Store(int64(i + 1))
				}
			}
		}(w)
	}
	// Deleters: put a key of their own, delete it next, and delete shared keys.
	for d := 0; d < cfg.deleters; d++ {
		me := cfg.putters + d
		actions[me] = lastAction{}
		wg.Add(1)
		writing.Add(1)
		go func(d, me int) {
			defer wg.Done()
			defer writing.Add(-1)
			rng := rand.New(rand.NewSource(int64(100 + d)))
			name := fmt.Sprintf("d%d", d)
			for i := 0; i < cfg.delOps && !failed(); i++ {
				var err error
				switch i % 3 {
				case 0:
					k := delKey(d, i)
					err = db.Put([]byte(k), mkValue(k, name, i))
					actions[me][k] = mkValue(k, name, i)
				case 1:
					k := delKey(d, i-1)
					err = db.Delete([]byte(k))
					actions[me][k] = nil
				case 2:
					k := shared[rng.Intn(len(shared))]
					err = db.Delete([]byte(k))
					actions[me][k] = nil
				}
				if err != nil {
					fail(fmt.Errorf("%s: %w", name, err))
					return
				}
			}
		}(d, me)
	}
	// Batch writers: each batch rewrites the writer's group with the next token
	// and also writes one shared key, so batches contend with single puts.
	for b := 0; b < cfg.batchers; b++ {
		me := cfg.putters + cfg.deleters + b
		actions[me] = lastAction{}
		wg.Add(1)
		writing.Add(1)
		go func(b, me int) {
			defer wg.Done()
			defer writing.Add(-1)
			rng := rand.New(rand.NewSource(int64(200 + b)))
			name := fmt.Sprintf("b%d", b)
			for n := 1; n <= cfg.batches && !failed(); n++ {
				sk := shared[rng.Intn(len(shared))]
				sv := mkValue(sk, name, n)
				if err := db.Batch(groupBatch(b, n, engine.Operation{Kind: engine.OpPut, Key: []byte(sk), Value: sv})); err != nil {
					fail(fmt.Errorf("%s batch: %w", name, err))
					return
				}
				for j := 0; j < groupSize; j++ {
					k := groupKey(b, j)
					actions[me][k] = mkValue(k, name, n)
				}
				actions[me][sk] = sv
			}
		}(b, me)
	}

	// Readers run until every writer is done.
	var rwg sync.WaitGroup
	for r := 0; r < cfg.readers; r++ {
		rwg.Add(1)
		go func(r int) {
			defer rwg.Done()
			rng := rand.New(rand.NewSource(int64(300 + r)))
			get := func(k string) ([]byte, bool) {
				v, err := db.Get([]byte(k))
				if errors.Is(err, engine.ErrNotFound) {
					return nil, false
				}
				if err != nil {
					fail(fmt.Errorf("reader %d: Get(%s): %w", r, k, err))
					return nil, false
				}
				if _, _, err := checkValue(k, v); err != nil {
					fail(fmt.Errorf("reader %d: Get(%s): %w", r, k, err))
				}
				return v, true
			}
			for writing.Load() > 0 && !failed() {
				get(shared[rng.Intn(len(shared))]) // any value must be intact

				// A batch must never be seen half applied: the first key of a
				// group is written first, so the last key, read after it, must
				// carry a token at least as new.
				b := rng.Intn(cfg.batchers)
				v0, ok0 := get(groupKey(b, 0))
				v9, ok9 := get(groupKey(b, groupSize-1))
				if !ok0 || !ok9 {
					fail(fmt.Errorf("reader %d: group %d key missing", r, b))
					return
				}
				_, t0, _ := checkValue(groupKey(b, 0), v0)
				_, t9, _ := checkValue(groupKey(b, groupSize-1), v9)
				if t9 < t0 {
					fail(fmt.Errorf("reader %d: group %d read token %d then %d: batch visible half applied", r, b, t0, t9))
					return
				}

				// An acknowledged put is visible to every other goroutine.
				w := rng.Intn(cfg.putters)
				if c := ownAcked[w].Load(); c > 0 {
					k := ownKey(w, int(c-1))
					if v, ok := get(k); !ok || !bytes.Equal(v, mkValue(k, fmt.Sprintf("p%d", w), int(c-1))) {
						fail(fmt.Errorf("reader %d: acknowledged %s not visible", r, k))
						return
					}
				}
				reads[r].Add(1)
			}
		}(r)
	}
	wg.Wait()
	rwg.Wait()
	if failure != nil {
		t.Fatal(failure)
	}
	total := int64(0)
	for r := range reads {
		if reads[r].Load() == 0 {
			t.Fatalf("reader %d never completed a read while writers ran", r)
		}
		total += reads[r].Load()
	}

	// Guarantee 1: everything appended parses, with consecutive transaction ids.
	frames := appendedFrames(t, path, startSize)
	for i := 1; i < len(frames); i++ {
		if frames[i].Seq != frames[i-1].Seq+1 {
			t.Fatalf("transaction ids jump from %d to %d", frames[i-1].Seq, frames[i].Seq)
		}
	}

	// Guarantees 2 and 5, before and after a restart.
	check := func(db *engine.DB, when string) {
		for w := 0; w < cfg.putters; w++ { // own keys: exactly as written
			for i := 0; i < cfg.putOps; i += 2 {
				k := ownKey(w, i)
				wantValue(t, db, k, string(mkValue(k, fmt.Sprintf("p%d", w), i)))
			}
		}
		for d := 0; d < cfg.deleters; d++ { // every put-then-deleted key is gone
			for i := 0; i < cfg.delOps; i += 3 {
				if i+1 < cfg.delOps {
					wantMissing(t, db, delKey(d, i))
				}
			}
		}
		for b := 0; b < cfg.batchers; b++ { // every group holds the last batch whole
			for j := 0; j < groupSize; j++ {
				k := groupKey(b, j)
				wantValue(t, db, k, string(mkValue(k, fmt.Sprintf("b%d", b), cfg.batches)))
			}
		}
		// Shared keys: the final state must be some writer's last action on it.
		for _, k := range shared {
			v, err := db.Get([]byte(k))
			found := err == nil
			if err != nil && !errors.Is(err, engine.ErrNotFound) {
				t.Fatalf("%s: Get(%s): %v", when, k, err)
			}
			ok := false
			for _, acts := range actions {
				if last, touched := acts[k]; touched && ((last == nil && !found) || (last != nil && found && bytes.Equal(v, last))) {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("%s: shared key %s holds %q (found=%v), which is no writer's last action", when, k, v, found)
			}
		}
	}
	check(db, "before restart")
	db.Close()
	db = openPerm(t)
	defer db.Close()
	check(db, "after restart")

	t.Logf("%d frames appended, %d reader iterations during the writes", len(frames), total)
}
