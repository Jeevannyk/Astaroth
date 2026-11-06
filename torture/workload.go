package main

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
	"sync"
	"time"

	"astaroth/src/token"
)

// modeParams shape the random workload of one mode.
type modeParams struct {
	desc                 string
	ownKeys, sharedKeys  int     // key ranges: each writer's own, and one range all writers share
	wPut, wDel, wBatch   float64 // relative weights of the action kinds
	wShared, wCompact    float64
	batchMin, batchMax   int
	batchFullFrac        float64 // fraction of batches of exactly batchMax operations
	valMin, valMax       int
	valMaxFrac           float64 // fraction of values of exactly valMax bytes
	binaryFrac           float64 // fraction of values made of random bytes rather than text
	delayMin, delayMax   time.Duration
	maxBytes             int64 // --max-bytes for kvdb; 0 is unlimited
	preload              int   // keys written once before the first iteration
	recoveryKillsMax     int   // extra kills during open() per iteration
	expectDiskFullErrors bool
}

const mib = 1 << 20

var modes = map[string]modeParams{
	"normal": {
		desc:    "puts, deletes and batches of up to 50 operations from concurrent writers, small text and binary values",
		ownKeys: 40, sharedKeys: 8,
		wPut: 45, wDel: 15, wBatch: 20, wShared: 20,
		batchMin: 2, batchMax: 50,
		valMin: 0, valMax: 64, binaryFrac: 0.2,
		delayMin: 5 * time.Millisecond, delayMax: 250 * time.Millisecond,
	},
	"large-values": {
		desc:    "values up to 1 MiB (a quarter exactly 1 MiB), in puts and in batches",
		ownKeys: 8, sharedKeys: 4,
		wPut: 60, wDel: 10, wBatch: 15, wShared: 15,
		batchMin: 2, batchMax: 6,
		valMin: 0, valMax: mib, valMaxFrac: 0.25, binaryFrac: 0.5,
		delayMin: 20 * time.Millisecond, delayMax: 800 * time.Millisecond,
	},
	"large-batches": {
		desc:    "batches of 1,000 to 10,000 operations (a quarter exactly 10,000) next to single puts",
		ownKeys: 20000, sharedKeys: 8,
		wPut: 10, wDel: 5, wBatch: 80, wShared: 5,
		batchMin: 1000, batchMax: 10000, batchFullFrac: 0.25,
		valMin: 0, valMax: 32, binaryFrac: 0.1,
		delayMin: 20 * time.Millisecond, delayMax: 1500 * time.Millisecond,
	},
	"recovery-crash": {
		desc:    "the normal workload on a 200,000-key database, then 1 to 3 kills while open() is recovering",
		ownKeys: 40, sharedKeys: 8,
		wPut: 45, wDel: 15, wBatch: 20, wShared: 20,
		batchMin: 2, batchMax: 50,
		valMin: 0, valMax: 64, binaryFrac: 0.2,
		delayMin: 5 * time.Millisecond, delayMax: 250 * time.Millisecond,
		preload: 200000, recoveryKillsMax: 3,
	},
	"compaction": {
		desc:    "overwrite-heavy workload with 1-6 KB values and explicit compactions, so kills land during compaction",
		ownKeys: 150, sharedKeys: 8,
		wPut: 55, wDel: 20, wBatch: 10, wShared: 10, wCompact: 5,
		batchMin: 2, batchMax: 20,
		valMin: 1000, valMax: 6000, binaryFrac: 0.3,
		delayMin: 20 * time.Millisecond, delayMax: 800 * time.Millisecond,
	},
	"disk-full": {
		desc:    "a 128 KiB --max-bytes limit with a delete-heavy workload, so writes keep hitting the limit",
		ownKeys: 60, sharedKeys: 8,
		wPut: 45, wDel: 30, wBatch: 15, wShared: 10,
		batchMin: 2, batchMax: 10,
		valMin: 500, valMax: 3500, binaryFrac: 0.3,
		delayMin: 10 * time.Millisecond, delayMax: 400 * time.Millisecond,
		maxBytes: 128 << 10, expectDiskFullErrors: true,
	},
}

