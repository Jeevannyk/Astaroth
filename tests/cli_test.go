package tests

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"astaroth/src/engine"
	"astaroth/src/storage"
	"astaroth/src/token"
	"astaroth/tests/testdb"
)

// End-to-end tests of the kvdb command, run as a separate process exactly as
// the judge runs it. The binary is built once per test run into a temporary
// directory. Plain put/get/del/batch run on the permanent database with
// run-unique keys; tests that need a fresh file (exact dump output, check on a
// damaged file, kills) use t.TempDir().

var (
	binOnce sync.Once
	binDir  string
	binPath string
	binErr  error
)

func kvdbBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		if binDir, binErr = os.MkdirTemp("", "astaroth-bin-"); binErr != nil {
			return
		}
		name := "kvdb"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binPath = filepath.Join(binDir, name)
		// The torture binary goes next to kvdb, which is where it looks for it.
		out, err := exec.Command("go", "build", "-o", binDir+string(filepath.Separator), "astaroth/cmd/kvdb", "astaroth/torture").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("building kvdb and torture: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

// removeBinaries is called by TestMain after the tests.
func removeBinaries() {
	if binDir != "" {
		os.RemoveAll(binDir)
	}
}

// kvdb runs one command and returns its exit code and output.
func kvdb(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(kvdbBinary(t), args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running kvdb %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, out.String(), errb.String()
}

// kvdbOK runs a command that must succeed and returns its stdout.
func kvdbOK(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := kvdb(t, "", args...)
	if code != 0 {
		t.Fatalf("kvdb %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// parseDump turns dump output back into a map, decoding the text form.
func parseDump(t *testing.T, out string) map[string]string {
	t.Helper()
	m := map[string]string{}
	prev := ""
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" && out == "" {
			break
		}
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("dump line %d %q has no value", i+1, line)
		}
		kb, err1 := token.Decode(k)
		vb, err2 := token.Decode(v)
		if err1 != nil || err2 != nil {
			t.Fatalf("dump line %d %q does not decode: %v %v", i+1, line, err1, err2)
		}
		if i > 0 && string(kb) <= prev {
			t.Fatalf("dump is not sorted by key at line %d", i+1)
		}
		prev = string(kb)
		m[string(kb)] = string(vb)
	}
	return m
}

var cliOpenLine = regexp.MustCompile(`^keys=(\d+) recovery_ms=\d+\.\d{3} frames=(\d+) torn_tail_bytes=(\d+)\n$`)

// --- open ---------------------------------------------------------------

func TestCLIOpen(t *testing.T) {
	path := newPath(t)
	out := kvdbOK(t, "open", path)
	if m := cliOpenLine.FindStringSubmatch(out); m == nil || m[1] != "0" {
		t.Fatalf("open of a new database printed %q", out)
	}
	if fileSize(t, path) != storage.HeaderSize {
		t.Fatalf("new database file is %d bytes", fileSize(t, path))
	}
	kvdbOK(t, "put", path, "a", "1")
	kvdbOK(t, "put", path, "b", "2")

	// A torn tail: open reports and removes it.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{1, 2, 3, 4, 5})
	f.Close()
	out = kvdbOK(t, "open", path, "--max-bytes", "1000000")
	if m := cliOpenLine.FindStringSubmatch(out); m == nil || m[1] != "2" || m[2] != "2" || m[3] != "5" {
		t.Fatalf("open after a torn tail printed %q", out)
	}
	if m := cliOpenLine.FindStringSubmatch(kvdbOK(t, "open", path)); m[3] != "0" {
		t.Fatal("torn tail reported again after it was removed")
	}

	if code, _, errOut := kvdb(t, "", "open", path, "--max-bytes", "10"); code != 1 || !strings.Contains(errOut, "MaxBytes") {
		t.Fatalf("--max-bytes 10: exit %d, %q", code, errOut)
	}
	foreign := writeTemp(t, "foreign.db", strings.Repeat("x", 100))
	if code, _, errOut := kvdb(t, "", "open", foreign); code != 1 || !strings.Contains(errOut, "not an Astaroth database") {
		t.Fatalf("foreign file: exit %d, %q", code, errOut)
	}
	if code, _, errOut := kvdb(t, "", "open"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Fatalf("no path: exit %d, %q", code, errOut)
	}
}

// --- put, get, del --------------------------------------------------------

func TestCLIPutGetDel(t *testing.T) {
	path := testdb.Path(t)
	k := func(s string) string { return testdb.Prefix(t) + s }
	get := func(key string) string { return strings.TrimSuffix(kvdbOK(t, "get", path, key), "\n") }

	if out := kvdbOK(t, "put", path, k("greeting"), "hello world"); out != "OK\n" {
		t.Fatalf("put printed %q", out)
	}
	if got := get(k("greeting")); got != "hello world" {
		t.Fatalf("get = %q", got)
	}
	kvdbOK(t, "put", path, k("greeting"), "second")
	if got := get(k("greeting")); got != "second" {
		t.Fatalf("after overwrite get = %q", got)
	}
	if got := get(k("never-written")); got != "NOTFOUND" {
		t.Fatalf("missing key: get = %q", got)
	}
	if out := kvdbOK(t, "del", path, k("greeting")); out != "OK\n" {
		t.Fatalf("del printed %q", out)
	}
	if got := get(k("greeting")); got != "NOTFOUND" {
		t.Fatalf("after del get = %q", got)
	}
	kvdbOK(t, "del", path, k("never-written")) // deleting a missing key is not an error

	// Binary-safe: keys and values of any bytes through hex:.
	binKey := "hex:" + hex.EncodeToString([]byte(k("bin\x00\xff key")))
	kvdbOK(t, "put", path, binKey, "hex:00ff0a0d20")
	if got := get(binKey); got != "hex:00ff0a0d20" {
		t.Fatalf("binary value printed as %q", got)
	}
	kvdbOK(t, "put", path, k("empty"), "hex:")
	if got := get(k("empty")); got != "hex:" {
		t.Fatalf("empty value printed as %q", got)
	}
	kvdbOK(t, "put", path, k("word"), "NOTFOUND") // a value that looks like the not-found marker
	if got := get(k("word")); got != "hex:4e4f54464f554e44" {
		t.Fatalf("value NOTFOUND printed as %q", got)
	}
	kvdbOK(t, "put", path, k("dash"), "-5") // a value starting with '-' is not an option
	if got := get(k("dash")); got != "-5" {
		t.Fatalf("value -5 printed as %q", got)
	}
	big := strings.Repeat("ab", 5000) // 10 KB of hex on the command line
	kvdbOK(t, "put", path, k("big"), "hex:"+big)
	if got := get(k("big")); got != "hex:"+big {
		t.Fatalf("10 KB binary value came back different (%d chars)", len(got))
	}

	if code, _, errOut := kvdb(t, "", "put", path, strings.Repeat("k", 1025), "v"); code != 1 || !strings.Contains(errOut, "key length") {
		t.Fatalf("1025-byte key: exit %d, %q", code, errOut)
	}
	if code, _, errOut := kvdb(t, "", "get", path); code != 2 || errOut == "" {
		t.Fatalf("get without key: exit %d, %q", code, errOut)
	}
}

// --- batch ----------------------------------------------------------------

func TestCLIBatch(t *testing.T) {
	path := testdb.Path(t)
	p := testdb.Prefix(t)
	file := writeTemp(t, "ops.txt", strings.Join([]string{
		"# comment lines and blank lines are ignored",
		"",
		"put " + p + "a 1",
		"put " + p + "b value with spaces",
		"put " + p + "c 3",
		"del " + p + "c",
		"put " + p + "d first",
		"put " + p + "d second", // last operation on a key wins
		"put hex:" + hex.EncodeToString([]byte(p+"e")) + " hex:00ff",
	}, "\r\n")+"\r\n") // Windows line endings
	if out := kvdbOK(t, "batch", path, file); out != "OK 7 operations\n" {
		t.Fatalf("batch printed %q", out)
	}
	for k, want := range map[string]string{"a": "1", "b": "value with spaces", "c": "NOTFOUND", "d": "second", "e": "hex:00ff"} {
		if got := strings.TrimSpace(kvdbOK(t, "get", path, p+k)); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}

	// A bad line anywhere rejects the whole file: nothing is written.
	bad := writeTemp(t, "bad.txt", "put "+p+"x1 1\nput "+p+"x2 2\nput "+p+"x3\nput "+p+"x4 4\n")
	code, _, errOut := kvdb(t, "", "batch", path, bad)
	if code != 1 || !strings.Contains(errOut, "line 3") || !strings.Contains(errOut, "nothing was written") {
		t.Fatalf("bad line: exit %d, %q", code, errOut)
	}
	if got := strings.TrimSpace(kvdbOK(t, "get", path, p+"x1")); got != "NOTFOUND" {
		t.Fatalf("x1 = %q after a rejected batch", got)
	}

	// Standard input, and the 10,000-operation limit.
	var lines []string
	for i := 0; i < storage.MaxOpsPerFrame; i++ {
		lines = append(lines, fmt.Sprintf("put %sn%d %d", p, i, i))
	}
	code, out, errOut := kvdb(t, strings.Join(lines, "\n"), "batch", path, "-")
	if code != 0 || out != "OK 10000 operations\n" {
		t.Fatalf("10,000 operations from stdin: exit %d, %q %q", code, out, errOut)
	}
	if got := strings.TrimSpace(kvdbOK(t, "get", path, p+"n9999")); got != "9999" {
		t.Fatalf("n9999 = %q", got)
	}
	lines = append(lines, "put "+p+"over 1")
	code, _, errOut = kvdb(t, strings.Join(lines, "\n"), "batch", path, "-")
	if code != 1 || !strings.Contains(errOut, "10000") {
		t.Fatalf("10,001 operations: exit %d, %q", code, errOut)
	}
	if got := strings.TrimSpace(kvdbOK(t, "get", path, p+"over")); got != "NOTFOUND" {
		t.Fatal("part of a rejected 10,001-operation batch was written")
	}
}

// --- dump -----------------------------------------------------------------

func TestCLIDump(t *testing.T) {
	path := newPath(t)
	kvdbOK(t, "open", path)
	if out := kvdbOK(t, "dump", path); out != "" {
		t.Fatalf("dump of an empty database printed %q", out)
	}
	file := writeTemp(t, "ops.txt", strings.Join([]string{
		"put zebra last",
		"put apple first one",
		"put hex:00 zero byte key",
		"put mango hex:",
		"put kiwi NOTFOUND",
		"put hex:6120 key with a space",
		"put banana hex:0a",
		"put cherry gone",
		"del cherry",
	}, "\n"))
	kvdbOK(t, "batch", path, file)

	want := "hex:00 zero byte key\n" +
		"hex:6120 key with a space\n" +
		"apple first one\n" +
		"banana hex:0a\n" +
		"kiwi hex:4e4f54464f554e44\n" +
		"mango hex:\n" +
		"zebra last\n"
	out := kvdbOK(t, "dump", path)
	if out != want {
		t.Fatalf("dump printed\n%s\nwant\n%s", out, want)
	}
	got := parseDump(t, out)
	model := map[string]string{"\x00": "zero byte key", "a ": "key with a space", "apple": "first one",
		"banana": "\n", "kiwi": "NOTFOUND", "mango": "", "zebra": "last"}
	if !sameState(got, model) {
		t.Fatalf("dump decodes to %q, want %q", got, model)
	}
}

// --- check ----------------------------------------------------------------

var checkOK = regexp.MustCompile(`^OK keys=(\d+) frames=(\d+) bytes=(\d+) generation=(\d+)( \(.*\))?\n$`)

func TestCLICheck(t *testing.T) {
	path := newPath(t)
	kvdbOK(t, "put", path, "a", "1")
	kvdbOK(t, "put", path, "b", "2")
	kvdbOK(t, "del", path, "a")
	out := kvdbOK(t, "check", path)
	if m := checkOK.FindStringSubmatch(out); m == nil || m[1] != "1" || m[2] != "3" || m[4] != "1" || m[5] != "" {
		t.Fatalf("check of a clean database printed %q", out)
	}

	// A torn tail is reported but not removed: check only reads.
	size := fileSize(t, path)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte("partial"))
	f.Close()
	out = kvdbOK(t, "check", path)
	if !strings.HasPrefix(out, "OK ") || !strings.Contains(out, fmt.Sprintf("torn tail: 7 bytes at offset %d", size)) {
		t.Fatalf("check with a torn tail printed %q", out)
	}
	if fileSize(t, path) != size+7 {
		t.Fatal("check changed the file")
	}
	kvdbOK(t, "open", path)
	if out := kvdbOK(t, "check", path); strings.Contains(out, "torn") {
		t.Fatalf("torn tail still reported after open: %q", out)
	}

	// An abandoned compaction file is reported, and left for open to remove.
	tmp := path + storage.CompactTempSuffix
	os.WriteFile(tmp, []byte("half a compaction"), 0o644)
	if out := kvdbOK(t, "check", path); !strings.Contains(out, "abandoned compaction file") {
		t.Fatalf("check with an abandoned temporary file printed %q", out)
	}
	if !exists(tmp) {
		t.Fatal("check removed the temporary file")
	}
	kvdbOK(t, "open", path)
	if exists(tmp) {
		t.Fatal("open did not remove the abandoned temporary file")
	}

	// Damage no crash can produce: a gap in the transaction ids.
	gap, err := storage.EncodeFrame(99, []storage.Op{{Type: storage.OpPut, Key: []byte("x"), Value: []byte("y")}})
	if err != nil {
		t.Fatal(err)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(gap)
	f.Close()
	code, out, _ := kvdb(t, "", "check", path)
	if code != 1 || !strings.HasPrefix(out, "CORRUPT: ") || !strings.Contains(out, "seq") {
		t.Fatalf("check of a corrupt file: exit %d, %q", code, out)
	}
	foreign := writeTemp(t, "foreign.db", strings.Repeat("x", 100))
	if code, out, _ := kvdb(t, "", "check", foreign); code != 1 || !strings.Contains(out, "CORRUPT: ") {
		t.Fatalf("check of a foreign file: exit %d, %q", code, out)
	}
	if code, _, errOut := kvdb(t, "", "check", filepath.Join(t.TempDir(), "absent")); code != 1 || !strings.Contains(errOut, "no database") {
		t.Fatalf("check of a missing file: exit %d, %q", code, errOut)
	}
}

func TestCLICheckWhileServeRuns(t *testing.T) {
	path := newPath(t)
	p := startServe(t, path)
	code, _, errOut := kvdb(t, "", "check", path)
	if code != 1 || !strings.Contains(errOut, "in use by another process") {
		t.Fatalf("check while serve holds the database: exit %d, %q", code, errOut)
	}
	p.finish(t)
}

// --- serve ----------------------------------------------------------------

type serveProc struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string
	ready string
	errb  bytes.Buffer
}

func startServe(t *testing.T, path string, extra ...string) *serveProc {
	t.Helper()
	p := &serveProc{lines: make(chan string, 1<<16)}
	p.cmd = exec.Command(kvdbBinary(t), append([]string{"serve", path}, extra...)...)
	var err error
	if p.in, err = p.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.cmd.Stderr = &p.errb
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 8<<20)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	p.ready = p.next(t)
	if !strings.HasPrefix(p.ready, "READY keys=") {
		t.Fatalf("serve started with %q (stderr %s)", p.ready, p.errb.String())
	}
	return p
}

