// Package recovery brings a data file to a state the engine can append to.
//
// Recovery is read-only except for two idempotent actions, so a crash inside
// it is safe: re-running it sees the same bytes and reaches the same decision.
//   - writing a missing or torn header (only possible when no frames exist)
//   - truncating a torn tail
package recovery

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"astaroth/src/failpoint"
	"astaroth/src/index"
	"astaroth/src/storage"
)

var (
	// ErrNotOurFile means the file has bytes that no crash of this database
	// can produce: wrong magic, bad header checksum, or an unknown version.
	ErrNotOurFile = errors.New("recovery: not an Astaroth database file")
	// ErrCorrupt means damage that is not a torn tail: a checksum-valid frame
	// with malformed contents, a sequence gap, or an invalid region too large
	// to be a single interrupted write. Recovery refuses to guess and leaves
	// the file untouched.
	ErrCorrupt = errors.New("recovery: file is corrupt")
)

// Replay sizes the index once it has seen a sample of the log, so a log of a
// million keys does not make the map grow and rehash a dozen times.
const (
	// sampleBytes is how much of the log is replayed before estimating.
	sampleBytes = 1 << 20
	// maxReserve bounds the estimate, so a misleading sample (a small first
	// part followed by huge values) can waste at most a few hundred MB.
	maxReserve = 4_000_000
)

// Result describes the file after recovery.
type Result struct {
	// End is the offset where the next frame will be appended.
	End int64
	// NextSeq is the sequence number of the next frame.
	NextSeq uint64
	// Frames is the number of frames replayed.
	Frames int
	// Created reports whether recovery had to write the header.
	Created bool
	// TruncatedBytes is the size of the torn tail that was removed.
	TruncatedBytes int64
	// Header is the validated file header.
	Header storage.Header
}

// Recover validates f, replays every committed frame into idx, and removes a
// torn tail. idx must be empty.
func Recover(f *os.File, idx *index.Index) (Result, error) {
	st, err := f.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("recovery: stat: %w", err)
	}
	size := st.Size()

	if size < storage.HeaderSize {
		return recoverTornStart(f, size)
	}

	var raw [storage.HeaderSize]byte
	if _, err := f.ReadAt(raw[:], 0); err != nil {
		return Result{}, fmt.Errorf("recovery: read header: %w", err)
	}
	h, err := storage.DecodeHeader(raw[:])
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotOurFile, err)
	}

	res := Result{Header: h}
	if err := scanLog(f, size, idx, &res); err != nil {
		return Result{}, err
	}
	if tail := size - res.End; tail > 0 {
		failpoint.Hit("recover-before-truncate")
		if err := f.Truncate(res.End); err != nil {
			return Result{}, fmt.Errorf("recovery: truncate torn tail: %w", err)
		}
		failpoint.Hit("recover-after-truncate")
		if err := f.Sync(); err != nil {
			return Result{}, fmt.Errorf("recovery: sync after truncate: %w", err)
		}
		failpoint.Hit("recover-after-truncate-sync")
		res.TruncatedBytes = tail
	}
	return res, nil
}

// Report is what Check finds in a data file, without changing it.
type Report struct {
	Size             int64          // file size in bytes
	Header           storage.Header // valid unless HeaderIncomplete
	HeaderIncomplete bool           // file shorter than a header, as a kill during creation leaves it
	Frames           int            // committed frames
	Keys             int            // live keys
	LiveBytes        int64          // size of the live records
	End              int64          // where the committed log ends
	TornTail         int64          // bytes after End that the next open would remove
}

