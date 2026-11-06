// Package engine is the public database handle. It wires the lock, storage,
// recovery and index components together and owns the concurrency lock.
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"astaroth/src/failpoint"
	"astaroth/src/index"
	"astaroth/src/lock"
	"astaroth/src/recovery"
	"astaroth/src/storage"
)

var (
	ErrClosed         = errors.New("engine: database is closed")
	ErrInvalidPath    = errors.New("engine: empty database path")
	ErrInvalidOptions = errors.New("engine: invalid options")
	ErrNotFound       = errors.New("engine: key not found")

	// ErrFailed is returned by every write after an I/O error left the file in
	// an unknown state. Reads still work. Close and reopen to recover.
	ErrFailed = errors.New("engine: database failed after an I/O error; close and reopen")

	// ErrIndexMismatch means the index pointed at a record that is not the
	// requested key's. It indicates a bug; the wrong value is never returned.
	ErrIndexMismatch = errors.New("engine: index points at the wrong record")

	// Limit errors, re-exported so callers need only this package.
	ErrInvalidKey    = storage.ErrInvalidKey
	ErrValueTooLarge = storage.ErrValueTooLarge
	ErrInvalidOp     = storage.ErrInvalidOp

	// ErrDiskFull is returned by a write that does not fit under
	// Options.MaxBytes, or that the disk itself had no room for. Nothing of
	// that write is in the database; everything acknowledged before is intact
	// and the database stays readable and writable for writes that fit.
	ErrDiskFull      = errors.New("engine: database is full")
	ErrTooManyOps    = storage.ErrTooManyOps    // more than 10,000 operations in one batch
	ErrBatchTooLarge = storage.ErrFrameTooLarge // batch encodes to more than 64 MiB
)

// OpKind says what a batch Operation does.
type OpKind uint8

const (
	OpPut    = OpKind(storage.OpPut)
	OpDelete = OpKind(storage.OpDel)
)

// Operation is one step of a Batch.
type Operation struct {
	Kind  OpKind
	Key   []byte
	Value []byte // OpPut only; must be empty for OpDelete
}

// DefaultLockTimeout is how long Open waits for another handle to release the
// database before failing with lock.ErrLocked.
const DefaultLockTimeout = time.Second

// Options configure Open.
type Options struct {
	// MaxBytes caps the size of the data file (and of the temporary file a
	// compaction writes). 0 means unlimited; otherwise it must be at least 32,
	// the size of the file header. Puts and batches containing a put may grow
	// the file to MaxBytes minus a small reserve (see deleteReserve); batches
	// of only deletes may use the reserve, so a full database can always
	// delete its way back to free space.
	MaxBytes int64
	// LockTimeout bounds the wait for the process lock. 0 means DefaultLockTimeout.
	LockTimeout time.Duration
}

// DB is an open database. All methods are safe for concurrent use.
//
// Locking: writers hold mu exclusively from the start of the append until the
// index is updated; readers hold it shared. A reader therefore never sees a
// write that is not yet on disk.
type DB struct {
	mu            sync.RWMutex
	opts          Options
	path          string
	lock          *lock.Lock
	f             *os.File // nil only after a compaction failed to reopen the file
	idx           *index.Index
	end           int64  // offset where the next frame is appended
	nextSeq       uint64 // sequence number of the next frame
	gen           uint64 // header generation of the current file
	closed        bool
	failed        error // set after an I/O error left the file in an unknown state
	noAutoCompact bool  // an automatic compaction failed; do not retry it
	recovered     RecoveryInfo
}

// Open opens the database at path, creating it if it does not exist, and runs
// recovery. The process lock is held until Close.
func Open(path string, opts Options) (*DB, error) {
	if path == "" {
		return nil, ErrInvalidPath
	}
	if opts.MaxBytes < 0 || (opts.MaxBytes > 0 && opts.MaxBytes < storage.HeaderSize) {
		return nil, fmt.Errorf("%w: MaxBytes %d must be 0 (unlimited) or at least %d", ErrInvalidOptions, opts.MaxBytes, storage.HeaderSize)
	}
	if opts.LockTimeout < 0 {
		return nil, fmt.Errorf("%w: LockTimeout %v is negative", ErrInvalidOptions, opts.LockTimeout)
	}
	if opts.LockTimeout == 0 {
		opts.LockTimeout = DefaultLockTimeout
	}

	l, err := lock.Acquire(path+".lock", opts.LockTimeout)
	if err != nil {
		return nil, fmt.Errorf("engine: open %s: %w", path, err)
	}
	if err := recovery.CleanupCompaction(path); err != nil {
		l.Release()
		return nil, fmt.Errorf("engine: open %s: %w", path, err)
	}
	f, err := storage.OpenFile(path)
	if err != nil {
		l.Release()
		return nil, fmt.Errorf("engine: open %s: %w", path, err)
	}
	idx := index.New()
	res, err := recovery.Recover(f, idx)
	if err != nil {
		f.Close()
		l.Release()
		return nil, fmt.Errorf("engine: open %s: %w", path, err)
	}

	return &DB{
		opts:    opts,
		path:    path,
		lock:    l,
		f:       f,
		idx:     idx,
		end:     res.End,
		nextSeq: res.NextSeq,
		gen:     res.Header.Generation,
		recovered: RecoveryInfo{
			Frames:         res.Frames,
			TruncatedBytes: res.TruncatedBytes,
			Created:        res.Created,
			Generation:     res.Header.Generation,
		},
	}, nil
}

