// Command kvdb is the command-line wrapper around the engine. README.md
// ("Run") documents every command, the text encoding of keys and values, and
// the serve protocol.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"astaroth/src/engine"
	"astaroth/src/lock"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// Exit codes.
const (
	exitOK    = 0
	exitError = 1 // the operation failed: I/O error, disk full, corrupt file, ...
	exitUsage = 2 // wrong command line
)

const usage = `usage:
  kvdb open  <path> [--max-bytes N]   open or create, run recovery, print key count and recovery time
  kvdb put   <path> <key> <value>     store value under key
  kvdb get   <path> <key>             print the value, or NOTFOUND
  kvdb del   <path> <key>             delete key (deleting a missing key is not an error)
  kvdb batch <path> <file>            apply a file of "put k v" and "del k" lines as one atomic batch
                                      (<file> "-" reads standard input)
  kvdb dump  <path>                   print every key and value, sorted by key
  kvdb check <path>                   verify the file without changing it; print OK or the problem
  kvdb serve <path>                   read operations from standard input, print ACK <id> after each
                                      once it is durable (protocol in README.md)

Every command accepts --max-bytes N to cap the file size (0, the default, is unlimited).
A key or value written as hex:<hex digits> stands for those bytes; anything else is literal.
`

type command struct {
	args []string // names of the positional arguments
	run  func(c *cmdContext) int
}

var commands = map[string]command{
	"open":  {[]string{"<path>"}, cmdOpen},
	"put":   {[]string{"<path>", "<key>", "<value>"}, cmdPut},
	"get":   {[]string{"<path>", "<key>"}, cmdGet},
	"del":   {[]string{"<path>", "<key>"}, cmdDel},
	"batch": {[]string{"<path>", "<file>"}, cmdBatch},
	"dump":  {[]string{"<path>"}, cmdDump},
	"check": {[]string{"<path>"}, cmdCheck},
	"serve": {[]string{"<path>"}, cmdServe},
}

type cmdContext struct {
	name     string
	args     []string // positional arguments
	maxBytes int64
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
}

// run executes one command line and returns the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "kvdb: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
	pos, maxBytes, err := parseArgs(args[1:])
	if err != nil {
		fmt.Fprintf(stderr, "kvdb %s: %v\n", args[0], err)
		return exitUsage
	}
	if len(pos) != len(cmd.args) {
		fmt.Fprintf(stderr, "usage: kvdb %s %s [--max-bytes N]\n", args[0], strings.Join(cmd.args, " "))
		return exitUsage
	}
	return cmd.run(&cmdContext{name: args[0], args: pos, maxBytes: maxBytes, stdin: stdin, stdout: stdout, stderr: stderr})
}

// parseArgs separates positional arguments from the one option, --max-bytes,
// which may appear anywhere ("--max-bytes N" or "--max-bytes=N"). Everything
// after "--" is positional, and so is every other argument, including ones
// that start with "-": a value such as -5 is never mistaken for an option.
func parseArgs(args []string) (pos []string, maxBytes int64, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		switch {
		case a == "--":
			return append(pos, args[i+1:]...), maxBytes, nil
		case a == "--max-bytes" || a == "-max-bytes":
			if i+1 == len(args) {
				return nil, 0, fmt.Errorf("--max-bytes needs a number")
			}
			i++
			val = args[i]
		case strings.HasPrefix(a, "--max-bytes=") || strings.HasPrefix(a, "-max-bytes="):
			_, val, _ = strings.Cut(a, "=")
		default:
			pos = append(pos, a)
			continue
		}
		if maxBytes, err = strconv.ParseInt(val, 10, 64); err != nil {
			return nil, 0, fmt.Errorf("--max-bytes %q is not a number", val)
		}
	}
	return pos, maxBytes, nil
}

// openDB opens the database named by the first argument.
func (c *cmdContext) openDB() (*engine.DB, error) {
	return engine.Open(c.args[0], engine.Options{MaxBytes: c.maxBytes})
}

// mustExist fails for commands that only read, so that they never create a
// database as a side effect. A missing data file next to a compaction
// temporary file still counts as existing: open completes the compaction.
func (c *cmdContext) mustExist() error {
	path := c.args[0]
	for _, p := range []string{path, path + ".compact.tmp"} {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no database at %s", path)
}

// fail reports err on stderr and returns the exit code for it.
func (c *cmdContext) fail(err error) int {
	if errors.Is(err, lock.ErrLocked) {
		fmt.Fprintf(c.stderr, "kvdb: %s is in use by another process (its lock file %s.lock is held)\n", c.args[0], c.args[0])
		return exitError
	}
	fmt.Fprintf(c.stderr, "kvdb: %v\n", err)
	return exitError
}

// closeDB closes db and reports a close error.
func (c *cmdContext) closeDB(db *engine.DB, code int) int {
	if err := db.Close(); err != nil && code == exitOK {
		return c.fail(fmt.Errorf("close: %w", err))
	}
	return code
}
