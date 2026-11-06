// Command torture kills the database at random moments and checks what
// survives against an independent oracle. README.md ("Torture tool")
// describes the modes and the output.
//
// Each iteration:
//  1. start `kvdb serve` as a child process;
//  2. run several concurrent writers against it, each sending random puts,
//     deletes and batches, one at a time, the next as soon as the previous
//     reply arrives, so every writer always has an operation in flight;
//  3. kill the child with SIGKILL (TerminateProcess on Windows) after a random
//     delay;
//  4. (recovery-crash mode) start it again and kill it again while open() is
//     recovering, one to three times;
//  5. run `kvdb open`, `kvdb check` and `kvdb dump`;
//  6. compare the dump with the oracle.
//
// The oracle is built only from the replies the tool received: an operation
// counts as promised when its ACK arrived, and is written to oracle.log. The
// database's own output is only ever the thing being judged.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"astaroth/src/token"
)

func main() {
	os.Exit(run())
}

type config struct {
	iterations int
	seed       int64
	mode       string
	writers    int
	kvdb       string
	dir        string
	keep       bool
	maxBytes   int64
}

func run() int {
	var c config
	flag.IntVar(&c.iterations, "iterations", 500, "number of kill-and-restart iterations")
	flag.Int64Var(&c.seed, "seed", 0, "random seed; the same seed gives the same operations and kill delays (0 picks one from the clock)")
	flag.StringVar(&c.mode, "mode", "normal", "normal | large-values | large-batches | recovery-crash | compaction | disk-full")
	flag.IntVar(&c.writers, "writers", 3, "concurrent writers (at least 2)")
	flag.StringVar(&c.kvdb, "kvdb", "", "path of the kvdb binary (default: kvdb next to this program, else ./kvdb)")
	flag.StringVar(&c.dir, "dir", "", "work directory for the database and oracle.log (default: a new temporary directory)")
	flag.BoolVar(&c.keep, "keep", false, "keep the work directory after a successful run")
	flag.Int64Var(&c.maxBytes, "max-bytes", -1, "override the mode's --max-bytes for kvdb (0 = unlimited)")
	flag.Parse()

	p, ok := modes[c.mode]
	if !ok || c.iterations < 1 || c.writers < 2 || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: torture [--iterations N] [--seed N] [--mode MODE] [--writers N] [--kvdb PATH] [--dir DIR] [--keep]")
		fmt.Fprintln(os.Stderr, "modes: normal, large-values, large-batches, recovery-crash, compaction, disk-full; at least 2 writers")
		return 2
	}
	if c.maxBytes >= 0 {
		p.maxBytes = c.maxBytes
	}
	if c.seed == 0 {
		c.seed = time.Now().UnixNano() % 1_000_000_000
	}
	if c.kvdb == "" {
		c.kvdb = defaultKvdb()
	}
	t, err := newTorture(c, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "torture:", err)
		return 2
	}
	return t.loop()
}

func defaultKvdb() string {
	name := "kvdb"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), name); fileExists(p) {
			return p
		}
	}
	return filepath.Join(".", name)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

type torture struct {
	c        config
	p        modeParams
	db       string
	dir      string
	madeDir  bool
	writers  []*writer
	byName   map[string]*writer
	shared   map[string]value // oracle: resolved state of the shared keys
	pre      map[string]value // oracle: preloaded keys (never written again)
	log      *oracleLog
	logFile  *os.File
	stray    []string // output lines no writer was waiting for
	strayMu  sync.Mutex
	readyEst readyInfo
	totals   totals
}

type totals struct {
	acked, puts, dels, batches, batchOps, compacts, diskFull          int
	kills, killsInFlight, tornWrites, appliedInFlight, absentInFlight int
	recoveryKills, recoveryKillsBeforeReady, killsDuringCompaction    int
	maxRecoveryMs                                                     float64
}

