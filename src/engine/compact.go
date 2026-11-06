package engine

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"astaroth/src/failpoint"
	"astaroth/src/index"
	"astaroth/src/recovery"
	"astaroth/src/storage"
)

// Compaction limits for the frames of the new file.
const (
	compactFrameOps   = 1000
	compactFrameBytes = 8 << 20
)

// Automatic compaction runs before a write when the file is at least this big
// and less than half of it is live data.
const autoCompactMinBytes = 4 << 20

// deleteReserve is the part of MaxBytes that only delete-only writes may use.
// Without it a completely full file could not even append a tombstone, and so
// could never free space. A tombstone frame is at most 1,051 bytes.
func (db *DB) deleteReserve() int64 {
	r := db.opts.MaxBytes / 16
	if r < 4<<10 {
		r = 4 << 10
	}
	if r > 4<<20 {
		r = 4 << 20
	}
	if r > db.opts.MaxBytes/4 {
		r = db.opts.MaxBytes / 4
	}
	return r
}

// writeLimit is how large the file may grow for a frame holding ops.
func (db *DB) writeLimit(ops []storage.Op) int64 {
	for _, op := range ops {
		if op.Type != storage.OpDel {
			return db.opts.MaxBytes - db.deleteReserve()
		}
	}
	return db.opts.MaxBytes
}

// compactedSize estimates the file size compaction would produce: header, the
// live records, and the overhead of their frames.
func (db *DB) compactedSize() int64 {
	live := db.idx.LiveBytes()
	frames := int64(db.idx.Len())/compactFrameOps + live/compactFrameBytes + 1
	return storage.HeaderSize + live + frames*storage.FrameOverhead
}

// ensureRoomLocked returns nil if a frame of ops fits under MaxBytes. If it
// does not, but compacting would make enough room, it compacts first. The
// caller holds mu exclusively.
func (db *DB) ensureRoomLocked(ops []storage.Op) error {
	if db.opts.MaxBytes == 0 {
		return nil
	}
	need, limit := storage.FrameSize(ops), db.writeLimit(ops)
	if db.end+need <= limit {
		return nil
	}
	if db.compactedSize()+need <= limit {
		if err := db.compactLocked(); err != nil {
			if werr := db.writableLocked(); werr != nil {
				return werr
			}
			return fmt.Errorf("%w: a %d-byte write does not fit (file %d bytes, limit %d) and compacting to make room failed: %v",
				ErrDiskFull, need, db.end, limit, err)
		}
		if db.end+need <= limit {
			return nil
		}
	}
	return fmt.Errorf("%w: a %d-byte write would grow the file from %d to %d bytes, over its limit of %d (MaxBytes %d); delete keys to make room",
		ErrDiskFull, need, db.end, db.end+need, limit, db.opts.MaxBytes)
}

// Compact rewrites the database so the file holds only the live data: the
// latest value of every existing key, and no deleted keys, overwritten values
// or tombstones. The content of the database does not change.
//
// Crash safety rests on one rule: the old file is not touched until the new
// one is complete, synced and verified, and the switch is a single atomic
// replace. A crash at any point leaves either the old file or the new one as
// the data file, each complete. See compactLocked for the order.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.writableLocked(); err != nil {
		return err
	}
	return db.compactLocked()
}

// maybeAutoCompactLocked compacts before a write when more than half the file
// is dead. If an automatic compaction fails it is not retried for the life of
// this handle (so a persistent problem does not turn every write into a full
// rewrite); Compact can still be called explicitly.
func (db *DB) maybeAutoCompactLocked() {
	if db.noAutoCompact || db.end < autoCompactMinBytes {
		return
	}
	if 2*(storage.HeaderSize+db.idx.LiveBytes()) >= db.end {
		return
	}
	if err := db.compactLocked(); err != nil {
		db.noAutoCompact = true
	}
}

