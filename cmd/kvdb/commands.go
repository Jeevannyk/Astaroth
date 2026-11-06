package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"astaroth/src/engine"
	"astaroth/src/recovery"
	"astaroth/src/token"
)

// maxLine bounds one line of a batch file or of serve input: a 1 MiB value
// written in hex is 2 MiB, plus the key.
const maxLine = 8 << 20

func cmdOpen(c *cmdContext) int {
	start := time.Now()
	db, err := c.openDB()
	elapsed := time.Since(start)
	if err != nil {
		return c.fail(err)
	}
	n, _ := db.Len()
	info := db.Recovery()
	fmt.Fprintf(c.stdout, "keys=%d recovery_ms=%.3f frames=%d torn_tail_bytes=%d\n",
		n, float64(elapsed.Microseconds())/1000, info.Frames, info.TruncatedBytes)
	return c.closeDB(db, exitOK)
}

func cmdPut(c *cmdContext) int {
	key, value, err := decodeTwo(c.args[1], c.args[2])
	if err != nil {
		return c.fail(err)
	}
	db, err := c.openDB()
	if err != nil {
		return c.fail(err)
	}
	if err := db.Put(key, value); err != nil {
		return c.closeDB(db, c.fail(err))
	}
	fmt.Fprintln(c.stdout, "OK")
	return c.closeDB(db, exitOK)
}

func cmdGet(c *cmdContext) int {
	key, err := token.Decode(c.args[1])
	if err != nil {
		return c.fail(err)
	}
	if err := c.mustExist(); err != nil {
		return c.fail(err)
	}
	db, err := c.openDB()
	if err != nil {
		return c.fail(err)
	}
	v, err := db.Get(key)
	switch {
	case errors.Is(err, engine.ErrNotFound):
		fmt.Fprintln(c.stdout, token.NotFound)
	case err != nil:
		return c.closeDB(db, c.fail(err))
	default:
		fmt.Fprintln(c.stdout, token.Value(v))
	}
	return c.closeDB(db, exitOK)
}

func cmdDel(c *cmdContext) int {
	key, err := token.Decode(c.args[1])
	if err != nil {
		return c.fail(err)
	}
	db, err := c.openDB()
	if err != nil {
		return c.fail(err)
	}
	if err := db.Delete(key); err != nil {
		return c.closeDB(db, c.fail(err))
	}
	fmt.Fprintln(c.stdout, "OK")
	return c.closeDB(db, exitOK)
}

// cmdBatch reads the whole file first. Any line that does not parse rejects
// the batch before the database is even opened, so nothing is written.
func cmdBatch(c *cmdContext) int {
	var in io.Reader = c.stdin
	name := "standard input"
	if c.args[1] != "-" {
		f, err := os.Open(c.args[1])
		if err != nil {
			return c.fail(err)
		}
		defer f.Close()
		in, name = f, c.args[1]
	}
	var ops []engine.Operation
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for n := 1; sc.Scan(); n++ {
		line, ok := cleanLine(sc.Text(), n)
		if !ok {
			continue
		}
		cmd, rest, _ := strings.Cut(line, " ")
		op, err := parseOp(cmd, rest)
		if err != nil {
			return c.fail(fmt.Errorf("%s line %d: %v (nothing was written)", name, n, err))
		}
		ops = append(ops, op)
	}
	if err := sc.Err(); err != nil {
		return c.fail(fmt.Errorf("reading %s: %v (nothing was written)", name, err))
	}

	db, err := c.openDB()
	if err != nil {
		return c.fail(err)
	}
	if err := db.Batch(ops); err != nil {
		return c.closeDB(db, c.fail(fmt.Errorf("batch rejected, nothing was written: %w", err)))
	}
	fmt.Fprintf(c.stdout, "OK %d operations\n", len(ops))
	return c.closeDB(db, exitOK)
}

func cmdDump(c *cmdContext) int {
	if err := c.mustExist(); err != nil {
		return c.fail(err)
	}
	db, err := c.openDB()
	if err != nil {
		return c.fail(err)
	}
	w := bufio.NewWriterSize(c.stdout, 1<<20)
	err = db.Dump(func(k, v []byte) error {
		_, err := fmt.Fprintf(w, "%s %s\n", token.Key(k), token.Value(v))
		return err
	})
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		return c.closeDB(db, c.fail(err))
	}
	return c.closeDB(db, exitOK)
}

// cmdCheck prints OK and what it found, or CORRUPT and the reason. It never
// changes the file: a torn tail is reported and left for the next open.
func cmdCheck(c *cmdContext) int {
	path := c.args[0]
	rep, err := engine.Check(path, engine.Options{})
	if err != nil {
		if errors.Is(err, recovery.ErrCorrupt) || errors.Is(err, recovery.ErrNotOurFile) {
			fmt.Fprintf(c.stdout, "CORRUPT: %v\n", err)
			return exitError
		}
		return c.fail(err)
	}
	line := fmt.Sprintf("OK keys=%d frames=%d bytes=%d", rep.Keys, rep.Frames, rep.Size)
	if !rep.HeaderIncomplete {
		line += fmt.Sprintf(" generation=%d", rep.Header.Generation)
	}
	var notes []string
	if rep.HeaderIncomplete {
		notes = append(notes, "header incomplete: an empty database, the next open writes the header")
	}
	if rep.TornTail > 0 {
		notes = append(notes, fmt.Sprintf("torn tail: %d bytes at offset %d, removed by the next open", rep.TornTail, rep.End))
	}
	if rep.AbandonedTemp {
		notes = append(notes, fmt.Sprintf("abandoned compaction file %s.compact.tmp, removed by the next open", path))
	}
	if rep.InterruptedReplace {
		notes = append(notes, fmt.Sprintf("data file missing: the next open renames %s.compact.tmp into place; this report describes that file", path))
	}
	if len(notes) > 0 {
		line += " (" + strings.Join(notes, "; ") + ")"
	}
	fmt.Fprintln(c.stdout, line)
	return exitOK
}

func decodeTwo(k, v string) ([]byte, []byte, error) {
	key, err := token.Decode(k)
	if err != nil {
		return nil, nil, err
	}
	value, err := token.Decode(v)
	if err != nil {
		return nil, nil, err
	}
	return key, value, nil
}
