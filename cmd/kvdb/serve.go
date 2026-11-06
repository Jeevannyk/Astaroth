package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"astaroth/src/engine"
	"astaroth/src/storage"
	"astaroth/src/token"
)

// The serve protocol. One request per line on standard input:
//
//	[<id>] put <key> <value>   ->  ACK <id>
//	[<id>] del <key>           ->  ACK <id>
//	[<id>] get <key>           ->  VALUE <id> <value>   or   NOTFOUND <id>
//	[<id>] begin               ->  (no reply) opens a batch for <id>
//	[<id>] put / del ...       ->  (no reply) added to <id>'s open batch
//	[<id>] commit              ->  ACK <id> once the whole batch is durable
//	[<id>] abort               ->  ABORTED <id>
//	[<id>] compact             ->  ACK <id> once compaction is done
//
// A failure is reported as "ERR <id> <CODE> <message>", CODE being one of
// PARSE, INVALID, DISKFULL, FAILED, IO, and never as ACK. ACK is written only
// after the operation's fsync has returned, so every ACK is a promise.
//
// <id> is optional and must not be one of the command words. A line without
// one is given its line number as id. Lines whose ids share the part before
// the first "." (the writer: "w1" for "w1.17") are executed one after the
// other in input order; different writers are executed concurrently, so
// several clients can share one serve process as concurrent writers. Ids with
// no "." and lines without an id all belong to one writer, in input order.
//
// The first line written is "READY keys=... recovery_ms=..." once recovery is
// done. At end of input serve finishes the queued operations, closes the
// database and exits; open batches that were never committed are discarded.

var serveCommands = map[string]bool{
	"put": true, "del": true, "get": true, "begin": true, "commit": true, "abort": true, "compact": true,
}

type openBatch struct {
	id    string
	ops   []engine.Operation
	bytes int
	bad   error // a line of the batch was invalid: the commit will fail
}

type server struct {
	db      *engine.DB
	outMu   sync.Mutex
	out     io.Writer
	queues  map[string]chan func()
	wg      sync.WaitGroup
	batches map[string]*openBatch // keyed by explicit id, or "" for lines without one
}

func cmdServe(c *cmdContext) int {
	start := time.Now()
	db, err := c.openDB()
	if err != nil {
		return c.fail(err)
	}
	s := &server{db: db, out: c.stdout, queues: map[string]chan func(){}, batches: map[string]*openBatch{}}
	n, _ := db.Len()
	info := db.Recovery()
	s.say("READY keys=%d recovery_ms=%.3f frames=%d torn_tail_bytes=%d",
		n, float64(time.Since(start).Microseconds())/1000, info.Frames, info.TruncatedBytes)

	sc := bufio.NewScanner(c.stdin)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for lineNo := 1; sc.Scan(); lineNo++ {
		s.handle(sc.Text(), lineNo)
	}
	code := exitOK
	if err := sc.Err(); err != nil {
		s.say("ERR - PARSE reading input: %v", err)
		code = exitError
	}
	for _, q := range s.queues {
		close(q)
	}
	s.wg.Wait()
	return c.closeDB(db, code)
}

// say writes one response line. Lines from concurrent writers never mix: each
// is written whole, under the lock.
func (s *server) say(format string, args ...any) {
	line := fmt.Sprintf(format, args...) + "\n"
	s.outMu.Lock()
	io.WriteString(s.out, line)
	s.outMu.Unlock()
}

