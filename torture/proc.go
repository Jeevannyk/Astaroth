package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"astaroth/src/token"
)

// serveProc is one `kvdb serve` child process.
type serveProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	inMu    sync.Mutex // requests from concurrent writers must not interleave
	ready   chan readyInfo
	done    chan struct{} // closed when the child's stdout reaches EOF
	stderr  bytes.Buffer
	started time.Time
	lines   int // non-READY lines seen (for the report)
}

type readyInfo struct {
	at        time.Duration // from process start to the READY line
	keys      int
	recoveryM float64 // recovery_ms reported by kvdb
	tornTail  int64
}

var readyRe = regexp.MustCompile(`^READY keys=(\d+) recovery_ms=([\d.]+) frames=(\d+) torn_tail_bytes=(\d+)$`)

// startServe starts `kvdb serve db`. route receives every output line other
// than READY, from the reader goroutine.
func startServe(bin, db string, maxBytes int64, route func(string)) (*serveProc, error) {
	p := &serveProc{ready: make(chan readyInfo, 1), done: make(chan struct{})}
	p.cmd = exec.Command(bin, "serve", db, "--max-bytes", strconv.FormatInt(maxBytes, 10))
	var err error
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p.cmd.Stderr = &p.stderr
	p.started = time.Now()
	if err := p.cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	go func() {
		defer close(p.done)
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				// A partial last line (cut off by the kill) is not a reply.
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if m := readyRe.FindStringSubmatch(line); m != nil {
				info := readyInfo{at: time.Since(p.started)}
				info.keys, _ = strconv.Atoi(m[1])
				info.recoveryM, _ = strconv.ParseFloat(m[2], 64)
				info.tornTail, _ = strconv.ParseInt(m[4], 10, 64)
				p.ready <- info
				continue
			}
			p.lines++
			if route != nil {
				route(line)
			}
		}
	}()
	return p, nil
}

func (p *serveProc) waitReady(timeout time.Duration) (readyInfo, error) {
	select {
	case info := <-p.ready:
		return info, nil
	case <-p.done:
		p.cmd.Wait()
		return readyInfo{}, fmt.Errorf("serve exited before READY: %s", strings.TrimSpace(p.stderr.String()))
	case <-time.After(timeout):
		return readyInfo{}, fmt.Errorf("serve did not print READY within %v", timeout)
	}
}

// send writes one complete request (one or more lines) atomically.
func (p *serveProc) send(req string) error {
	p.inMu.Lock()
	defer p.inMu.Unlock()
	_, err := io.WriteString(p.stdin, req)
	return err
}

// kill terminates the child with SIGKILL (TerminateProcess on Windows): no
// signal handler, no cleanup, no flushing. It returns once every line the
// child wrote before dying has been read.
func (p *serveProc) kill() {
	p.cmd.Process.Kill()
	<-p.done
	p.cmd.Wait()
}

// sawReady reports whether the READY line arrived (without waiting).
func (p *serveProc) sawReady() bool {
	select {
	case info := <-p.ready:
		p.ready <- info
		return true
	default:
		return false
	}
}

// runKvdb runs a one-shot kvdb command and returns its output and exit code.
func runKvdb(bin string, args ...string) (stdout, stderr string, code int, err error) {
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), errb.String(), ee.ExitCode(), nil
	}
	if err != nil {
		return "", "", -1, err
	}
	return out.String(), errb.String(), 0, nil
}

type openInfo struct {
	keys      int
	recoveryM float64
	frames    int
	tornTail  int64
}

var openRe = regexp.MustCompile(`^keys=(\d+) recovery_ms=([\d.]+) frames=(\d+) torn_tail_bytes=(\d+)\n$`)

func parseOpen(out string) (openInfo, bool) {
	m := openRe.FindStringSubmatch(out)
	if m == nil {
		return openInfo{}, false
	}
	var o openInfo
	o.keys, _ = strconv.Atoi(m[1])
	o.recoveryM, _ = strconv.ParseFloat(m[2], 64)
	o.frames, _ = strconv.Atoi(m[3])
	o.tornTail, _ = strconv.ParseInt(m[4], 10, 64)
	return o, true
}

var checkBytesRe = regexp.MustCompile(` bytes=(\d+)`)

// parseDump decodes `kvdb dump` output into a map.
func parseDump(out string) (map[string][]byte, error) {
	m := map[string][]byte{}
	if out == "" {
		return m, nil
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("dump line %d has no value: %q", i+1, line)
		}
		kb, err := token.Decode(k)
		if err != nil {
			return nil, fmt.Errorf("dump line %d: %v", i+1, err)
		}
		vb, err := token.Decode(v)
		if err != nil {
			return nil, fmt.Errorf("dump line %d: %v", i+1, err)
		}
		if _, dup := m[string(kb)]; dup {
			return nil, fmt.Errorf("dump lists key %q twice", kb)
		}
		m[string(kb)] = vb
	}
	return m, nil
}
