package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"

	"astaroth/src/index"
	"astaroth/src/lock"
	"astaroth/src/recovery"
	"astaroth/src/storage"
)

// ErrNoDatabase is returned by Check for a path where no database exists.
var ErrNoDatabase = errors.New("engine: no database at this path")

// RecoveryInfo describes what Open found and did.
type RecoveryInfo struct {
	Frames         int    // committed frames replayed
	TruncatedBytes int64  // torn tail removed
	Created        bool   // the file was new (or had only a torn header)
	Generation     uint64 // header generation: 1, plus one per compaction
}

// Recovery reports what the Open that returned this handle found and did.
func (db *DB) Recovery() RecoveryInfo {
	return db.recovered
}

// Dump calls fn for every key and its value, in ascending byte order of the
// keys. It holds the read lock throughout, so it sees one consistent state:
// a batch is either entirely in it or not at all. fn must not call other
// methods of db. Dump stops at the first error from fn and returns it.
func (db *DB) Dump(fn func(key, value []byte) error) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	keys := make([]string, 0, db.idx.Len())
	db.idx.Range(func(k string, _ index.Entry) bool {
		keys = append(keys, k)
		return true
	})
	sort.Strings(keys) // byte-wise order
	for _, k := range keys {
		e, _ := db.idx.Get([]byte(k))
		v, err := db.readLocked([]byte(k), e)
		if err != nil {
			return err
		}
		if err := fn([]byte(k), v); err != nil {
			return err
		}
	}
	return nil
}

// CheckReport is what Check finds.
type CheckReport struct {
	recovery.Report
	// AbandonedTemp: a compaction temporary file exists next to an intact
	// data file. The next open removes it.
	AbandonedTemp bool
	// InterruptedReplace: the data file is missing and a complete compaction
	// temporary file exists. The next open renames it into place; the report
	// describes that file.
	InterruptedReplace bool
}

// Check verifies the database at path without changing it: it never creates,
// repairs or truncates anything (see recovery.Check). It takes the process
// lock, so it fails with lock.ErrLocked while another process has the
// database open.
func Check(path string, opts Options) (CheckReport, error) {
	if path == "" {
		return CheckReport{}, ErrInvalidPath
	}
	timeout := opts.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	l, err := lock.Acquire(path+".lock", timeout)
	if err != nil {
		return CheckReport{}, fmt.Errorf("engine: check %s: %w", path, err)
	}
	defer l.Release()

	var rep CheckReport
	target := path
	tmp := path + storage.CompactTempSuffix
	_, perr := os.Stat(path)
	_, terr := os.Stat(tmp)
	switch {
	case perr == nil:
		rep.AbandonedTemp = terr == nil
	case errors.Is(perr, fs.ErrNotExist) && terr == nil:
		rep.InterruptedReplace, target = true, tmp
	case errors.Is(perr, fs.ErrNotExist):
		return CheckReport{}, fmt.Errorf("%w: %s", ErrNoDatabase, path)
	default:
		return CheckReport{}, fmt.Errorf("engine: check %s: %w", path, perr)
	}

	f, err := os.Open(target)
	if err != nil {
		return CheckReport{}, fmt.Errorf("engine: check %s: %w", target, err)
	}
	defer f.Close()
	rep.Report, err = recovery.Check(f)
	if err != nil {
		return CheckReport{}, fmt.Errorf("engine: check %s: %w", target, err)
	}
	return rep, nil
}
