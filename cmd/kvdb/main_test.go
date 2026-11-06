package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"astaroth/src/engine"
	"astaroth/tests/testdb"
)

// In-process tests of the command implementations. End-to-end tests that run
// the built kvdb binary are in tests/cli_test.go.

func runCLI(args ...string) (code int, stdout, stderr string) {
	return runCLIIn("", args...)
}

func runCLIIn(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

var openLine = regexp.MustCompile(`^keys=\d+ recovery_ms=\d+\.\d{3} frames=\d+ torn_tail_bytes=\d+\n$`)

// The main database is the permanent data/astaroth.db, which keeps its keys
// between runs, so the key count is only checked to be a number.
func TestOpenPermanentFile(t *testing.T) {
	path := testdb.Path(t)
	for i := 0; i < 2; i++ {
		code, out, errOut := runCLI("open", path)
		if code != 0 {
			t.Fatalf("run %d: exit %d, stderr %q", i, code, errOut)
		}
		if !openLine.MatchString(out) {
			t.Fatalf("run %d: stdout = %q", i, out)
		}
	}
	if st, err := os.Stat(path); err != nil || st.Size() < 32 {
		t.Fatalf("data file: stat = %v, err = %v; want at least 32 bytes", st, err)
	}
}

func TestOpenAcceptsMaxBytesAfterPath(t *testing.T) {
	path := testdb.Path(t)
	if code, _, errOut := runCLI("open", path, "--max-bytes", "50000000"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestOpenAcceptsMaxBytesBeforePath(t *testing.T) {
	path := testdb.Path(t)
	if code, _, errOut := runCLI("open", "--max-bytes=100", path); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestUsageErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	cases := map[string][]string{
		"no args":           {},
		"unknown":           {"frobnicate"},
		"open no path":      {"open"},
		"open extra arg":    {"open", path, "extra"},
		"unknown option":    {"open", path, "--nope"},
		"bad max-bytes":     {"open", path, "--max-bytes", "abc"},
		"max-bytes no num":  {"open", path, "--max-bytes"},
		"put missing value": {"put", path, "k"},
		"get missing key":   {"get", path},
		"del extra arg":     {"del", path, "k", "v"},
		"batch no file":     {"batch", path},
		"dump extra":        {"dump", path, "x"},
		"check no path":     {"check"},
		"serve extra":       {"serve", path, "x"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if code, _, errOut := runCLI(args...); code != 2 || errOut == "" {
				t.Fatalf("exit = %d, stderr %q; want 2 with a message", code, errOut)
			}
		})
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a usage error created a database file")
	}
	if code, out, _ := runCLI("help"); code != 0 || !strings.Contains(out, "kvdb serve") {
		t.Fatalf("help: exit %d, %q", code, out)
	}
}

func TestRuntimeErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("negative max-bytes", func(t *testing.T) {
		if code, _, _ := runCLI("open", filepath.Join(dir, "a"), "--max-bytes", "-1"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		if code, _, errOut := runCLI("open", filepath.Join(dir, "nope", "db")); code != 1 || errOut == "" {
			t.Fatalf("exit = %d, stderr %q; want 1 with a message", code, errOut)
		}
	})
	t.Run("foreign file", func(t *testing.T) {
		path := filepath.Join(dir, "foreign")
		if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 64), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runCLI("open", path); code != 1 || errOut == "" {
			t.Fatalf("exit = %d, stderr %q; want 1 with a message", code, errOut)
		}
	})
	t.Run("reading commands do not create a database", func(t *testing.T) {
		path := filepath.Join(dir, "absent")
		for _, args := range [][]string{{"get", path, "k"}, {"dump", path}, {"check", path}} {
			if code, _, errOut := runCLI(args...); code != 1 || !strings.Contains(errOut, "no database") {
				t.Fatalf("%v: exit %d, stderr %q", args, code, errOut)
			}
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatal("a reading command created the database")
		}
	})
	t.Run("bad hex", func(t *testing.T) {
		if code, _, errOut := runCLI("put", filepath.Join(dir, "h"), "hex:zz", "v"); code != 1 || !strings.Contains(errOut, "hex") {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
	})
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		in   []string
		pos  []string
		max  int64
		fail bool
	}{
		{[]string{"db"}, []string{"db"}, 0, false},
		{[]string{"db", "--max-bytes", "10"}, []string{"db"}, 10, false},
		{[]string{"--max-bytes=10", "db"}, []string{"db"}, 10, false},
		{[]string{"-max-bytes", "7", "db", "k"}, []string{"db", "k"}, 7, false},
		{[]string{"db", "k", "-5"}, []string{"db", "k", "-5"}, 0, false}, // a value, not an option
		{[]string{"db", "--", "--max-bytes", "9"}, []string{"db", "--max-bytes", "9"}, 0, false},
		{[]string{"db", "--max-bytes"}, nil, 0, true},
		{[]string{"db", "--max-bytes", "x"}, nil, 0, true},
	}
	for _, c := range cases {
		pos, max, err := parseArgs(c.in)
		if c.fail != (err != nil) || fmt.Sprint(pos) != fmt.Sprint(c.pos) || max != c.max {
			t.Fatalf("parseArgs(%q) = %q, %d, %v", c.in, pos, max, err)
		}
	}
}

func TestParseOp(t *testing.T) {
	ok := []struct {
		cmd, rest string
		want      engine.Operation
	}{
		{"put", "k v", engine.Operation{Kind: engine.OpPut, Key: []byte("k"), Value: []byte("v")}},
		{"put", "k hello world", engine.Operation{Kind: engine.OpPut, Key: []byte("k"), Value: []byte("hello world")}},
		{"put", "k ", engine.Operation{Kind: engine.OpPut, Key: []byte("k"), Value: []byte{}}},
		{"put", "hex:00 hex:", engine.Operation{Kind: engine.OpPut, Key: []byte{0}, Value: []byte{}}},
		{"del", "k", engine.Operation{Kind: engine.OpDelete, Key: []byte("k")}},
	}
	for _, c := range ok {
		got, err := parseOp(c.cmd, c.rest)
		if err != nil || got.Kind != c.want.Kind || !bytes.Equal(got.Key, c.want.Key) || !bytes.Equal(got.Value, c.want.Value) {
			t.Fatalf("parseOp(%q, %q) = %+v, %v", c.cmd, c.rest, got, err)
		}
	}
	for _, bad := range [][2]string{{"put", "k"}, {"put", ""}, {"put", " v"}, {"del", ""}, {"del", "a b"}, {"put", "hex:0 v"}, {"get", "k"}} {
		if _, err := parseOp(bad[0], bad[1]); err == nil {
			t.Fatalf("parseOp(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

// serveSession runs serve on path with the given input lines and returns its
// replies grouped by id, plus the READY line.
func serveSession(t *testing.T, path string, lines []string, extra ...string) (ready string, replies map[string][]string, code int) {
	t.Helper()
	args := append([]string{"serve", path}, extra...)
	code, out, errOut := runCLIIn(strings.Join(lines, "\n")+"\n", args...)
	if errOut != "" {
		t.Logf("stderr: %s", errOut)
	}
	replies = map[string][]string{}
	for i, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if i == 0 {
			ready = l
			continue
		}
		f := strings.SplitN(l, " ", 3)
		if len(f) < 2 {
			t.Fatalf("malformed reply %q", l)
		}
		replies[f[1]] = append(replies[f[1]], l)
	}
	return ready, replies, code
}

func TestServeProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	lines := []string{
		"put a 1",                 // 1
		"get a",                   // 2
		"get missing",             // 3
		"del a",                   // 4
		"get a",                   // 5
		"w1.1 put sp hello world", //
		"w1.2 get sp",             //
		"b1 begin",                //
		"b1 put x 1",              //
		"b1 put y 2",              //
		"b1 del x",                //
		"b1 commit",               //
		"b2 begin",                //
		"b2 put z 1",              //
		"b2 abort",                //
		"b3 begin",                //
		"b3 put",                  // bad line inside a batch
		"b3 put ok 1",             //
		"b3 commit",               //
		"put hex:00ff hex:",       // 20
		"get hex:00ff",            // 21
		"frobnicate",              // 22
		"x commit",                //
		"put " + strings.Repeat("k", 1025) + " v", // 24: key too long
		"",            // 25: blank, ignored
		"# a comment", // 26: ignored
		"compact",     // 27
		"begin",       // 28: a batch for lines without ids
		"put m 1",     // 29
		"put n 2",     // 30
		"commit",      // 31
		"get m",       // 32
		"get y",       // 33
		"get z",       // 34
		"get ok",      // 35
	}
	ready, r, code := serveSession(t, path, lines)
	if code != 0 {
		t.Fatalf("serve exit %d", code)
	}
	if !regexp.MustCompile(`^READY keys=0 recovery_ms=\d+\.\d{3} frames=0 torn_tail_bytes=0$`).MatchString(ready) {
		t.Fatalf("first line %q", ready)
	}
	want := map[string]string{
		"1": "ACK 1", "2": "VALUE 2 1", "3": "NOTFOUND 3", "4": "ACK 4", "5": "NOTFOUND 5",
		"w1.1": "ACK w1.1", "w1.2": "VALUE w1.2 hello world",
		"b1": "ACK b1", "b2": "ABORTED b2",
		"20": "ACK 20", "21": "VALUE 21 hex:",
		"27": "ACK 27", "28": "ACK 28", "32": "VALUE 32 1", "33": "VALUE 33 2",
		"34": "NOTFOUND 34", "35": "NOTFOUND 35",
	}
	for id, w := range want {
		if got := r[id]; len(got) != 1 || got[0] != w {
			t.Fatalf("id %s: replies %q, want [%q]", id, got, w)
		}
	}
	errs := map[string]string{"22": "ERR 22 PARSE", "x": "ERR x PARSE", "24": "ERR 24 INVALID"}
	for id, prefix := range errs {
		if got := r[id]; len(got) != 1 || !strings.HasPrefix(got[0], prefix) {
			t.Fatalf("id %s: replies %q, want one starting %q", id, got, prefix)
		}
	}
	// b3: the bad line is reported, and the commit is refused: never an ACK.
	if got := r["b3"]; len(got) != 2 || !strings.HasPrefix(got[0], "ERR b3 PARSE") || !strings.HasPrefix(got[1], "ERR b3 INVALID") {
		t.Fatalf("b3 replies %q", got)
	}
	for id, rs := range r {
		for _, l := range rs {
			if strings.HasPrefix(l, "ACK") && strings.Contains(strings.Join(rs, "|"), "ERR") {
				t.Fatalf("id %s got both an ACK and an ERR: %q", id, rs)
			}
		}
	}
	if len(r) != len(want)+len(errs)+1 { // +1 for b3
		t.Fatalf("unexpected replies: %v", r)
	}

	// The database holds exactly what was acknowledged.
	db, err := engine.Open(path, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for k, v := range map[string]string{"sp": "hello world", "y": "2", "m": "1", "n": "2", "\x00\xff": ""} {
		if got, err := db.Get([]byte(k)); err != nil || string(got) != v {
			t.Fatalf("%q = %q, %v; want %q", k, got, err, v)
		}
	}
	for _, k := range []string{"a", "x", "z", "ok"} {
		if _, err := db.Get([]byte(k)); err == nil {
			t.Fatalf("%q should not exist", k)
		}
	}
}

// Windows PowerShell puts a UTF-8 byte order mark in front of text it pipes
// into a program, and Notepad at the start of a file. Neither may break the
// first command.
func TestByteOrderMarkIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	_, r, code := serveSession(t, path, []string{"\ufeffput a 1", "get a"})
	if code != 0 || len(r["1"]) != 1 || r["1"][0] != "ACK 1" || r["2"][0] != "VALUE 2 1" {
		t.Fatalf("serve with a BOM: exit %d, replies %v", code, r)
	}
	file := filepath.Join(t.TempDir(), "ops.txt")
	if err := os.WriteFile(file, []byte("\ufeffput b 2\r\nput c 3\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runCLI("batch", path, file); code != 0 || out != "OK 2 operations\n" {
		t.Fatalf("batch file with a BOM: exit %d, %q %q", code, out, errOut)
	}
	if _, out, _ := runCLI("get", path, "b"); out != "2\n" {
		t.Fatalf("b = %q", out)
	}
}

// A write that fails is never acknowledged.
func TestServeNeverAcknowledgesAFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	var lines []string
	value := strings.Repeat("v", 1000)
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf("p%d put key%d %s", i, i, value))
	}
	_, r, code := serveSession(t, path, lines, "--max-bytes", "16384")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	acked, full := 0, 0
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("p%d", i)
		got := r[id]
		if len(got) != 1 {
			t.Fatalf("%s: replies %q", id, got)
		}
		switch {
		case got[0] == "ACK "+id:
			acked++
		case strings.HasPrefix(got[0], "ERR "+id+" DISKFULL "):
			full++
		default:
			t.Fatalf("%s: unexpected reply %q", id, got[0])
		}
	}
	if acked == 0 || full == 0 {
		t.Fatalf("acked %d, full %d: the limit was not reached", acked, full)
	}
	db, err := engine.Open(path, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n, _ := db.Len(); n != acked {
		t.Fatalf("database holds %d keys, %d were acknowledged", n, acked)
	}
}