func newTorture(c config, p modeParams) (*torture, error) {
	t := &torture{c: c, p: p, byName: map[string]*writer{}, shared: map[string]value{}, pre: map[string]value{}}
	if !fileExists(c.kvdb) {
		return nil, fmt.Errorf("kvdb binary not found at %s (build it first, see README, or pass --kvdb)", c.kvdb)
	}
	t.dir = c.dir
	if t.dir == "" {
		d, err := os.MkdirTemp("", "torture-")
		if err != nil {
			return nil, err
		}
		t.dir, t.madeDir = d, true
	} else if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return nil, err
	}
	t.db = filepath.Join(t.dir, "torture.db")
	for _, f := range []string{t.db, t.db + ".compact.tmp"} { // the oracle starts empty, so must the database
		os.Remove(f)
	}
	f, err := os.Create(filepath.Join(t.dir, "oracle.log"))
	if err != nil {
		return nil, err
	}
	t.logFile = f
	t.log = &oracleLog{w: bufio.NewWriterSize(f, 1<<20)}
	for i := 0; i < c.writers; i++ {
		w := &writer{n: i, name: fmt.Sprintf("w%d", i), p: p, own: map[string]value{}, log: t.log}
		t.writers = append(t.writers, w)
		t.byName[w.name] = w
	}
	return t, nil
}

func (t *torture) loop() int {
	fmt.Printf("torture: mode %s (%s)\n", t.c.mode, t.p.desc)
	fmt.Printf("torture: %d iterations, seed %d, %d concurrent writers, kill after %v to %v, kvdb %s\n",
		t.c.iterations, t.c.seed, t.c.writers, t.p.delayMin, t.p.delayMax, t.c.kvdb)
	fmt.Printf("torture: work directory %s\n", t.dir)
	t.log.note("torture mode=%s seed=%d iterations=%d writers=%d", t.c.mode, t.c.seed, t.c.iterations, t.c.writers)
	if t.p.preload > 0 {
		if err := t.preload(); err != nil {
			fmt.Fprintln(os.Stderr, "torture: preload:", err)
			return 2
		}
	}
	start := time.Now()
	for it := 1; it <= t.c.iterations; it++ {
		if v := t.iteration(it); v != nil {
			t.log.flush()
			t.report(it, v)
			return 1
		}
	}
	t.log.flush()
	t.logFile.Close()
	t.summary(time.Since(start))
	if t.madeDir && !t.c.keep {
		os.RemoveAll(t.dir)
	}
	return 0
}

// preload writes keys pre/0 .. pre/N-1 with `kvdb batch`, 10,000 per batch,
// so that recovery has real work to do and kills can land inside it.
func (t *torture) preload() error {
	fmt.Printf("torture: preloading %d keys\n", t.p.preload)
	for start := 0; start < t.p.preload; start += 10000 {
		var b strings.Builder
		for i := start; i < start+10000 && i < t.p.preload; i++ {
			k, v := fmt.Sprintf("pre/%d", i), fmt.Sprintf("preloaded value %d", i)
			fmt.Fprintf(&b, "put %s %s\n", k, v)
			t.pre[k] = value{data: []byte(v), op: "preload"}
		}
		file := filepath.Join(t.dir, "preload.txt")
		if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
			return err
		}
		out, errOut, code, err := runKvdb(t.c.kvdb, "batch", t.db, file)
		if err != nil || code != 0 {
			return fmt.Errorf("kvdb batch: exit %d %v %s %s", code, err, out, errOut)
		}
		os.Remove(file)
	}
	t.log.note("preload %d keys pre/0..pre/%d (acknowledged by kvdb batch exit 0)", t.p.preload, t.p.preload-1)
	return nil
}

// violation is a broken promise, or anything else that must never happen.
type violation struct {
	what     string
	expected []string
	actual   []string
	ops      []string
	scenario []string
}

func (t *torture) route(line string) {
	f := strings.SplitN(line, " ", 3)
	if len(f) >= 2 {
		name := f[1]
		if i := strings.IndexByte(name, '.'); i > 0 {
			name = name[:i]
		}
		if w := t.byName[name]; w != nil {
			w.replies <- line
			return
		}
	}
	t.strayMu.Lock()
	t.stray = append(t.stray, line)
	t.strayMu.Unlock()
}