func (p *serveProc) send(line string) { io.WriteString(p.in, line+"\n") }

func (p *serveProc) next(t *testing.T) string {
	t.Helper()
	select {
	case l, ok := <-p.lines:
		if !ok {
			t.Fatalf("serve closed its output (stderr %s)", p.errb.String())
		}
		return l
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for serve")
	}
	return ""
}

// request sends one line and returns the reply.
func (p *serveProc) request(t *testing.T, line string) string {
	t.Helper()
	p.send(line)
	return p.next(t)
}

// drain returns every remaining output line once the process has ended.
func (p *serveProc) drain() []string {
	var rest []string
	for l := range p.lines {
		rest = append(rest, l)
	}
	p.cmd.Wait()
	return rest
}

// finish closes serve's input and waits for a clean exit.
func (p *serveProc) finish(t *testing.T) []string {
	t.Helper()
	p.in.Close()
	rest := p.drain()
	if code := p.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("serve exited %d (stderr %s)", code, p.errb.String())
	}
	return rest
}

// kill terminates serve without warning (TerminateProcess / SIGKILL) and
// returns the lines it had written.
func (p *serveProc) kill(t *testing.T) []string {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	return p.drain()
}

// A judge-style session: one request at a time, each reply read before the
// next request.
func TestCLIServeSession(t *testing.T) {
	path := newPath(t)
	p := startServe(t, path)
	steps := [][2]string{
		{"put a 1", "ACK 1"},
		{"get a", "VALUE 2 1"},
		{"put a hello world", "ACK 3"},
		{"get a", "VALUE 4 hello world"},
		{"del a", "ACK 5"},
		{"get a", "NOTFOUND 6"},
		{"t1 put b hex:00ff", "ACK t1"},
		{"t2 get b", "VALUE t2 hex:00ff"},
		{"compact", "ACK 9"},
	}
	for _, s := range steps {
		if got := p.request(t, s[0]); got != s[1] {
			t.Fatalf("%q: reply %q, want %q", s[0], got, s[1])
		}
	}
	// A batch: nothing is replied, and nothing is written, until commit.
	for _, l := range []string{"b begin", "b put x 1", "b put y 2", "b del b"} {
		p.send(l)
	}
	if got := p.request(t, "b commit"); got != "ACK b" {
		t.Fatalf("commit: %q", got)
	}
	if got := p.request(t, "get b"); got != "NOTFOUND 15" {
		t.Fatalf("after the batch: %q", got)
	}
	if got := p.request(t, "nonsense"); !strings.HasPrefix(got, "ERR 16 PARSE") {
		t.Fatalf("bad command: %q", got)
	}
	if got := p.request(t, "put "+strings.Repeat("k", 1025)+" v"); !strings.HasPrefix(got, "ERR 17 INVALID") {
		t.Fatalf("too-long key: %q", got)
	}
	if rest := p.finish(t); len(rest) != 0 {
		t.Fatalf("unexpected output at exit: %q", rest)
	}
	got := parseDump(t, kvdbOK(t, "dump", path))
	if !sameState(got, map[string]string{"x": "1", "y": "2"}) {
		t.Fatalf("after the session the database holds %q", got)
	}
}