// compactLocked does the work. The caller holds mu exclusively, so no reader
// or writer runs during compaction.
//
// Order:
//  1. Create <path>.compact.tmp, truncating any leftover.
//  2. Write the new header (generation + 1), then every live record, read
//     sequentially from the old file, in frames of up to 1,000 records.
//  3. Sync the temporary file.
//  4. Verify it: run the real recovery on it and require exactly the index we
//     built while writing it. A mismatch aborts; the old file is untouched.
//  5. Close the temporary file and the old data file (Windows cannot replace
//     an open file).
//  6. Atomically replace the data file with the temporary file
//     (storage.ReplaceFile). This is the commit point of the compaction.
//  7. Sync the directory so the rename is durable (Unix; on Windows the
//     replace is already write-through).
//  8. Reopen the data file and switch to the new index.
//
// A failure before step 6 removes the temporary file and leaves the database
// as it was. A failure after step 6 leaves the new file as the data file and
// marks the handle failed; reopening recovers it.
func (db *DB) compactLocked() error {
	failpoint.Hit("compact-start")
	tmpPath := db.path + storage.CompactTempSuffix
	tmp, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("engine: compact: create %s: %w", tmpPath, err)
	}
	abort := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		if storage.IsDiskFull(err) && !errors.Is(err, ErrDiskFull) {
			err = fmt.Errorf("%w: %v", ErrDiskFull, err)
		}
		return fmt.Errorf("engine: compact: %w", err)
	}

	newIdx, newEnd, newSeq, err := db.writeCompacted(tmp)
	if err != nil {
		return abort(err)
	}
	failpoint.Hit("compact-before-sync")
	if err := tmp.Sync(); err != nil {
		return abort(fmt.Errorf("sync %s: %w", tmpPath, err))
	}
	if failpoint.Fire("compact-corrupt-tmp") { // test hook: damage the new file to prove step 4 catches it
		b := []byte{0}
		tmp.ReadAt(b, newEnd-1)
		tmp.WriteAt([]byte{b[0] ^ 0xFF}, newEnd-1)
	}
	failpoint.Hit("compact-before-verify")
	if err := verifyCompacted(tmp, newIdx, newEnd); err != nil {
		return abort(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("engine: compact: close %s: %w", tmpPath, err)
	}

	// The replacement sequence. The old data file stays complete and
	// authoritative until ReplaceFile returns.
	db.f.Close()
	db.f = nil
	failpoint.Hit("compact-before-rename")
	if err := storage.ReplaceFile(tmpPath, db.path); err != nil {
		os.Remove(tmpPath)
		return db.reopenLocked(fmt.Errorf("engine: compact: %w", err))
	}
	failpoint.Hit("compact-after-rename")
	if err := storage.SyncDir(filepath.Dir(db.path)); err != nil {
		// The rename happened but may not be durable. The new file is complete,
		// so either outcome after a crash is correct; carry on with it.
		db.failed = fmt.Errorf("compaction: sync directory: %v", err)
	}
	failpoint.Hit("compact-after-dir-sync")
	f, err := storage.OpenFile(db.path)
	if err != nil {
		db.failed = fmt.Errorf("compaction: reopen data file: %v", err)
		return fmt.Errorf("engine: compact: reopen: %w", err)
	}
	db.f = f
	db.idx, db.end, db.nextSeq = newIdx, newEnd, newSeq
	db.gen++
	failpoint.Hit("compact-done")
	return nil
}

// reopenLocked reopens the unchanged data file after a compaction that failed
// before its replace, and returns cause.
func (db *DB) reopenLocked(cause error) error {
	f, err := storage.OpenFile(db.path)
	if err != nil {
		db.failed = fmt.Errorf("compaction failed (%v) and reopening the data file failed (%v)", cause, err)
		return cause
	}
	db.f = f
	return cause
}