func (t *torture) iteration(it int) *violation {
	rng := rand.New(rand.NewSource(t.c.seed*1_000_003 + int64(it)))
	for _, w := range t.writers {
		w.rng = rand.New(rand.NewSource(t.c.seed + int64(it)*7919 + int64(w.n)*104729))
		w.acked, w.ackedID = map[string]*value{}, map[string]string{}
		w.inflight, w.stats, w.fail, w.lastAck = nil, iterStats{}, nil, ""
		w.replies = make(chan string, 64)
	}
	t.stray = nil
	scenario := []string{fmt.Sprintf("iteration %d, mode %s, seed %d", it, t.c.mode, t.c.seed)}

	// 1-3: serve, concurrent writers, a kill after a random delay.
	proc, err := startServe(t.c.kvdb, t.db, t.p.maxBytes, t.route)
	if err != nil {
		return &violation{what: "could not start kvdb serve: " + err.Error(), scenario: scenario}
	}
	ready, err := proc.waitReady(60 * time.Second)
	if err != nil {
		return &violation{what: "open() failed on a file the database produced (invariant 5): " + err.Error(), scenario: scenario}
	}
	t.readyEst = ready
	if ready.recoveryM > t.totals.maxRecoveryMs {
		t.totals.maxRecoveryMs = ready.recoveryM
	}
	var wg sync.WaitGroup
	for _, w := range t.writers {
		wg.Add(1)
		go w.run(proc, it, &wg)
	}
	delay := t.p.delayMin + time.Duration(rng.Int63n(int64(t.p.delayMax-t.p.delayMin)+1))
	time.Sleep(delay)
	proc.kill()
	for _, w := range t.writers {
		close(w.replies)
	}
	wg.Wait()
	t.totals.kills++
	scenario = append(scenario, fmt.Sprintf("kvdb serve killed with SIGKILL %v after READY", delay.Round(time.Millisecond)))

	inFlight, compacting := 0, false
	for _, w := range t.writers {
		if w.fail != nil {
			return &violation{what: "protocol error: " + w.fail.Error(), scenario: scenario}
		}
		if w.inflight != nil {
			inFlight++
			scenario = append(scenario, fmt.Sprintf("writer %s had %s in flight (sent %v before the kill); last acknowledged: %s",
				w.name, w.inflight, time.Since(w.inflight.sentAt).Round(time.Millisecond), orNone(w.lastAck)))
			compacting = compacting || w.inflight.kind == "compact"
		} else {
			scenario = append(scenario, fmt.Sprintf("writer %s had nothing in flight; last acknowledged: %s", w.name, orNone(w.lastAck)))
		}
		t.totals.add(w.stats)
	}
	if inFlight > 0 {
		t.totals.killsInFlight++
	}
	if compacting {
		t.totals.killsDuringCompaction++
	}
	if len(t.stray) > 0 {
		return &violation{what: "serve wrote lines no request asked for: " + strings.Join(t.stray, " | "), scenario: scenario}
	}

	// 4: kills while open() is recovering.
	recKills, recBefore := 0, 0
	if t.p.recoveryKillsMax > 0 {
		n := 1 + rng.Intn(t.p.recoveryKillsMax)
		for j := 0; j < n; j++ {
			before, err := t.killDuringOpen(rng)
			if err != nil {
				return &violation{what: err.Error(), scenario: scenario}
			}
			recKills++
			if before {
				recBefore++
			}
		}
		t.totals.recoveryKills += recKills
		t.totals.recoveryKillsBeforeReady += recBefore
		scenario = append(scenario, fmt.Sprintf("then killed %d more times while restarting (%d of them before recovery finished)", recKills, recBefore))
	}

	// 5: open, check, dump.
	maxArg := strconv.FormatInt(t.p.maxBytes, 10)
	out, errOut, code, err := runKvdb(t.c.kvdb, "open", t.db, "--max-bytes", maxArg)
	oi, ok := parseOpen(out)
	if err != nil || code != 0 || !ok {
		return &violation{what: "open() failed on a file the database produced (invariant 5)",
			actual: []string{fmt.Sprintf("kvdb open exit %d: %s %s %v", code, out, errOut, err)}, scenario: scenario}
	}
	if oi.tornTail > 0 {
		t.totals.tornWrites++
	}
	if oi.recoveryM > t.totals.maxRecoveryMs {
		t.totals.maxRecoveryMs = oi.recoveryM
	}
	out, errOut, code, err = runKvdb(t.c.kvdb, "check", t.db)
	if err != nil || code != 0 || !strings.HasPrefix(out, "OK ") || strings.Contains(out, "torn tail") {
		return &violation{what: "check did not report OK after recovery",
			actual: []string{fmt.Sprintf("kvdb check exit %d: %s %s %v", code, strings.TrimSpace(out), errOut, err)}, scenario: scenario}
	}
	if t.p.maxBytes > 0 {
		if m := checkBytesRe.FindStringSubmatch(out); m != nil {
			if n, _ := strconv.ParseInt(m[1], 10, 64); n > t.p.maxBytes {
				return &violation{what: fmt.Sprintf("data file is %d bytes, over the %d-byte limit", n, t.p.maxBytes), scenario: scenario}
			}
		}
	}
	dumpOut, errOut, code, err := runKvdb(t.c.kvdb, "dump", t.db)
	if err != nil || code != 0 {
		return &violation{what: "dump failed", actual: []string{fmt.Sprintf("exit %d: %s %v", code, errOut, err)}, scenario: scenario}
	}
	actual, err := parseDump(dumpOut)
	if err != nil {
		return &violation{what: "dump output does not parse: " + err.Error(), scenario: scenario}
	}

	// 6: compare with the oracle.
	if v := t.verify(actual); v != nil {
		v.scenario = append(scenario, fmt.Sprintf("open reported: keys=%d frames=%d torn_tail_bytes=%d", oi.keys, oi.frames, oi.tornTail))
		return v
	}

	extra := ""
	switch {
	case t.p.recoveryKillsMax > 0:
		extra = fmt.Sprintf(" | %d kills during restart (%d before READY)", recKills, recBefore)
	case t.p.wCompact > 0:
		c := 0
		for _, w := range t.writers {
			c += w.stats.compacts
		}
		extra = fmt.Sprintf(" | %d compactions", c)
		if compacting {
			extra += ", killed during one"
		}
	case t.p.maxBytes > 0:
		d := 0
		for _, w := range t.writers {
			d += w.stats.diskFull
		}
		extra = fmt.Sprintf(" | %d disk-full errors", d)
	}
	acked := 0
	for _, w := range t.writers {
		acked += w.stats.acked
	}
	fmt.Printf("[%*d/%d] killed %4dms after READY | %4d ops acknowledged | %d/%d writers had an op in flight | torn tail %6dB | recovery %6.2fms | %d keys%s | OK\n",
		len(strconv.Itoa(t.c.iterations)), it, t.c.iterations, delay.Milliseconds(), acked, inFlight, len(t.writers),
		oi.tornTail, oi.recoveryM, len(actual), extra)
	return nil
}

