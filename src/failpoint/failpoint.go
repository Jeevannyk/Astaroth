// Package failpoint lets tests kill the process at a named point in the write
// or recovery path, to reproduce "the process died exactly here".
//
// It is inert unless the environment variable ASTAROTH_FAILPOINT is set, as
// "name" (fire on the first hit) or "name:N" (fire on the Nth hit). When the
// point fires the process exits immediately with ExitCode: no deferred calls,
// no Close, no flushing, just as if it had been killed. The OS then drops the
// file handles and locks, and the page cache keeps whatever was written.
//
// A test that arms a failpoint must check for ExitCode; otherwise a point that
// never fired would make the test pass without testing anything.
package failpoint

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// ExitCode is the exit status of a process killed by a failpoint.
const ExitCode = 86

var (
	enabled bool
	armed   string
	nth     int64 = 1
	hits    atomic.Int64
)

func init() {
	v := os.Getenv("ASTAROTH_FAILPOINT")
	if v == "" {
		return
	}
	armed = v
	if i := strings.LastIndexByte(v, ':'); i >= 0 {
		if n, err := strconv.ParseInt(v[i+1:], 10, 64); err == nil && n > 0 {
			armed, nth = v[:i], n
		}
	}
	enabled = true
}

// Fire reports whether the named point triggers now. It is always false unless
// the environment arms this exact name, and then true only on the Nth hit.
func Fire(name string) bool {
	if !enabled || name != armed {
		return false
	}
	return hits.Add(1) == nth
}

// Exit ends the process at once, as a kill would.
func Exit() {
	os.Exit(ExitCode)
}

// Hit exits the process if the named point triggers now.
func Hit(name string) {
	if enabled && Fire(name) {
		Exit()
	}
}