func (s *server) handle(raw string, lineNo int) {
	line, ok := cleanLine(raw, lineNo)
	if !ok {
		return
	}
	first, rest, _ := strings.Cut(line, " ")
	id, batchKey, cmd := strconv.Itoa(lineNo), "", first
	if !serveCommands[first] {
		// The first word is an id only if a command follows it.
		second, rest2, _ := strings.Cut(rest, " ")
		if !serveCommands[second] {
			s.say("ERR %s PARSE unknown command %q (want [<id>] put, del, get, begin, commit, abort or compact)", id, first)
			return
		}
		id, batchKey, cmd, rest = first, first, second, rest2
	}

	b := s.batches[batchKey]
	switch cmd {
	case "begin":
		if b != nil {
			s.say("ERR %s PARSE a batch is already open for this id", id)
			return
		}
		s.batches[batchKey] = &openBatch{id: id}
	case "abort":
		if b == nil {
			s.say("ERR %s PARSE no open batch to abort", id)
			return
		}
		delete(s.batches, batchKey)
		s.say("ABORTED %s", b.id)
	case "commit":
		if b == nil {
			s.say("ERR %s PARSE no open batch to commit (start one with begin)", id)
			return
		}
		delete(s.batches, batchKey)
		if b.bad != nil {
			s.say("ERR %s INVALID batch not written: %v", b.id, b.bad)
			return
		}
		s.enqueue(b.id, func() { s.reply(b.id, s.db.Batch(b.ops)) })
	case "put", "del":
		op, err := parseOp(cmd, rest)
		if b != nil {
			b.add(op, err)
			if err != nil {
				s.say("ERR %s PARSE %v (the batch will be rejected at commit)", id, err)
			}
			return
		}
		if err != nil {
			s.say("ERR %s PARSE %v", id, err)
			return
		}
		s.enqueue(id, func() {
			if op.Kind == engine.OpPut {
				s.reply(id, s.db.Put(op.Key, op.Value))
			} else {
				s.reply(id, s.db.Delete(op.Key))
			}
		})
	case "get":
		key, err := token.Decode(rest)
		if err != nil || rest == "" || strings.Contains(rest, " ") {
			s.say(`ERR %s PARSE get needs exactly one key: "get <key>"`, id)
			return
		}
		s.enqueue(id, func() {
			v, err := s.db.Get(key)
			switch {
			case errors.Is(err, engine.ErrNotFound):
				s.say("NOTFOUND %s", id)
			case err != nil:
				s.reply(id, err)
			default:
				s.say("VALUE %s %s", id, token.Value(v))
			}
		})
	case "compact":
		s.enqueue(id, func() { s.reply(id, s.db.Compact()) })
	}
}

// add appends an operation to an open batch. Past the batch limits the batch
// is marked bad and its contents dropped, so a runaway batch cannot exhaust
// memory; its commit then fails.
func (b *openBatch) add(op engine.Operation, err error) {
	if b.bad != nil {
		return
	}
	if err != nil {
		b.bad = err
		return
	}
	b.ops = append(b.ops, op)
	b.bytes += len(op.Key) + len(op.Value)
	if len(b.ops) > storage.MaxOpsPerFrame || b.bytes > storage.MaxPayload {
		b.bad = fmt.Errorf("batch is over the limit of %d operations or %d bytes", storage.MaxOpsPerFrame, storage.MaxPayload)
		b.ops = nil
	}
}

// reply reports the outcome of a write: ACK only if it succeeded.
func (s *server) reply(id string, err error) {
	if err == nil {
		s.say("ACK %s", id)
		return
	}
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	s.say("ERR %s %s %s", id, errorCode(err), msg)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, engine.ErrDiskFull):
		return "DISKFULL"
	case errors.Is(err, engine.ErrInvalidKey), errors.Is(err, engine.ErrValueTooLarge),
		errors.Is(err, engine.ErrInvalidOp), errors.Is(err, engine.ErrTooManyOps),
		errors.Is(err, engine.ErrBatchTooLarge):
		return "INVALID"
	case errors.Is(err, engine.ErrFailed), errors.Is(err, engine.ErrClosed):
		return "FAILED"
	}
	return "IO"
}

// writer returns the queue an id belongs to: the part before the first ".",
// or the shared default writer.
func writerOf(id string) string {
	if i := strings.IndexByte(id, '.'); i > 0 {
		return id[:i]
	}
	return ""
}

// enqueue runs job on the goroutine of id's writer, after that writer's
// earlier jobs.
func (s *server) enqueue(id string, job func()) {
	w := writerOf(id)
	q, ok := s.queues[w]
	if !ok {
		q = make(chan func(), 1024)
		s.queues[w] = q
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for job := range q {
				job()
			}
		}()
	}
	q <- job
}