// value is one key's state in the oracle.
type value struct {
	data []byte
	op   string // id of the operation that wrote it
}

// op is one put or delete.
type op struct {
	key string
	del bool
	val []byte
}

// action is one request: a put, a delete, a batch or a compaction.
type action struct {
	id     string
	kind   string // "put", "del", "batch", "compact"
	ops    []op
	shared bool // a single put or delete on a shared key
	sentAt time.Time
}

func (a *action) String() string {
	switch a.kind {
	case "batch":
		return fmt.Sprintf("%s batch of %d operations", a.id, len(a.ops))
	case "compact":
		return a.id + " compact"
	}
	return fmt.Sprintf("%s %s %s", a.id, a.kind, a.ops[0].key)
}

// request is the serve protocol text for an action.
func (a *action) request() string {
	var b strings.Builder
	line := func(o op) {
		if o.del {
			fmt.Fprintf(&b, "%s del %s\n", a.id, o.key)
		} else {
			fmt.Fprintf(&b, "%s put %s %s\n", a.id, o.key, token.Value(o.val))
		}
	}
	switch a.kind {
	case "batch":
		fmt.Fprintf(&b, "%s begin\n", a.id)
		for _, o := range a.ops {
			line(o)
		}
		fmt.Fprintf(&b, "%s commit\n", a.id)
	case "compact":
		fmt.Fprintf(&b, "%s compact\n", a.id)
	default:
		line(a.ops[0])
	}
	return b.String()
}

// applyTo applies the action's operations, in order, to a key/value map.
func (a *action) applyTo(m map[string]value) {
	for _, o := range a.ops {
		if o.del {
			delete(m, o.key)
		} else {
			m[o.key] = value{data: o.val, op: a.id}
		}
	}
}

// writer is one concurrent client of serve. Its own keys (w<n>/k<i>) are
// written by nobody else, so its oracle for them is exact. It also writes the
// shared keys (s/k<i>), single operations only.
type writer struct {
	n        int
	name     string
	p        modeParams
	rng      *rand.Rand
	seq      int               // operation counter, across iterations
	own      map[string]value  // oracle: exact state of this writer's own keys
	replies  chan string       // serve output lines for this writer
	log      *oracleLog        //
	acked    map[string]*value // this iteration: last acknowledged action per shared key (nil data = deleted)
	ackedID  map[string]string // ... and its operation id
	inflight *action           // sent, not answered when serve died
	lastAck  string            // id of the last acknowledged action
	stats    iterStats
	fail     error // the protocol broke: an unexpected reply
}

type iterStats struct {
	acked, puts, dels, batches, batchOps, compacts, diskFull int
}

func (w *writer) ownKey(i int) string   { return fmt.Sprintf("%s/k%d", w.name, i) }
func sharedKey(i int) string            { return fmt.Sprintf("s/k%d", i) }
func (w *writer) chance(f float64) bool { return w.rng.Float64() < f }

func (w *writer) newValue(id string) []byte {
	n := w.p.valMin
	if w.p.valMax > w.p.valMin {
		n += w.rng.Intn(w.p.valMax - w.p.valMin + 1)
	}
	if w.chance(w.p.valMaxFrac) {
		n = w.p.valMax
	}
	if w.chance(w.p.binaryFrac) {
		b := make([]byte, n)
		w.rng.Read(b)
		return b
	}
	// Text values start with the id that wrote them, which makes a failure
	// report readable.
	b := make([]byte, 0, n+len(id)+1)
	b = append(b, id...)
	b = append(b, ':')
	for len(b) < n {
		b = append(b, byte('a'+w.rng.Intn(26)))
	}
	return b
}

// pickOwnKey prefers keys that exist when deleting, so deletes do something.
func (w *writer) pickOwnKey(forDelete bool) string {
	if forDelete && len(w.own) > 0 && w.chance(0.8) {
		i := w.rng.Intn(len(w.own))
		for k := range w.own { // map order is random; good enough to pick one
			if i == 0 {
				return k
			}
			i--
		}
	}
	return w.ownKey(w.rng.Intn(w.p.ownKeys))
}

