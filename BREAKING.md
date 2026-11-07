# How I would break this

Line numbers refer to this commit. `ARCHITECTURE.md` explains why the design works; this document looks at where it could fail.

## 1. Crash points

| # | Process dies at | On disk | Next `open()` |
|---|---|---|---|
| 1 | `src/engine/engine.go:338` — frame encoded, before `WriteAt` (`:364`) | Only previous frames | Replays previous frames. The operation is missing because it was never acknowledged. |
| 2 | `engine.go:364` — during `WriteAt` | Only part of the frame may be written: header, records, or everything except the 4-byte commit marker | `scanLog` (`src/recovery/recovery.go:165`) detects an invalid frame through its length or CRC. `Recover` truncates the file to the last valid frame (`:87`) and syncs it (`:91`). |
| 3 | `engine.go:370` — while rolling back a failed `WriteAt` due to disk full | A partial frame may remain | Same recovery process as row 2. |
| 4 | `engine.go:378-379` — after `WriteAt`, before or during `Sync` | Complete frame may still be in the OS page cache | The process kill does not clear the OS cache. If the CRC is valid, recovery replays the frame, even though it was never acknowledged. This is allowed. |
| 5 | `engine.go:387` — after `Sync`, before `index.Apply` (`:388`) | Complete frame saved to disk | Recovery replays it. The operation is present even though no reply was sent. |
| 6 | `cmd/kvdb/serve.go:207` — write returned successfully, but `ACK` (`:209`) was not printed | Complete frame saved to disk | The operation is present, although the client never received an ACK. This is allowed. |
| 7 | `recovery.go:229` → `src/storage/file.go:26-29` — while creating a new file header | File is empty or contains less than 32 bytes of the header | `recoverTornStart` (`recovery.go:216`) writes the header again. No frame exists yet. |
| 8 | `recovery.go:186` — during `scanLog` replay | Database file is unchanged because replay only reads | Recovery starts again and produces the same result. |
| 9 | `recovery.go:87-91` — between truncating an invalid tail and syncing | Invalid tail may be removed or still present | Recovery runs again and either removes it or finds nothing. The result stays the same. |
| 10 | `src/engine/compact.go:159` — during `writeCompacted` | Old file is untouched; temporary file may be incomplete | `CleanupCompaction` deletes the temporary file (`src/recovery/compaction.go:41`). The old file is recovered. |
| 11 | `compact.go:183-185` — after temporary file is synced and verified, but before `ReplaceFile` (`:186`) | Old file and complete temporary file both exist | Same as row 10. The old database is recovered, and only compaction is lost. |
| 12 | `compact.go:186` — during `ReplaceFile` (`MoveFileExW` / `rename(2)`) | Either the old file remains or the new file replaces it | Recovery handles either state. If replacement leaves no data file, `compaction.go:45` renames the complete temporary file into place. |
| 13 | `compact.go:190-197` — after replacement, before directory sync or reopen | New compacted file (generation + 1) is in place | Normal recovery of the compacted file. |
| 14 | `compaction.go:40-45` — while `open` removes or renames the temporary file | Old file may remain, or there may be no data file but a complete temporary file | The next `open` repeats the cleanup or rename. |
| 15 | `engine.go:407-414` — during `Close` | Nothing new is written. Every acknowledged frame is already synced. | Same state as killing the process just before `Close`. The OS releases the lock. |

## 2. The ordering argument

Every write follows a fixed order:

1. **Write:** The entire frame is written using one `WriteAt` operation (`pwrite` on Unix or `WriteFile` at an offset on Windows).
2. **Sync:** `Sync` (`fsync` on Unix or `FlushFileBuffers` on Windows) saves it to disk.
3. **Update index:** The in-memory hash index is updated.
4. **Return:** The write operation completes, and `serve` prints `ACK`.

Therefore, receiving an ACK means the frame has already been synced to disk.

The last 4 bytes of each frame contain a CRC32C checksum calculated from its header and records. This checksum also acts as the commit marker. If any covered byte is missing or corrupted, validation fails.

Because the checksum verifies the entire frame, we do not need a separate sync between records and the marker. A batch is stored as one frame, so recovery accepts or rejects the entire batch.

During recovery, Astaroth stops at the first invalid frame and uses `ftruncate` (`SetEndOfFile` on Windows) to remove everything after it before accepting new writes.

A single lock ensures writes happen one at a time. Each frame is synced before the next write starts, so only the last frame can be partially written during a crash.

If `fsync` fails, Astaroth does not retry. The OS may have lost the data, and retrying could incorrectly report success. Instead, the database refuses further writes.