// The judge's kill test: serve is killed without warning at random moments,
// several times, with operations in flight and a batch left open. After every
// kill: check says OK, and dump holds every acknowledged operation, no
// operation of the uncommitted batch, and no value that was never sent.
func TestCLIServeKilledRepeatedly(t *testing.T) {
	rounds := 6
	if testing.Short() {
		rounds = 2
	}
	path := newPath(t)
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)

	mustHave := map[string]string{} // acknowledged: must be present with this value
	maybe := map[string]string{}    // sent, not acknowledged: absent, or exactly this value
	never := map[string]bool{}      // in a batch that was never committed

	for r := 0; r < rounds; r++ {
		p := startServe(t, path)
		for i := 0; i < 10; i++ { // closed loop: wait for every ACK
			k, v := fmt.Sprintf("r%d/s%d", r, i), fmt.Sprintf("value %d.%d", r, i)
			if got := p.request(t, fmt.Sprintf("s.%d put %s %s", i, k, v)); got != fmt.Sprintf("ACK s.%d", i) {
				t.Fatalf("round %d: %q", r, got)
			}
			mustHave[k] = v
		}
		p.send("cb begin") // a committed batch
		for j := 0; j < 5; j++ {
			p.send(fmt.Sprintf("cb put r%d/cb%d batch %d", r, j, j))
		}
		if got := p.request(t, "cb commit"); got != "ACK cb" {
			t.Fatalf("round %d: batch commit %q", r, got)
		}
		for j := 0; j < 5; j++ {
			mustHave[fmt.Sprintf("r%d/cb%d", r, j)] = fmt.Sprintf("batch %d", j)
		}
		p.send("ob begin") // a batch that is never committed
		for j := 0; j < 5; j++ {
			k := fmt.Sprintf("r%d/ob%d", r, j)
			p.send(fmt.Sprintf("ob put %s uncommitted", k))
			never[k] = true
		}
		burst := map[string][2]string{} // a burst of puts, not waiting for replies
		for i := 0; i < 100; i++ {
			k, v := fmt.Sprintf("r%d/b%d", r, i), fmt.Sprintf("burst %d.%d", r, i)
			p.send(fmt.Sprintf("b.%d put %s %s", i, k, v))
			burst[fmt.Sprintf("b.%d", i)] = [2]string{k, v}
		}
		time.Sleep(time.Duration(rng.Intn(60)) * time.Millisecond)
		acks := 0
		for _, l := range p.kill(t) {
			id, ok := strings.CutPrefix(l, "ACK ")
			if kv, isBurst := burst[id]; ok && isBurst {
				mustHave[kv[0]] = kv[1]
				delete(burst, id)
				acks++
			}
		}
		for _, kv := range burst {
			maybe[kv[0]] = kv[1]
		}
		t.Logf("round %d: killed with %d of 100 burst puts acknowledged", r, acks)

		if out := kvdbOK(t, "check", path); !strings.HasPrefix(out, "OK ") {
			t.Fatalf("round %d: check printed %q", r, out)
		}
		got := parseDump(t, kvdbOK(t, "dump", path))
		for k, v := range mustHave {
			if got[k] != v {
				t.Fatalf("round %d: acknowledged %s = %q lost (dump has %q)", r, k, v, got[k])
			}
		}
		for k, v := range got {
			if never[k] {
				t.Fatalf("round %d: %s from an uncommitted batch is visible", r, k)
			}
			if w, ok := mustHave[k]; ok && w == v {
				continue
			}
			if w, ok := maybe[k]; ok && w == v {
				mustHave[k] = v // present now: from here on it must stay
				delete(maybe, k)
				continue
			}
			t.Fatalf("round %d: dump holds %s = %q, which was never sent", r, k, v)
		}
		for k := range maybe { // absent after this restart: absent for good
			if _, ok := got[k]; !ok {
				delete(maybe, k)
				never[k] = true
			}
		}
	}
}

