# How I would break this

Line numbers are those of this commit. `ARCHITECTURE.md` defends the design; this page attacks it.

## 1. Crash points

| # | Process dies at | On disk | Next `open()` |
|---|---|---|---|
| 1 | `src/engine/engine.go:338` `appendLocked`: frame encoded (`:333`), `WriteAt` (`:364`) not called | earlier frames only | replays them; the operation is absent (never acknowledged) |
| 2 | `engine.go:364`, inside `WriteAt` | a prefix of the frame: part of its 20-byte header, or every record but not the 4-byte commit marker | `scanLog` (`src/recovery/recovery.go:165`) rejects the frame (length past end of file, or CRC mismatch); `Recover` truncates to the end of the last valid frame (`:87`) and syncs (`:91`) |
| 3 | `engine.go:370`, `Truncate` rolling back a failed `WriteAt` (disk full) | the partial frame may remain | as row 2 |
| 4 | `engine.go:378`-`379`, after `WriteAt`, before or inside `Sync` | whole frame, marker included, in the page cache (a process kill does not lose it) | CRC valid, frame replayed: present though never acknowledged (allowed) |
| 5 | `engine.go:387`, after `Sync`, before `index.Apply` (`:388`) | frame durable; no reply yet | replayed: present (A4) |
| 6 | `cmd/kvdb/serve.go:207`, call returned nil, `ACK` (`:209`) not yet written | frame durable | present; the client saw no ACK (allowed) |
| 7 | `recovery.go:229` -> `src/storage/file.go:26`-`29`, header of a new file | empty, or under 32 bytes that prefix our header | `recoverTornStart` (`recovery.go:216`) writes the header again; no frame can exist yet |
| 8 | `recovery.go:186`, `scanLog` replaying | unchanged: replay only reads | same state again |
| 9 | `recovery.go:87`-`91`, between `Truncate` and `Sync` | tail cut, or still there | scans again: cuts it again or finds nothing. Same result |
| 10 | `src/engine/compact.go:159`, `writeCompacted` | old file untouched; `<path>.compact.tmp` partial | `CleanupCompaction` deletes the temporary file (`src/recovery/compaction.go:41`); old file recovered |
| 11 | `compact.go:183`-`185`: temporary file synced (`:164`), verified (`:173`), old handle closed, `ReplaceFile` (`:186`) not called | old file complete, complete temporary file beside it | as row 10; only the compaction is lost |
| 12 | `compact.go:186`, inside `ReplaceFile` (`MoveFileExW` / `rename(2)`) | one atomic call: row 11's state, or the new file without a temporary file | as row 11 or row 13. If a replace ever left no data file, `compaction.go:45` renames the complete temporary file into place |
| 13 | `compact.go:190`-`197`, after the replace, before directory sync or reopen | the compacted file (generation + 1) is the data file | ordinary recovery of it |
| 14 | `compaction.go:40`-`45`, `open` killed removing or renaming the temporary file | as row 11, or no data file and a complete temporary file | repeats the same removal or rename |
| 15 | `engine.go:407`-`414`, inside `Close` | nothing: `Close` writes nothing, every acknowledged frame is already synced | as a kill just before `Close`; the OS released the lock |

## 2. The ordering argument

A write is one `WriteAt` (`pwrite`; `WriteFile` at an offset on Windows) of the whole frame, then `Sync` (`fsync`; `FlushFileBuffers`), then the index update, then the return; `serve` prints `ACK` only after the return. So an ACK means `fsync` had returned for every byte of the frame. The frame's last 4 bytes, a CRC32C over its header and records, are the commit marker. Being a checksum, not a flag, it cannot validate unless every byte it covers reached the disk, in whatever order: no fsync is needed between records and marker, and a batch, being one frame, is all or nothing. Recovery stops at the first invalid frame and `ftruncate`s (`SetEndOfFile`) there before anything is appended, so an acknowledged frame can never sit behind garbage. One lock serializes writes, each synced before the next starts, so only the last frame can be torn. A failed `fsync` is never retried (the OS may have dropped the data, and a retry could report false success); the handle refuses further writes instead.