// killDuringOpen starts serve and kills it after a delay chosen to land while
// open() is still recovering: past the process start-up, inside the recovery
// time measured on the last successful start. It reports whether the kill
// came before READY, i.e. before recovery had finished.
func (t *torture) killDuringOpen(rng *rand.Rand) (bool, error) {
	startup := t.readyEst.at - time.Duration(t.readyEst.recoveryM*float64(time.Millisecond))
	if startup < 0 {
		startup = 0
	}
	recovery := time.Duration(t.readyEst.recoveryM * 1.5 * float64(time.Millisecond))
	lo := startup * 6 / 10
	hi := startup + recovery + 2*time.Millisecond
	delay := lo + time.Duration(rng.Int63n(int64(hi-lo)+1))
	p, err := startServe(t.c.kvdb, t.db, t.p.maxBytes, nil)
	if err != nil {
		return false, err
	}
	time.Sleep(delay)
	before := !p.sawReady()
	p.kill()
	return before, nil
}

func (s *totals) add(w iterStats) {
	s.acked += w.acked
	s.puts += w.puts
	s.dels += w.dels
	s.batches += w.batches
	s.batchOps += w.batchOps
	s.compacts += w.compacts
	s.diskFull += w.diskFull
}

func orNone(s string) string {
	if s == "" {
		return "none in this iteration"
	}
	return s
}