**Compaction safety**

Compaction creates `<path>.compact.tmp`, writes the compacted data, syncs it, and verifies it using the recovery code. It then replaces the old database file with one operation:

- **Windows:** `MoveFileExW` with `MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH`.
- **Unix:** `rename(2)`.

The old file is never modified during compaction. Whichever file survives the replacement is complete.

**Directory fsync:** On Unix, Astaroth syncs the directory after renaming (`SyncDir`). This is necessary because syncing the file alone does not guarantee that the rename survives a power failure.

Windows does not provide directory fsync through `os`. Instead, `MOVEFILE_WRITE_THROUGH` waits until the rename is written to disk.

A process kill does not require directory syncing because the OS continues running and maintains its filesystem state.

Astaroth does **not** sync the directory after creating a new database file. See Section 3.

## 3. Known weaknesses

- **Real kills never tore a write:** On Windows, a kill happens between system calls, not inside `WriteFile`. In over 1,000 torture kills, recovery never removed a torn tail. Rows 2 and 3 are verified through tests that simulate partial writes and cut a log at every byte.

- **Torture seeds are not perfectly reproducible:** The seed controls kill delays, but Go's random map order affects which keys some delete operations choose. Later iterations also depend on which operations survived previous crashes. Failures include a complete `oracle.log`, but rerunning the same seed may not reproduce them.

- **Bit rot can cause data loss:** If corruption occurs in the middle of a file and less than 64 MiB follows it, recovery treats it as a torn tail and removes everything after it. The CRC detects corruption, but distinguishing bit rot from a torn tail requires checking ahead for a valid frame with the next transaction ID.

- **Power loss is not tested:** On Unix, the directory entry of a newly created database file is not synced. A power failure after the first acknowledged writes could therefore lose the entire file. The Unix code (`flock`, `rename(2)`, `SyncDir`) compiles but has never been run.

- **Recovery slows as the database grows:** Recovery takes about 0.3 seconds for 1 million keys, plus around 0.5 seconds per GB (measured up to 1 GB). A few GB of live data could exceed 2 seconds.

- **One lock limits performance:** `Get` waits while a write is syncing. Compaction blocks all operations and needs enough disk space for a second copy. `MaxBytes` limits the data file and temporary file separately. A batch must fit within 64 MiB, so 10,000 operations of 1 MB each will not fit. If the file exceeds a lowered `MaxBytes` limit, even deletes are rejected.

## 4. What we tried that failed

- **Separate commit-marker record:** We considered writing records, syncing, writing a marker, and syncing again. We dropped this design in favour of the CRC trailer because it needs only one sync and has fewer crash scenarios.

- **Recovery speed improvements:** The first version took 0.56–0.63 seconds for 1 million keys. Disabling garbage collection during replay gave no meaningful improvement, so we dropped it. One optimisation initially appeared slower because of machine performance variation. We then started alternating A/B builds to get more reliable comparisons.

- **Tests that could not fail:** We deliberately broke the code and found tests that still passed:
  - The reader-isolation test checked keys in the wrong order.
  - The disk-full rollback test checked the file only after recovery had cleaned it.
  - The delete-reserve test always left extra space.
  - A compaction kill test expected 3 frames when there were only 2. Its failpoint check correctly caught this.

  We fixed these testing problems.

- **Bugs discovered and fixed:** `serve` rejected the first command piped from Windows PowerShell because PowerShell adds a byte-order mark. A bare unknown word in `serve` was treated as an ID. Two test packages used the same test database, causing a race. The stress test also had a data race in its own failure flag.

  We also caught a design issue: compacting after encoding a frame during the disk-full check could give the frame an outdated transaction ID. The check now runs before encoding.

- **Torture testing:** The torture tool found no violations in the engine. However, when tested against deliberately broken builds, it detected every issue in the first iteration:
  - ACK sent before fsync.
  - One frame written per batch operation.
  - Recovery skipping frames.

  Its disk-full mode initially failed to reach the actual storage limit, so we adjusted it.

## 5. If we had 24 more hours

1. **Reproducible torture failures and real torn writes:** Generate every operation using only `(seed, iteration, writer, index)`. Write large frames using multiple `WriteAt` calls so a `SIGKILL` can happen between them.

2. **Linux and power-loss testing:** Sync the directory after creating a database file, run the torture suite on Linux, and cut a VM's power during execution.

3. **Bit rot and faster recovery:** When an invalid frame is found, check ahead for the next transaction ID. If a valid later frame exists, report an error instead of silently truncating. Also add periodic index checkpoints so `open` only needs to replay the log after the latest checkpoint.