// Check verifies a data file the way recovery reads it, but only reads: it
// never truncates a torn tail or rewrites a header. Every byte of every frame
// is checked against its CRC, transaction ids must be consecutive, and every
// record must parse. A torn tail is reported, not treated as an error: it is
// what a crash during a write leaves, and the next open removes it. It returns
// ErrNotOurFile or ErrCorrupt for damage that no crash can produce.
func Check(f *os.File) (Report, error) {
	st, err := f.Stat()
	if err != nil {
		return Report{}, fmt.Errorf("recovery: stat: %w", err)
	}
	rep := Report{Size: st.Size()}

	if rep.Size < storage.HeaderSize {
		got := make([]byte, rep.Size)
		if rep.Size > 0 {
			if _, err := f.ReadAt(got, 0); err != nil {
				return Report{}, fmt.Errorf("recovery: read torn header: %w", err)
			}
		}
		want := storage.NewHeader().Encode()
		if !bytes.Equal(got, want[:rep.Size]) && !allZero(got) {
			return Report{}, fmt.Errorf("%w: %d-byte file is not a header prefix", ErrNotOurFile, rep.Size)
		}
		rep.HeaderIncomplete = true
		return rep, nil
	}

	var raw [storage.HeaderSize]byte
	if _, err := f.ReadAt(raw[:], 0); err != nil {
		return Report{}, fmt.Errorf("recovery: read header: %w", err)
	}
	if rep.Header, err = storage.DecodeHeader(raw[:]); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrNotOurFile, err)
	}
	idx := index.New()
	var res Result
	if err := scanLog(f, rep.Size, idx, &res); err != nil {
		return Report{}, err
	}
	rep.Frames, rep.Keys, rep.LiveBytes = res.Frames, idx.Len(), idx.LiveBytes()
	rep.End, rep.TornTail = res.End, rep.Size-res.End
	return rep, nil
}

// scanLog replays every committed frame of the log into idx and records where
// the committed log ends (res.End) and the next transaction id. It only reads.
//
// The first frame that is not a complete, checksum-valid frame ends the log.
// Only one frame can be in flight at a time (writes are serialized and each is
// synced before the next starts), so a legitimate torn tail is shorter than one
// maximum-size frame. A longer invalid region is not something a crash can
// produce, and truncating it could silently destroy acknowledged data.
func scanLog(f io.ReaderAt, size int64, idx *index.Index, res *Result) error {
	br := bufio.NewReaderSize(io.NewSectionReader(f, storage.HeaderSize, size-storage.HeaderSize), 1<<20)
	off := int64(storage.HeaderSize)
	nextSeq := uint64(1)
	reserved := false
	frames := storage.NewFrameReader(br)

scan:
	for {
		fr, err := frames.Next(size - off)
		switch {
		case err == io.EOF, errors.Is(err, storage.ErrTornFrame):
			break scan
		case errors.Is(err, storage.ErrCorruptFrame):
			return fmt.Errorf("%w: frame at offset %d: %v", ErrCorrupt, off, err)
		case err != nil:
			return fmt.Errorf("recovery: frame at offset %d: %w", off, err)
		}
		if res.Frames > 0 && fr.Seq != nextSeq {
			return fmt.Errorf("%w: frame at offset %d has seq %d, want %d", ErrCorrupt, off, fr.Seq, nextSeq)
		}
		idx.Apply(off, fr.Ops)
		failpoint.Hit("recover-replay-frame")
		nextSeq = fr.Seq + 1
		off += fr.Size
		res.Frames++

		if !reserved && off-storage.HeaderSize >= sampleBytes {
			// Distinct keys per byte so far, scaled to the whole file. Overwrite
			// heavy logs give a small estimate, so they do not over-allocate.
			reserved = true
			est := (size - storage.HeaderSize) * int64(idx.Len()) / (off - storage.HeaderSize)
			if est > maxReserve {
				est = maxReserve
			}
			idx.Reserve(int(est))
		}
	}

	if tail := size - off; tail > storage.MaxFrameSize {
		return fmt.Errorf("%w: %d invalid bytes at offset %d are too many for one interrupted write", ErrCorrupt, tail, off)
	}
	res.End = off
	res.NextSeq = nextSeq
	return nil
}

// recoverTornStart handles a file shorter than a header. That is what a kill
// during file creation leaves: empty, or a prefix of the header we write
// (some filesystems may also extend the file with zeros). No frame can exist
// yet, so rewriting the header loses nothing. Anything else is not our file.
func recoverTornStart(f *os.File, size int64) (Result, error) {
	got := make([]byte, size)
	if size > 0 {
		if _, err := f.ReadAt(got, 0); err != nil {
			return Result{}, fmt.Errorf("recovery: read torn header: %w", err)
		}
	}
	h := storage.NewHeader()
	want := h.Encode()
	if !bytes.Equal(got, want[:size]) && !allZero(got) {
		return Result{}, fmt.Errorf("%w: %d-byte file is not a header prefix", ErrNotOurFile, size)
	}
	failpoint.Hit("recover-before-header")
	if err := storage.WriteHeader(f, h); err != nil {
		return Result{}, err
	}
	failpoint.Hit("recover-after-header")
	return Result{End: storage.HeaderSize, NextSeq: 1, Created: true, Header: h}, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