// writeCompacted writes the header and the live records into tmp and returns
// the index of the new file, its end offset and the next transaction id.
//
// It scans the old log sequentially. A PUT record is live if the index points
// at exactly that record; every other record (an overwritten value or a
// tombstone) is dropped. Dropping tombstones is safe because the new file is a
// complete snapshot: there is no older record left in it for them to hide.
func (db *DB) writeCompacted(tmp *os.File) (*index.Index, int64, uint64, error) {
	h := storage.Header{Version: storage.Version, Generation: db.gen + 1}.Encode()
	if _, err := tmp.Write(h[:]); err != nil {
		return nil, 0, 0, fmt.Errorf("write header: %w", err)
	}

	newIdx := index.New()
	newIdx.Reserve(db.idx.Len())
	off := int64(storage.HeaderSize)
	seq := db.nextSeq
	var pending []storage.Op
	pendingBytes := 0
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		buf, err := storage.EncodeFrame(seq, pending)
		if err != nil {
			return err
		}
		if max := db.opts.MaxBytes; max > 0 && off+int64(len(buf)) > max {
			return fmt.Errorf("%w: the compacted file would exceed the %d-byte limit", ErrDiskFull, max)
		}
		if failpoint.Fire("compact-disk-full") { // test hook: the disk fills up while writing the new file
			tmp.Write(buf[:len(buf)/2])
			return fmt.Errorf("write frame: %w", storage.ErrSimulatedDiskFull)
		}
		if _, err := tmp.Write(buf); err != nil {
			return fmt.Errorf("write frame: %w", err)
		}
		failpoint.Hit("compact-write-frame") // some frames are in the temporary file
		newIdx.Apply(off, pending)
		off += int64(len(buf))
		seq++
		pending, pendingBytes = pending[:0], 0
		return nil
	}

	src := storage.NewFrameReader(bufio.NewReaderSize(
		io.NewSectionReader(db.f, storage.HeaderSize, db.end-storage.HeaderSize), 1<<20))
	pos := int64(storage.HeaderSize)
	for {
		fr, err := src.Next(db.end - pos)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Everything up to db.end was replayed or written by this handle, so
			// this is damage or a bug, not a crash. Never compact past it.
			return nil, 0, 0, fmt.Errorf("read old log at %d: %w", pos, err)
		}
		for _, op := range fr.Ops {
			if op.Type != storage.OpPut {
				continue
			}
			if e, ok := db.idx.Get(op.Key); !ok || e.RecOff != pos+int64(op.Off) {
				continue
			}
			// The frame reader reuses its buffers: copy what we keep.
			pending = append(pending, storage.Op{
				Type:  storage.OpPut,
				Key:   append([]byte(nil), op.Key...),
				Value: append([]byte(nil), op.Value...),
			})
			pendingBytes += storage.PutHeaderSize + len(op.Key) + len(op.Value)
			if len(pending) == compactFrameOps || pendingBytes >= compactFrameBytes {
				if err := flush(); err != nil {
					return nil, 0, 0, err
				}
			}
		}
		pos += fr.Size
	}
	if err := flush(); err != nil {
		return nil, 0, 0, err
	}
	if newIdx.Len() != db.idx.Len() || newIdx.LiveBytes() != db.idx.LiveBytes() {
		return nil, 0, 0, fmt.Errorf("copied %d keys (%d bytes), index has %d (%d bytes)",
			newIdx.Len(), newIdx.LiveBytes(), db.idx.Len(), db.idx.LiveBytes())
	}
	return newIdx, off, seq, nil
}

// verifyCompacted runs recovery on the synced temporary file and requires it
// to produce exactly the index built while writing it, ending exactly where
// the writer stopped. This is what the next Open would see.
func verifyCompacted(tmp *os.File, want *index.Index, end int64) error {
	got := index.New()
	res, err := recovery.Recover(tmp, got)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if res.End != end || res.TruncatedBytes != 0 || got.Len() != want.Len() || got.LiveBytes() != want.LiveBytes() {
		return fmt.Errorf("verify: new file recovers to %d keys ending at %d (truncated %d), want %d keys ending at %d",
			got.Len(), res.End, res.TruncatedBytes, want.Len(), end)
	}
	var mismatch error
	want.Range(func(k string, e index.Entry) bool {
		if ge, ok := got.Get([]byte(k)); !ok || ge != e {
			mismatch = fmt.Errorf("verify: key %q recovers to %+v, want %+v", k, ge, e)
			return false
		}
		return true
	})
	return mismatch
}