// --- verification -----------------------------------------------------------

// verify compares the recovered database with the oracle and, if they agree,
// resolves the in-flight operations: whatever the database shows for them is
// the state the next iteration starts from.
func (t *torture) verify(actual map[string][]byte) *violation {
	classified := 0

	// Each writer's own keys: exactly what it had acknowledged, or that plus
	// its in-flight operation as a whole. Anything else is a lost or changed
	// acknowledged write, or a partly applied batch.
	for _, w := range t.writers {
		prefix := w.name + "/"
		got := map[string][]byte{}
		for k, v := range actual {
			if strings.HasPrefix(k, prefix) {
				got[k] = v
			}
		}
		classified += len(got)
		a := w.own
		var b map[string]value
		if w.inflight != nil && !w.inflight.shared && w.inflight.kind != "compact" {
			b = make(map[string]value, len(a)+len(w.inflight.ops))
			for k, v := range a {
				b[k] = v
			}
			w.inflight.applyTo(b)
		}
		switch {
		case sameAs(got, a):
			if b != nil && !sameMaps(a, b) {
				t.totals.absentInFlight++
			}
		case b != nil && sameAs(got, b):
			w.own = b
			t.totals.appliedInFlight++
		default:
			return t.ownViolation(w, got, a, b)
		}
	}

	// Shared keys: written by several writers, so the order between writers is
	// not known to the client. The value must be the last acknowledged action
	// of one of the writers that acknowledged one this iteration (or, if none
	// did, the state before), or an in-flight action on that key.
	for i := 0; i < t.p.sharedKeys; i++ {
		k := sharedKey(i)
		got, present := actual[k]
		if present {
			classified++
		}
		type cand struct {
			v    *value // nil: absent
			from string
		}
		var cands []cand
		anyAcked := false
		for _, w := range t.writers {
			if v, ok := w.acked[k]; ok {
				anyAcked = true
				if v.data == nil && v.op != "" && !hasPut(v) {
					cands = append(cands, cand{nil, "deleted by " + v.op + " (acknowledged)"})
				} else {
					cands = append(cands, cand{v, v.op + " (acknowledged)"})
				}
			}
		}
		if !anyAcked {
			if v, ok := t.shared[k]; ok {
				vv := v
				cands = append(cands, cand{&vv, vv.op + " (state before this iteration)"})
			} else {
				cands = append(cands, cand{nil, "absent before this iteration"})
			}
		}
		for _, w := range t.writers {
			if a := w.inflight; a != nil && a.shared && a.ops[0].key == k {
				if a.ops[0].del {
					cands = append(cands, cand{nil, "deleted by " + a.id + " (in flight)"})
				} else {
					cands = append(cands, cand{&value{data: a.ops[0].val, op: a.id}, a.id + " (in flight)"})
				}
			}
		}
		matched := false
		for _, c := range cands {
			if (c.v == nil && !present) || (c.v != nil && present && bytes.Equal(c.v.data, got)) {
				matched = true
				if present {
					t.shared[k] = value{data: got, op: c.v.op}
				} else {
					delete(t.shared, k)
				}
				break
			}
		}
		if !matched {
			v := &violation{what: fmt.Sprintf("shared key %s holds a value no writer could have left there", k)}
			for _, c := range cands {
				v.expected = append(v.expected, fmt.Sprintf("%s = %s  [%s]", k, show(c.v), c.from))
				v.ops = append(v.ops, strings.Fields(c.from)[0])
			}
			v.actual = []string{fmt.Sprintf("%s = %s", k, showBytes(got, present))}
			return v
		}
	}

	// Preloaded keys: written once, before the first kill.
	for k, want := range t.pre {
		got, ok := actual[k]
		if !ok || !bytes.Equal(got, want.data) {
			return &violation{what: "acknowledged preloaded key lost or changed",
				expected: []string{fmt.Sprintf("%s = %s  [preload, acknowledged]", k, show(&want))},
				actual:   []string{fmt.Sprintf("%s = %s", k, showBytes(got, ok))}, ops: []string{"preload"}}
		}
		classified++
	}

	if classified != len(actual) {
		var unknown []string
		for k := range actual {
			if !t.knownKey(k) {
				unknown = append(unknown, fmt.Sprintf("%s = %s", token.Key([]byte(k)), showBytes(actual[k], true)))
			}
		}
		sort.Strings(unknown)
		if len(unknown) > 10 {
			unknown = append(unknown[:10], fmt.Sprintf("... %d more", len(unknown)-10))
		}
		return &violation{what: "the database holds keys that were never written", actual: unknown}
	}
	return nil
}

