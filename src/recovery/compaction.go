package recovery

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"astaroth/src/failpoint"
	"astaroth/src/storage"
)

// CleanupCompaction deals with a compaction that was interrupted, before the
// data file is opened. The caller holds the process lock, so no compaction is
// running. Both actions are safe to repeat if recovery itself is killed.
//
//   - Data file present, temporary file present: the compaction died before
//     its rename. The data file is the old, complete, authoritative state; the
//     temporary file may be incomplete and is never authoritative. Remove it.
//     If removing fails (a virus scanner may hold it for a moment) it is left:
//     it is still ignored, and the next compaction truncates it.
//   - Data file missing, temporary file present: this cannot happen with an
//     atomic replace, but it is what a replace done as "remove old, rename
//     new" would leave if killed between the two. The temporary file is
//     renamed only after it has been fully written, synced and verified, so it
//     is complete: finish the replace by renaming it into place.
//   - No temporary file: nothing to do.
func CleanupCompaction(path string) error {
	tmp := path + storage.CompactTempSuffix
	if _, err := os.Stat(tmp); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("recovery: stat %s: %w", tmp, err)
	}

	_, err := os.Stat(path)
	switch {
	case err == nil:
		failpoint.Hit("recover-before-tmp-remove")
		os.Remove(tmp)
		return nil
	case errors.Is(err, fs.ErrNotExist):
		failpoint.Hit("recover-before-tmp-promote")
		if err := storage.ReplaceFile(tmp, path); err != nil {
			return fmt.Errorf("recovery: finish interrupted compaction: %w", err)
		}
		return storage.SyncDir(filepath.Dir(path))
	default:
		return fmt.Errorf("recovery: stat %s: %w", path, err)
	}
}