// Put stores value under key, replacing any previous value. When Put returns
// nil the record is on disk: it has been written and fsynced.
func (db *DB) Put(key, value []byte) error {
	op := storage.Op{Type: storage.OpPut, Key: key, Value: value}
	if err := storage.ValidateOp(op); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.writableLocked(); err != nil {
		return err
	}
	if err := db.autoCompactLocked(); err != nil {
		return err
	}
	return db.appendLocked([]storage.Op{op})
}

// Delete removes key. Deleting a missing key is not an error. When Delete
// returns nil the key is gone durably.
func (db *DB) Delete(key []byte) error {
	op := storage.Op{Type: storage.OpDel, Key: key}
	if err := storage.ValidateOp(op); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.writableLocked(); err != nil {
		return err
	}
	// The index holds exactly the durable, acknowledged state, so a key it
	// does not hold needs no tombstone: it is already gone after any crash.
	if _, ok := db.idx.Get(key); !ok {
		return nil
	}
	if err := db.autoCompactLocked(); err != nil {
		return err
	}
	return db.appendLocked([]storage.Op{op})
}

// Batch applies ops atomically: after a crash either every operation is in the
// database or none is, and a concurrent Get sees the state before the batch or
// after it, never a part of it. Operations apply in order, so when several
// touch the same key the last one wins: [put x, delete x] leaves x absent and
// [delete x, put x=v] leaves x = v. When Batch returns nil the whole batch is
// on disk. An empty batch does nothing and writes nothing.
//
// The whole batch is one frame. Its seq is the batch's transaction id, and its
// trailing CRC is the commit marker: it covers the frame header (id, op count,
// length) and every record, so it cannot validate unless all of them are on
// disk. See appendLocked for the write ordering.
//
// Every operation is validated before anything is written; one invalid
// operation rejects the whole batch.
func (db *DB) Batch(ops []Operation) error {
	if len(ops) > storage.MaxOpsPerFrame {
		return fmt.Errorf("%w (got %d)", ErrTooManyOps, len(ops))
	}
	sops := make([]storage.Op, len(ops))
	for i, o := range ops {
		sops[i] = storage.Op{Type: uint8(o.Kind), Key: o.Key, Value: o.Value}
		if err := storage.ValidateOp(sops[i]); err != nil {
			return fmt.Errorf("engine: batch operation %d: %w", i, err)
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.writableLocked(); err != nil {
		return err
	}
	if len(sops) == 0 {
		return nil
	}
	if err := db.autoCompactLocked(); err != nil {
		return err
	}
	return db.appendLocked(sops)
}

// Get returns the current value for key, or ErrNotFound. The returned slice
// belongs to the caller.
func (db *DB) Get(key []byte) ([]byte, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, err
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	e, ok := db.idx.Get(key)
	if !ok {
		return nil, ErrNotFound
	}
	return db.readLocked(key, e)
}

// readLocked reads the value the index entry e points at. The caller holds mu.
func (db *DB) readLocked(key []byte, e index.Entry) ([]byte, error) {
	if db.f == nil {
		return nil, fmt.Errorf("%w: %v", ErrFailed, db.failed)
	}
	buf := make([]byte, storage.PutHeaderSize+len(key)+int(e.ValLen))
	n, err := db.f.ReadAt(buf, e.RecOff)
	if n < len(buf) {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("engine: read record at %d: %w", e.RecOff, err)
	}
	// Check that the record really is this key's PUT before trusting it: an
	// offset bug must produce an error, never another key's value.
	op, _, err := storage.ParseOp(buf)
	if err != nil || op.Type != storage.OpPut || !bytes.Equal(op.Key, key) || len(op.Value) != int(e.ValLen) {
		return nil, ErrIndexMismatch
	}
	return op.Value, nil
}

// Len returns the number of live keys.
func (db *DB) Len() (int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return 0, ErrClosed
	}
	return db.idx.Len(), nil
}

// autoCompactLocked runs an automatic compaction if one is due, and reports
// whether the handle can still write afterwards.
func (db *DB) autoCompactLocked() error {
	db.maybeAutoCompactLocked()
	return db.writableLocked()
}

func (db *DB) writableLocked() error {
	if db.closed {
		return ErrClosed
	}
	if db.failed != nil {
		return fmt.Errorf("%w: %v", ErrFailed, db.failed)
	}
	return nil
}

// appendLocked is the single write path for Put, Delete and Batch: ops become
// one frame. The caller holds mu exclusively and has validated the ops.
//
// The frame is laid out as
//
//	header (txn id = seq, op_count, payload_len) | records ... | commit marker (CRC32C)
//
// Order matters:
//  1. Encode the frame. The commit marker is computed last, over the header
//     and every record.
//  2. WriteAt the whole frame in one call, at the offset we own: records and
//     marker together. No fsync is needed between them: the marker is a
//     checksum of the records, not a flag, so it validates only if every byte
//     it covers is on disk, in whatever order the bytes got there.
//  3. Sync. Only after it returns is the batch durable.
//  4. Apply all ops to the index, still under the exclusive lock, so readers
//     (who take the lock shared) see none of the batch or all of it. A reader
//     can never see data a crash could erase, and a failed write never
//     becomes visible.
//  5. Return. The caller may now acknowledge the batch.
func (db *DB) appendLocked(ops []storage.Op) error {
	// The size limit is checked before anything is encoded or written: a
	// write that does not fit leaves no byte behind, and a batch is checked
	// as a whole, so the limit can never commit part of a batch. (Checked
	// before encoding because making room may compact the file, which moves
	// the transaction ids on.)
	if err := db.ensureRoomLocked(ops); err != nil {
		return err
	}
	buf, err := storage.EncodeFrame(db.nextSeq, ops)
	if err != nil {
		return err
	}

	failpoint.Hit("append-before-write")
	if failpoint.Fire("append-partial-header") { // a write cut off inside the frame header
		db.f.WriteAt(buf[:storage.FrameHeaderSize/2], db.end)
		failpoint.Exit()
	}
	if failpoint.Fire("append-partial-write") { // a write cut off in the middle of the frame
		db.f.WriteAt(buf[:len(buf)/2], db.end)
		failpoint.Exit()
	}
	if failpoint.Fire("append-after-3-records") { // header and the first 3 records only
		cut := len(buf) - storage.FrameTrailerSize
		if len(ops) > 3 {
			cut = ops[3].Off
		}
		db.f.WriteAt(buf[:cut], db.end)
		failpoint.Exit()
	}
	if failpoint.Fire("append-before-commit-marker") { // every record, no commit marker
		db.f.WriteAt(buf[:len(buf)-storage.FrameTrailerSize], db.end)
		failpoint.Exit()
	}
	var werr error
	if failpoint.Fire("append-disk-full") { // test hook: the disk fills up halfway through the write
		db.f.WriteAt(buf[:len(buf)/2], db.end)
		werr = storage.ErrSimulatedDiskFull
	} else {
		_, werr = db.f.WriteAt(buf, db.end)
	}
	if werr != nil {
		// Part of the frame may be on disk. Cut it off so the next append
		// lands at db.end; if that fails we cannot know what the file holds.
		// (Truncating needs no free space.)
		if terr := db.f.Truncate(db.end); terr != nil {
			db.failed = fmt.Errorf("write failed (%v) and rollback failed (%v)", werr, terr)
		}
		if storage.IsDiskFull(werr) {
			return fmt.Errorf("%w: %v", ErrDiskFull, werr)
		}
		return fmt.Errorf("engine: write: %w", werr)
	}
	failpoint.Hit("append-after-write") // records and commit marker written, not yet synced
	if err := db.f.Sync(); err != nil {
		// Never retry a failed fsync: the OS may have dropped the dirty pages,
		// so a second Sync could report success for data that is gone. The
		// frame may or may not survive; that is legal for an unacknowledged op.
		db.failed = fmt.Errorf("sync failed: %v", err)
		return fmt.Errorf("engine: sync: %w", err)
	}

	failpoint.Hit("append-after-sync") // durable, not yet visible or acknowledged
	db.idx.Apply(db.end, ops)
	db.end += int64(len(buf))
	db.nextSeq++
	return nil
}

// Close syncs and closes the data file and releases the process lock. The
// handle is unusable afterwards even if Close returns an error. A second Close
// returns ErrClosed.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	db.closed = true

	var errs []error
	if db.f != nil {
		if err := db.f.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("engine: sync: %w", err))
		}
		if err := db.f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("engine: close file: %w", err))
		}
	}
	if err := db.lock.Release(); err != nil {
		errs = append(errs, err)
	}
	db.f, db.lock = nil, nil
	return errors.Join(errs...)
}