func hasPut(v *value) bool { return v.data != nil }

func (t *torture) knownKey(k string) bool {
	if _, ok := t.pre[k]; ok {
		return true
	}
	for _, w := range t.writers {
		if strings.HasPrefix(k, w.name+"/") {
			return true
		}
	}
	for i := 0; i < t.p.sharedKeys; i++ {
		if k == sharedKey(i) {
			return true
		}
	}
	return false
}

// ownViolation explains a mismatch on one writer's keys.
func (t *torture) ownViolation(w *writer, got map[string][]byte, a, b map[string]value) *violation {
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	v := &violation{}
	lost, partialA, partialB := 0, 0, 0
	var inBatch map[string]bool
	if b != nil && w.inflight.kind == "batch" {
		inBatch = map[string]bool{}
		for _, o := range w.inflight.ops {
			inBatch[o.key] = true
		}
	}
	shown := 0
	for _, k := range sorted {
		gv, gok := got[k]
		av, aok := a[k]
		matchA := gok == aok && (!gok || bytes.Equal(gv, av.data))
		matchB := false
		var bv value
		var bok bool
		if b != nil {
			bv, bok = b[k]
			matchB = gok == bok && (!gok || bytes.Equal(gv, bv.data))
		}
		if inBatch != nil && inBatch[k] && !sameValue(av, aok, bv, bok) {
			if matchA {
				partialA++
			} else if matchB {
				partialB++
			}
		}
		if matchA || (b == nil && matchA) {
			continue
		}
		if b != nil && matchB && (inBatch == nil || inBatch[k]) {
			continue
		}
		if aok && !matchA {
			lost++
		}
		if shown < 12 {
			exp := fmt.Sprintf("%s = %s  [%s]", k, showValue(av, aok), opOf(av, aok, "acknowledged"))
			if b != nil && !sameValue(av, aok, bv, bok) {
				exp += fmt.Sprintf("   or, if %s was applied: %s", w.inflight.id, showValue(bv, bok))
			}
			v.expected = append(v.expected, exp)
			v.actual = append(v.actual, fmt.Sprintf("%s = %s", k, showBytes(gv, gok)))
			if aok {
				v.ops = append(v.ops, av.op)
			}
			shown++
		}
	}
	switch {
	case inBatch != nil && partialA > 0 && partialB > 0:
		v.what = fmt.Sprintf("partially committed batch: %d of the %d keys batch %s changes show it applied, %d show it absent",
			partialB, partialA+partialB, w.inflight.id, partialA)
	case lost > 0:
		v.what = fmt.Sprintf("acknowledged write lost or changed (writer %s)", w.name)
	default:
		v.what = fmt.Sprintf("unexpected value (writer %s)", w.name)
	}
	if w.inflight != nil {
		v.ops = append(v.ops, w.inflight.id+" (in flight)")
	}
	v.ops = dedup(v.ops)
	return v
}

func sameValue(a value, aok bool, b value, bok bool) bool {
	return aok == bok && (!aok || bytes.Equal(a.data, b.data))
}

func sameAs(got map[string][]byte, want map[string]value) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		g, ok := got[k]
		if !ok || !bytes.Equal(g, v.data) {
			return false
		}
	}
	return true
}