Compaction writes `<path>.compact.tmp`, `fsync`s it, verifies it with the recovery code, then replaces the data file with one call: `MoveFileExW(MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)` on Windows, `rename(2)` on Unix. The old file is never modified, so whichever name survives is a complete file. **Directory fsync:** on Unix we `fsync` the directory after the rename (`SyncDir`), because a rename changes the directory and the file's own `fsync` does not make it survive a power cut. Windows offers no directory fsync through `os`; `MOVEFILE_WRITE_THROUGH` returns only once the rename is on disk. A process kill needs neither (the OS keeps its state), which is all the problem requires. We do **not** fsync the directory after creating a new data file (see section 3).

## 3. Known weaknesses

- **Real kills never tore a write.** On Windows a kill lands between system calls, not inside a `WriteFile`; in over 1,000 torture kills recovery never removed a torn tail. Rows 2 and 3 are verified only by tests that write part of a frame and exit, and by a test that cuts a log at every byte.
- **A torture seed does not reproduce the operation stream exactly.** The seed fixes the kill delays, but the key some deletes pick depends on Go's random map order, and later iterations depend on which in-flight operations survived. A failure is still reported in full with its `oracle.log`, but rerunning the seed may not hit it.
- **Bit rot mid-file is treated as a torn tail** when under 64 MiB follows it: everything after it is silently cut. The CRC detects the damage; telling it from a torn tail needs a look-ahead for a valid frame with the next transaction id.
- **Power loss is argued, not tested.** On Unix a new data file's directory entry is not fsynced, so a power cut just after the first acknowledged writes could lose the whole file. The Unix code (`flock`, `rename(2)`, `SyncDir`) compiles but has never run.
- **Recovery time grows with file size**: about 0.3 s for 1M keys plus about 0.5 s per GB (measured to 1 GB). A few GB of live data would pass 2 s.
- **One lock, one fsync per write:** `Get` waits during a write's fsync; compaction stops everything and needs room for a second copy (`MaxBytes` caps data file and temporary file separately). A batch must fit in 64 MiB, so 10,000 operations of 1 MB do not; a file over a lowered `MaxBytes` rejects even deletes.

## 4. What we tried that failed

- **A separate commit-marker record** (records, fsync, marker, fsync): dropped at design time for the CRC trailer, which needs one fsync and has fewer crash states.
- **Recovery speed:** the first version took 0.56-0.63 s for 1M keys. Disabling the garbage collector during replay was tried and dropped (gain within noise). One optimisation first *looked* slower because of machine drift; we moved to alternating A/B builds.
- **Tests that could not fail**, found by breaking the code on purpose: the reader-isolation test read keys in the wrong order; the disk-full rollback test only looked at the file after recovery had cleaned it; the delete-reserve test always left slack; a compaction kill test assumed 3 frames where there were 2 (its "failpoint must fire" check caught it).
- **Bugs:** `serve` rejected the first command piped from Windows PowerShell, which adds a byte-order mark (found by rehearsing the README); a bare unknown word in `serve` was taken for an id; two test packages raced for a shared test database; the stress test had a data race in its own failure flag. Caught in design: compacting inside the disk-full check after encoding would have given the frame a stale transaction id; the check now runs first.
- **The torture tool found no violation in the engine.** Against deliberately broken builds (ACK before fsync, one frame per batch operation, recovery skipping frames) it failed each in the first iteration. Its disk-full mode at first never reached the limit and had to be retuned.

## 5. If we had 24 more hours

1. **Reproducible torture failures and real torn writes:** derive every operation from (seed, iteration, writer, index) alone, and write large frames in several `WriteAt` calls so a SIGKILL can land between them.
2. **Linux and power loss:** fsync the directory after creating a data file, run the torture suite on Linux, and cut a VM's power mid-run.
3. **Bit rot and recovery time:** on an invalid frame, look ahead for the next transaction id and refuse instead of truncating if found; add a periodic index checkpoint so `open` replays only the log after it.