// Several writers share one serve process: their operations run concurrently,
// but each writer's operations stay in its own order.
func TestCLIServeConcurrentWriters(t *testing.T) {
	path := newPath(t)
	p := startServe(t, path)
	const n = 150
	for i := 0; i < n; i++ { // interleaved, without waiting
		p.send(fmt.Sprintf("a.%d put a/k %d", i, i))
		p.send(fmt.Sprintf("b.%d put b/k %d", i, i))
		p.send(fmt.Sprintf("c.%d put c/%d x", i, i))
	}
	p.send("d.0 begin")
	p.send("d.0 put d/1 one")
	p.send("d.0 put d/2 two")
	p.send("d.0 commit")
	lines := p.finish(t)

	last := map[string]int{"a": -1, "b": -1, "c": -1}
	acks := 0
	for _, l := range lines {
		id, ok := strings.CutPrefix(l, "ACK ")
		if !ok {
			t.Fatalf("unexpected reply %q", l)
		}
		acks++
		w, num, _ := strings.Cut(id, ".")
		if w == "d" {
			continue
		}
		var i int
		fmt.Sscan(num, &i)
		if i != last[w]+1 {
			t.Fatalf("writer %s: ACK %d after %d: out of order", w, i, last[w])
		}
		last[w] = i
	}
	if acks != 3*n+1 {
		t.Fatalf("%d ACKs, want %d", acks, 3*n+1)
	}
	got := parseDump(t, kvdbOK(t, "dump", path))
	if got["a/k"] != fmt.Sprint(n-1) || got["b/k"] != fmt.Sprint(n-1) || got["d/1"] != "one" || got["d/2"] != "two" || len(got) != n+4 {
		t.Fatalf("final state wrong: a/k=%q b/k=%q, %d keys", got["a/k"], got["b/k"], len(got))
	}
}