func sameMaps(a, b map[string]value) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !bytes.Equal(v.data, w.data) {
			return false
		}
	}
	return true
}

func dedup(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func opOf(v value, ok bool, how string) string {
	if !ok {
		return "absent"
	}
	return "written by " + v.op + ", " + how
}

func show(v *value) string {
	if v == nil || v.data == nil {
		return "(absent)"
	}
	return showBytes(v.data, true)
}

func showValue(v value, ok bool) string { return showBytes(v.data, ok) }

func showBytes(b []byte, ok bool) string {
	if !ok {
		return "(absent)"
	}
	s := token.Value(b)
	if len(s) > 60 {
		s = s[:60] + "..."
	}
	return fmt.Sprintf("%s (%d bytes)", s, len(b))
}

// --- output -----------------------------------------------------------------

func (t *torture) report(it int, v *violation) {
	fmt.Println()
	fmt.Println("FAIL: correctness violation")
	fmt.Printf("  what:      %s\n", v.what)
	fmt.Printf("  iteration: %d of %d\n", it, t.c.iterations)
	fmt.Printf("  seed:      %d\n", t.c.seed)
	fmt.Printf("  mode:      %s\n", t.c.mode)
	fmt.Println("  failure scenario:")
	for _, s := range v.scenario {
		fmt.Printf("    - %s\n", s)
	}
	if len(v.expected) > 0 {
		fmt.Println("  expected state (from the oracle):")
		for _, s := range v.expected {
			fmt.Printf("    %s\n", s)
		}
	}
	if len(v.actual) > 0 {
		fmt.Println("  actual state (from kvdb dump after recovery):")
		for _, s := range v.actual {
			fmt.Printf("    %s\n", s)
		}
	}
	if len(v.ops) > 0 {
		fmt.Printf("  operation ids involved: %s\n", strings.Join(v.ops, ", "))
	}
	fmt.Printf("  reproduce: torture --iterations %d --seed %d --mode %s --writers %d\n", it, t.c.seed, t.c.mode, t.c.writers)
	fmt.Printf("  (the same seed gives the same operations and kill delays; where exactly a kill lands also depends on timing)\n")
	fmt.Printf("  kept for inspection: %s (database torture.db, oracle oracle.log)\n", t.dir)
}

func (t *torture) summary(d time.Duration) {
	s := t.totals
	fmt.Println()
	fmt.Printf("PASS: %d of %d iterations, mode %s, seed %d, %d writers, %v\n",
		t.c.iterations, t.c.iterations, t.c.mode, t.c.seed, t.c.writers, d.Round(time.Second))
	fmt.Printf("  acknowledged operations: %d (puts %d, deletes %d, batches %d with %d operations, compactions %d)\n",
		s.acked, s.puts, s.dels, s.batches, s.batchOps, s.compacts)
	fmt.Printf("  kills: %d with SIGKILL; %d landed while operations were in flight; %d tore a write in progress (torn tail removed by recovery)\n",
		s.kills, s.killsInFlight, s.tornWrites)
	fmt.Printf("  in-flight operations whose outcome was unknown: %d found applied in full, %d found absent, none partial\n",
		s.appliedInFlight, s.absentInFlight)
	if s.recoveryKills > 0 {
		fmt.Printf("  kills during restart: %d, of which %d before recovery finished (READY not yet printed)\n", s.recoveryKills, s.recoveryKillsBeforeReady)
	}
	if t.p.wCompact > 0 {
		fmt.Printf("  kills while a compaction was in flight: %d\n", s.killsDuringCompaction)
	}
	if t.p.maxBytes > 0 {
		fmt.Printf("  writes rejected by the %d-byte limit: %d, every one with no effect\n", t.p.maxBytes, s.diskFull)
		if s.diskFull == 0 && t.p.expectDiskFullErrors {
			fmt.Println("  WARNING: the size limit was never reached; raise --iterations or lower --max-bytes")
		}
	}
	fmt.Printf("  slowest recovery: %.2f ms\n", s.maxRecoveryMs)
}