func (w *writer) next() *action {
	w.seq++
	a := &action{id: fmt.Sprintf("%s.%d", w.name, w.seq)}
	total := w.p.wPut + w.p.wDel + w.p.wBatch + w.p.wShared + w.p.wCompact
	r := w.rng.Float64() * total
	switch {
	case r < w.p.wPut:
		a.kind = "put"
		a.ops = []op{{key: w.pickOwnKey(false), val: w.newValue(a.id)}}
	case r < w.p.wPut+w.p.wDel:
		a.kind = "del"
		a.ops = []op{{key: w.pickOwnKey(true), del: true}}
	case r < w.p.wPut+w.p.wDel+w.p.wBatch:
		a.kind = "batch"
		n := w.p.batchMin + w.rng.Intn(w.p.batchMax-w.p.batchMin+1)
		if w.chance(w.p.batchFullFrac) {
			n = w.p.batchMax
		}
		for i := 0; i < n; i++ {
			if w.chance(0.2) {
				a.ops = append(a.ops, op{key: w.pickOwnKey(true), del: true})
			} else {
				a.ops = append(a.ops, op{key: w.pickOwnKey(false), val: w.newValue(a.id)})
			}
		}
	case r < w.p.wPut+w.p.wDel+w.p.wBatch+w.p.wShared:
		a.shared = true
		k := sharedKey(w.rng.Intn(w.p.sharedKeys))
		if w.chance(0.25) {
			a.kind, a.ops = "del", []op{{key: k, del: true}}
		} else {
			a.kind, a.ops = "put", []op{{key: k, val: w.newValue(a.id)}}
		}
	default:
		a.kind = "compact"
	}
	return a
}

// run is the writer's loop for one iteration: send an action, wait for its
// reply, send the next. It returns when serve dies.
func (w *writer) run(p *serveProc, iteration int, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		a := w.next()
		w.inflight = a
		a.sentAt = time.Now()
		if err := p.send(a.request()); err != nil {
			return // serve is gone; the action may or may not have arrived
		}
		reply, ok := <-w.replies
		if !ok {
			return // serve died with the action in flight
		}
		switch {
		case reply == "ACK "+a.id:
			w.inflight = nil
			w.lastAck = a.id
			w.recordAck(a, iteration)
		case strings.HasPrefix(reply, "ERR "+a.id+" DISKFULL ") && w.p.maxBytes > 0:
			w.inflight = nil // rejected cleanly: it must have had no effect
			w.stats.diskFull++
		default:
			w.fail = fmt.Errorf("unexpected reply to %s: %q", a, reply)
			w.inflight = nil
			// Keep reading so serve is not blocked; nothing else is sent.
			for range w.replies {
			}
			return
		}
	}
}

func (w *writer) recordAck(a *action, iteration int) {
	w.stats.acked++
	switch a.kind {
	case "put":
		w.stats.puts++
	case "del":
		w.stats.dels++
	case "batch":
		w.stats.batches++
		w.stats.batchOps += len(a.ops)
	case "compact":
		w.stats.compacts++
	}
	if a.shared {
		o := a.ops[0]
		if o.del {
			w.acked[o.key] = &value{op: a.id}
		} else {
			w.acked[o.key] = &value{data: o.val, op: a.id}
		}
		w.ackedID[o.key] = a.id
	} else {
		a.applyTo(w.own)
	}
	w.log.record(iteration, a)
}

// oracleLog is the torture tool's own record of every acknowledged operation.
type oracleLog struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (l *oracleLog) record(iteration int, a *action) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "iter=%d ACK %s %s ops=%d\n", iteration, a.id, a.kind, len(a.ops))
	for _, o := range a.ops {
		if o.del {
			fmt.Fprintf(l.w, "  del %s\n", o.key)
		} else {
			h := fnv.New64a()
			h.Write(o.val)
			fmt.Fprintf(l.w, "  put %s len=%d fnv64a=%016x\n", o.key, len(o.val), h.Sum64())
		}
	}
}

func (l *oracleLog) note(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, format+"\n", args...)
}

func (l *oracleLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Flush()
}