// The file-size limit through serve: rejected writes get ERR ... DISKFULL and
// never an ACK, and survive a restart as absent.
func TestCLIServeDiskFull(t *testing.T) {
	path := newPath(t)
	p := startServe(t, path, "--max-bytes", "16384")
	acked, full := []string{}, 0
	for i := 0; i < 30; i++ {
		reply := p.request(t, fmt.Sprintf("p%d put k%02d %s", i, i, strings.Repeat("v", 900)))
		switch {
		case reply == fmt.Sprintf("ACK p%d", i):
			acked = append(acked, fmt.Sprintf("k%02d", i))
		case strings.HasPrefix(reply, fmt.Sprintf("ERR p%d DISKFULL ", i)):
			full++
		default:
			t.Fatalf("reply %q", reply)
		}
	}
	p.kill(t)
	if len(acked) == 0 || full == 0 {
		t.Fatalf("%d acked, %d rejected: limit not reached", len(acked), full)
	}
	got := parseDump(t, kvdbOK(t, "dump", path))
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if fmt.Sprint(keys) != fmt.Sprint(acked) {
		t.Fatalf("after restart the database holds %v, acknowledged were %v", keys, acked)
	}
	underCap(t, path, 16384)
}

// The binary-safe path end to end: a 1 MiB value of every byte value, through
// serve, read back with get and dump.
func TestCLIServeLargeBinaryValue(t *testing.T) {
	path := newPath(t)
	val := make([]byte, storage.MaxValueLen)
	for i := range val {
		val[i] = byte(i * 7)
	}
	p := startServe(t, path)
	if got := p.request(t, "big put hex:00ff hex:"+hex.EncodeToString(val)); got != "ACK big" {
		t.Fatalf("1 MiB put: %q", got)
	}
	if got := p.request(t, "g get hex:00ff"); got != "VALUE g hex:"+hex.EncodeToString(val) {
		t.Fatalf("1 MiB get through serve came back different (%d chars)", len(got))
	}
	p.finish(t)
	if got := strings.TrimSpace(kvdbOK(t, "get", path, "hex:00ff")); got != "hex:"+hex.EncodeToString(val) {
		t.Fatal("1 MiB value read back by get differs")
	}
	db, err := engine.Open(path, fast)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := db.Get([]byte{0, 0xff}); !bytes.Equal(v, val) {
		t.Fatal("stored bytes differ from what was sent")
	}
}
