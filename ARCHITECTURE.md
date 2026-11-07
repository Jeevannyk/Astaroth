# Astaroth — Architecture

## 1. Overview

Astaroth is an embedded key-value database written in Go. It stores data in one append-only database file and rebuilds an in-memory index when the database opens.

Its main goals are:
- Store, read, update and delete key-value pairs.
- Recover the database after an interrupted process.
- Apply each batch completely or not at all.
- Return success only after the write has been synced.
- Support concurrent access, compaction and a configurable file-size limit.

## 2. High-level architecture

![Astaroth architecture](./diagrams/Architecture.drawio.png)

| Component | Source code | Responsibility |
|---|---|---|
| Database engine | `src/engine/` | Public operations, write ordering, locking and compaction |
| Storage | `src/storage/` | Header, frame encoding/decoding, file I/O and replacement |
| Index | `src/index/` | Maps each key to its latest value location in the file |
| Recovery | `src/recovery/` | Validates the log and rebuilds the index |
| Process lock | `src/lock/` | Prevents two processes from opening the same database |
| Failpoints | `src/failpoint/` | Creates controlled crash points for tests |
| CLI | `cmd/kvdb/` | Commands and the line-based server interface |
| Torture tool | `torture/` | Runs random workloads, kills and restarts the database process |
| Tests | `tests/` and package tests | Checks storage, recovery, batches, compaction and concurrency |

**Source of truth:** The database file is authoritative. The in-memory index is only an acceleration structure and can be rebuilt from the file.

## 3. Database file format

The file starts with a 32-byte header. Frames are appended after the header.

### 3.1 Header — 32 bytes

| Field | Size | Purpose |
|---|---:|---|
| Magic (`ASTAROTH`) | 8 bytes | Identifies an Astaroth database |
| Version | 4 bytes | Identifies the file-format version |
| Flags | 4 bytes | Reserved; currently zero |
| Generation | 8 bytes | Identifies the file generation; increases after compaction |
| Header CRC32C | 4 bytes | Checks the header contents |
| Reserved | 4 bytes | Reserved; currently zero |

The header uses little-endian encoding. The header checksum covers the first 24 bytes.

### 3.2 Frame layout

| Field | Size |
|---|---:|
| Frame magic | 4 bytes |
| Payload length | 4 bytes |
| Sequence number | 8 bytes |
| Operation count | 4 bytes |
| Payload | Variable |
| CRC32C | 4 bytes |

The CRC32C covers the frame header and payload, but not the CRC field itself. A frame is the unit of append and batch atomicity.

### 3.3 Operations

A payload contains one or more operations:

| Operation | Encoding |
|---|---|
| PUT | Type byte, key length (2 bytes), value length (4 bytes), key bytes, value bytes |
| DEL | Type byte, key length (2 bytes), key bytes |

Operation types are `1` for PUT and `2` for DEL. Lengths and numeric fields use little-endian encoding.

### 3.4 Limits

| Limit | Value |
|---|---:|
| Maximum key length | 1,024 bytes |
| Maximum value length | 1 MiB |
| Maximum operations per frame | 10,000 |
| Maximum frame payload | 64 MiB |

## 4. Write path and durability

A PUT, DEL or BATCH follows this sequence:

1. Validate the key, value and operation limits.
2. Acquire the database's exclusive lock.
3. Check that the database is writable and that the size limit allows the write.
4. Encode the operation or complete batch as one frame.
5. Write the frame at the current end of the database file.
6. Call `Sync()` on the database file.
7. Apply the operations to the in-memory index.
8. Advance the file end and next sequence number.
9. Return success to the caller.

The server sends `ACK` only after the database operation returns successfully. Therefore, an ACK means the write passed the file synchronization step.

If writing fails, the engine attempts to truncate the file back to its previous end. If that cleanup fails, the database is marked failed. If `Sync()` fails, the database is also marked failed rather than pretending the operation succeeded.

## 5. Batch atomicity

A batch is encoded into one frame, not a sequence of independent frames.

- The complete batch is validated before it is written.
- The batch is written with one append and one `Sync()` call.
- Recovery validates the frame before applying its operations.
- A valid frame is replayed as a whole; an incomplete or invalid trailing frame is not partly applied.
- The exclusive database lock prevents readers from observing an intermediate index update.
- Operations are applied in input order. The last operation for a key determines its final state.

This provides all-or-nothing recovery for a batch.

## 6. Index and read path

The index is an in-memory Go map:

`key → {record offset, value length}`

It stores locations, not the values themselves.

For `Get`, the engine:
1. Acquires a read lock.
2. Looks up the key in the index.
3. Reads the value bytes from the database file at the stored location.
4. Checks the record information and returns the value.

A PUT updates the key's index entry. A DEL removes the key from the index.

## 7. Recovery

When a database is opened, Astaroth:

1. Obtains the process lock.
2. Handles any interrupted compaction cleanup.
3. Opens the database file and checks its header.
4. Scans frames from the beginning of the log.
5. Checks frame magic, lengths, CRC32C, operation structure and sequence progression.
6. Replays valid frames into a new in-memory index.
7. Truncates and syncs a recognized incomplete trailing frame.
8. Returns the recovered database and recovery statistics.

A read error is reported; it is not treated as a torn write. Invalid data that cannot be explained as an interrupted final append is reported as corruption rather than being silently repaired.

Recovery is designed to be restartable. If the process is interrupted during recovery, the next open scans the persistent file again and repeats the required cleanup.

`check` performs validation without modifying the database. `open` performs recovery and may repair a recognized torn tail.

## 8. Compaction

The append-only log can contain old values and delete records. Compaction creates a smaller file containing only the current live values.

The compaction sequence is:

1. Acquire the exclusive database lock.
2. Create a temporary replacement file with the next generation number.
3. Copy only the latest live records into the new file.
4. Sync the temporary file.
5. Reopen and verify the temporary file using recovery and compare its logical contents.
6. Close the old database file and replace it with the verified temporary file.
7. Sync the containing directory where supported.
8. Reopen the replacement and update the engine's file and index state.

Before replacement, the original database remains authoritative. After the replacement, the higher-generation compacted file is authoritative. Startup cleanup handles temporary files left by an interrupted compaction.

## 9. Disk-full protection

The optional `MaxBytes` setting limits the database file size. A value of zero means no configured limit.

- A write is checked before append.
- A batch is checked as one complete frame; it is not partially accepted because of the limit.
- A rejected write returns a disk-full error and must not change the logical database state.
- Deletes remain possible using the reserved space described by the implementation.
- Compaction can reclaim space occupied by overwritten and deleted records, but it also needs space for its temporary replacement file.

The limit applies to the data file and the temporary compaction file as implemented; during compaction both files may exist at once.

## 10. Concurrency and process ownership

### In-process locking

The engine uses `sync.RWMutex`:
- Multiple readers can hold a read lock together.
- A write or compaction takes the exclusive lock.
- Readers cannot observe an index partway through a batch update.

### Cross-process locking

A lock file associated with the database prevents another process from opening the same database concurrently. Platform-specific implementations are in `src/lock/`.

## 11. CLI and server interface

The `kvdb` command provides:

| Command | Purpose |
|---|---|
| `open` | Open or create a database and run recovery |
| `put` | Store a value |
| `get` | Read a value |
| `del` | Delete a key |
| `batch` | Apply operations from a file or standard input as one batch |
| `dump` | Print all key-value pairs in key order |
| `check` | Validate the file without repairing it |
| `serve` | Accept line-based requests and return replies such as ACK, VALUE or ERR |

In `serve`, a batch is staged between `begin` and `commit`. Nothing from that batch is written before commit. An `ACK` is returned only after the operation is durable.

## 12. Testing strategy

Astaroth uses unit tests, integration tests, failpoint-driven process crashes and the torture executable.

- Storage tests check header and frame encoding, decoding and corruption handling.
- Engine tests check key-value operations, batch behaviour and file limits.
- Recovery tests check replay, truncation and interrupted recovery.
- Compaction tests check replacement and crash points around the swap.
- Concurrency tests check readers, writers and process termination.
- The torture tool repeatedly runs concurrent workloads, kills and restarts `kvdb`, then compares the recovered database with an oracle built from acknowledged replies.

The individual A1–F4 procedures, commands, file states, expected dumps and explanations are included in Section 16 below.

## 13. Correctness guarantees

### Durability

A successful database write returns only after the frame has been written and `Sync()` has succeeded. In server mode, `ACK` is sent after that success.

### Atomicity

A batch is one frame. Recovery validates the complete frame before applying its operations, so a batch is recovered as a whole or not at all.

### Recoverability

The append-only database file is the source of truth. Recovery scans and validates the log, rebuilds the index and repairs a recognized incomplete trailing frame. Recovery can be restarted after another process interruption.

## 14. Known limitations

- Process-kill tests do not prove behaviour under physical power loss.
- A successful `Sync()` is trusted; tests cannot prove that faulty hardware truly persisted the bytes.
- CRC32C detects accidental corruption but is not cryptographic authentication.
- Compaction takes the exclusive lock and can temporarily require space for a second copy of live data.
- Compaction thresholds are fixed in the current implementation.
- Performance measurements depend on the machine, storage device, operating system and file-cache state.
- Platform-specific file replacement and locking behaviour should be tested on each target operating system.

## 15. Related documentation

- `README.md` — build, CLI usage, serve protocol, torture tool and project tests.
- `DESIGN.md` — detailed design decisions and implementation reasoning.
- `BREAKING.md` — crash points and known weaknesses.
- `PROMPTS.md` — development prompt history.
- `diagrams/Architecture.drawio.png` — architecture diagram.

## 16. Scenario-to-mechanism proof

This table connects the challenge scenarios to the mechanism that protects the database and the corresponding test evidence. A test passing is evidence for the tested process-crash scenario; it is not proof of physical power-loss behaviour.

| Scenario | Failure or workload | Protection mechanism | Test evidence |
|---|---|---|---|
| A1–A2 | Crash before or during a frame write | Frame boundaries, length checks and CRC32C; recovery rejects an incomplete tail | `TestKillDuringWritePath` |
| A3 | Crash after write, before `Sync()` | Recovery accepts a complete valid frame if it survived; otherwise it ignores the incomplete tail | `TestKillDuringWritePath` |
| A4–A5 | Crash after `Sync()` or immediately after success | Sync-before-acknowledgement ordering | `TestKillDuringWritePath`, `TestKillImmediatelyAfterSuccess` |
| A6–A7 | Interrupted delete or large-value write | Delete is logged as a frame; incomplete frame is not replayed | Write-path crash tests and key/value tests |
| B1–B3 | Batch interrupted before/after its commit check or before sync | A batch is one frame; recovery validates it before replay | `TestKillDuringBatch` |
| B4 | Crash after batch acknowledgement | Batch is synced before success/ACK | `TestKillAfterBatchAcknowledged` |
| B5 | Repeated key operations in one batch | Operations are applied in input order | `TestBatch` / batch operation tests |
| B6 | Interrupted 10,000-operation batch | One-frame batch validation and recovery | `TestKillDuringTenThousandOpBatch` |
| C1–C2 | Recovery interrupted or torn tail cleanup interrupted | Recovery rescans the persistent log and repeats tail cleanup | `TestKillDuringRecovery`, `TestRecoveryKilledRepeatedly` |
| C3–C5 | Crash during temporary-file creation or replacement | Original file remains authoritative until replacement; generation and startup cleanup identify the active file | `TestKillDuringCompaction`, `TestKillWhileWritingTemporaryFile`, `TestReplacementInterruptedBetweenRemoveAndRename` |
| C6 | Close/reopen after an acknowledged write | Persisted log is replayed when reopened | `TestKillImmediatelyAfterSuccess` (not a dedicated crash-inside-`Close` test) |
| D1 | Concurrent writers and process termination | Exclusive writer lock, serialized append and recovery validation | `TestConcurrentWritersKilledAtRandom` |
| D2 | Concurrent writers updating a shared key | Serialized writes prevent torn or mixed records | `TestConcurrentWritersKilledAtRandom` |
| D3 | Concurrent batches, PUTs and DELs | Database lock and whole-frame batch application | `TestConcurrentWritersKilledAtRandom`, `TestReadersNeverSeePartialBatch` |
| E1 | File-size limit reached during PUT | Capacity checked before append; rejected write leaves file/state unchanged | `TestPutRejectedAtLimit` |
| E2 | File-size limit reached during batch | Entire batch capacity checked before append | `TestBatchRejectedAtLimit` |
| E3 | Reopen after reaching the limit | Recovery reads existing valid records independently of remaining write capacity | `TestRestartAfterReachingLimit` |
| E4 | Disk full during compaction | Original database is retained if temporary replacement cannot be completed | `TestCompactionFailsCleanlyAtLimit`, `TestDiskFullWhileWritingCompactedFile` |
| E5 | Delete, then write again | Delete reserve and compaction reclaim space for later writes | `TestDeleteThenWriteAgainWithoutManualCompaction` |
| F1–F2 | Repeated random workloads and recovery crashes | Repeated recovery and state checking against acknowledged operations | Torture tool tests and `TestRecoveryKilledRepeatedly` |
| F3 | One-million-key recovery performance | Sequential frame scan and index reconstruction | `TestRecoveryTimeOneMillionKeys` |
| F4 | Database truncated by one byte | Incomplete final frame is detected and removed during recovery | `TestEveryTruncationIsTorn`, truncation tests |

**Group D note:** The supplied implementation and test source define three concurrency sub-scenarios (D1–D3), all exercised by `TestConcurrentWritersKilledAtRandom`. There is no separate D4 requirement in the supplied project specification, so none is invented here.

## 17. Crash-test cases (A1–F4)

Run the commands in **PowerShell from the project root**. Follow each case in order: create the database, run the actual command, inspect the file, then dump/reopen it where a dump is available. Failpoint-triggered exits (commonly exit code 86) are expected. Reset `ASTAROTH_FAILPOINT` to `$null` after each failpoint command.


### Prepare shared input files

Run once from the project root before cases that use these files:

```powershell
New-Item -ItemType Directory -Force demo | Out-Null
1..10 | ForEach-Object { "put b$_ value$_" } | Set-Content demo\ten.txt -Encoding ascii
1..10000 | ForEach-Object { "put t$_ value$_" } | Set-Content demo\tenk.txt -Encoding ascii
$largeValue = "Z" * 1048576
"put big $largeValue" | Set-Content demo\big.txt -Encoding ascii
```

`ten.txt` and `tenk.txt` are batch input files. `big.txt` contains one PUT with a 1 MiB value. The CLI batch format is `put <key> <value>` or `del <key>`.

# Group A: Single write

## A1. Kill before any byte is written

**1. DB creation**
```powershell
Remove-Item demo\a1.db* -EA 0; .\kvdb put demo\a1.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-before-write"; .\kvdb put demo\a1.db k v; $env:ASTAROTH_FAILPOINT=$null
```

**3. File accessed — `demo\a1.db`**
- Contains the committed record `base = 1`.
- The new `k = v` write is interrupted before any byte is written.

**4. Dump**
```powershell
.\kvdb dump demo\a1.db
```
**Expected:** `base 1` only.

**5. Why?** The new record never entered the log. Recovery finds only the previously committed record.

## A2. Torn write (partial record)

**1. DB creation**
```powershell
Remove-Item demo\a2.db* -EA 0; .\kvdb put demo\a2.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-partial-write"; .\kvdb put demo\a2.db k v; $env:ASTAROTH_FAILPOINT=$null
```

**3. File accessed — `demo\a2.db`**
- Contains `base = 1`.
- A partial `k = v` record is appended before termination.

**4. Dump**
```powershell
.\kvdb dump demo\a2.db
```
**Expected:** `base 1` only.

**5. Why?** Recovery detects the incomplete final record and does not apply it.

For a partial frame-header test, use the failpoint `append-partial-header`.

## A3. Written, before fsync

**1. DB creation**
```powershell
Remove-Item demo\a3.db* -EA 0; .\kvdb put demo\a3.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-after-write"; .\kvdb put demo\a3.db k v; $env:ASTAROTH_FAILPOINT=$null
```

**3. File accessed — `demo\a3.db`**
- The new record is written.
- The process stops before `fsync`/`Sync()`.

**4. Dump**
```powershell
.\kvdb dump demo\a3.db
```
**Expected:** `base 1` and `k v`, or `base 1` only.

**5. Why?** Before sync, the new bytes may or may not survive. Durability is not yet guaranteed.

## A4. After fsync, before returning

**1. DB creation**
```powershell
Remove-Item demo\a4.db* -EA 0; .\kvdb put demo\a4.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-after-sync"; .\kvdb put demo\a4.db k v; $env:ASTAROTH_FAILPOINT=$null
```

**3. File accessed — `demo\a4.db`**
- The new record is written and synced.
- The process stops before returning success.

**4. Dump**
```powershell
.\kvdb dump demo\a4.db
```
**Expected:** `base 1` and `k v`.

**5. Why?** Sync completed before the crash, so the write must survive recovery.

## A5. Kill right after success

**1. DB creation**  
The Go test creates its own database.

**2. Actual command**
```powershell
go test ./tests -run '^TestKillImmediatelyAfterSuccess$' -v
```

**3. File accessed**  
The test's internally created database. Its temporary path is selected by the test.

**4. Dump**  
No separate dump command is provided by this test.

**5. Why?** It checks that a write acknowledged as successful survives immediate process termination and recovery.

## A6. Kill during a delete

**1. DB creation**
```powershell
Remove-Item demo\a6.db* -EA 0; .\kvdb put demo\a6.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-partial-write"; .\kvdb del demo\a6.db base; $env:ASTAROTH_FAILPOINT=$null
```

**3. File accessed — `demo\a6.db`**
- Contains `base = 1`.
- The delete record is only partially written.

**4. Dump**
```powershell
.\kvdb dump demo\a6.db
```
**Expected:** `base 1` remains.

**5. Why?** The incomplete delete is rejected, so the previous value remains.

To demonstrate a completed delete instead:
```powershell
$env:ASTAROTH_FAILPOINT="append-after-sync"; .\kvdb del demo\a6.db base; $env:ASTAROTH_FAILPOINT=$null
.\kvdb dump demo\a6.db
```
Expected: `base` is absent.

## A7. Kill during a 1 MB put

**1. DB creation**
```powershell
Remove-Item demo\a7.db* -EA 0; .\kvdb put demo\a7.db big old
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-partial-write"; .\kvdb batch demo\a7.db demo\big.txt; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\a7.db` — database containing `big = old`.
- `demo\big.txt` — input containing the 1 MB replacement value.

**4. Dump**
```powershell
.\kvdb dump demo\a7.db
```
**Expected:** `big old`.

**5. Why?** The replacement record is incomplete, so recovery keeps the previous value.

# Group B: Batch

## B1. Kill after 3 of 10 operations

**1. DB creation**
```powershell
Remove-Item demo\b1.db* -EA 0; .\kvdb put demo\b1.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-after-3-records"; .\kvdb batch demo\b1.db demo\ten.txt; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\b1.db` — database.
- `demo\ten.txt` — ten-operation input.

Only three operations are written before termination.

**4. Dump**
```powershell
.\kvdb dump demo\b1.db
```
**Expected:** `base 1` only.

**5. Why?** The batch is incomplete and is not applied during recovery.

## B2. All 10 written, no commit marker

**1. DB creation**
```powershell
Remove-Item demo\b2.db* -EA 0; .\kvdb put demo\b2.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-before-commit-marker"; .\kvdb batch demo\b2.db demo\ten.txt; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\b2.db` — database.
- `demo\ten.txt` — ten-operation input.

All ten operations are written, but the commit marker is not.

**4. Dump**
```powershell
.\kvdb dump demo\b2.db
```
**Expected:** `base 1` only.

**5. Why?** Recovery rejects the batch because its complete commit check is missing.

## B3. Marker written, before fsync

**1. DB creation**
```powershell
Remove-Item demo\b3.db* -EA 0; .\kvdb put demo\b3.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-after-write"; .\kvdb batch demo\b3.db demo\ten.txt; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\b3.db` — database.
- `demo\ten.txt` — ten-operation input.

The frame and its CRC are written, but sync has not completed.

**4. Dump**
```powershell
.\kvdb dump demo\b3.db
```
**Expected:** Either all ten batch operations or none; never a partial batch.

**5. Why?** The frame may or may not persist before sync, but recovery must preserve batch atomicity.

## B4. Kill after batch acknowledgement

**1. DB creation and server startup** — PowerShell window 1
```powershell
Remove-Item demo\b4.db* -EA 0; .\kvdb serve demo\b4.db
```

**2. Actual command** — enter in the server window
```text
b1 begin
b1 put b1 1
b1 put b2 2
b1 put b3 3
b1 commit
```
Wait for `ACK b1`. Then, in PowerShell window 2:
```powershell
Stop-Process -Name kvdb -Force
```

**3. File accessed — `demo\b4.db`**  
The server writes the three operations as a batch to this database.

**4. Dump**
```powershell
.\kvdb dump demo\b4.db
```
**Expected:**
```text
b1 1
b2 2
b3 3
```

**5. Why?** The server acknowledged the batch only after its write completed successfully. All three values must remain.

## B5. Put X and delete X in one batch

**1. DB creation and input file**
```powershell
Remove-Item demo\b5.db* -EA 0; "put x 1","del x","put y 2" | Set-Content demo\b5.txt -Encoding ascii
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-after-sync"; .\kvdb batch demo\b5.db demo\b5.txt; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\b5.db` — database.
- `demo\b5.txt` — three-operation batch: put x, delete x, put y.

**4. Dump**
```powershell
.\kvdb dump demo\b5.db
```
**Expected:** `y 2` only.

**5. Why?** The delete follows the put for `x`, so `x` is absent in the final state. `y` remains.

## B6. 10,000-operation batch killed during append

**1. DB creation**
```powershell
Remove-Item demo\b6.db* -EA 0; .\kvdb put demo\b6.db base 1
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="append-partial-write"; .\kvdb batch demo\b6.db demo\tenk.txt; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\b6.db` — database.
- `demo\tenk.txt` — input containing 10,000 PUT operations.

**4. Recovery and dump**
```powershell
.\kvdb open demo\b6.db; .\kvdb dump demo\b6.db
```
**Expected:** `base 1` only; recovery time should be below the stated test target.

**5. Why?** The interrupted batch is rejected as a whole, preserving the baseline record.

**Automated test:** `go test ./tests -run '^TestKillDuringTenThousandOpBatch$' -v`

# Group C: Recovery and compaction

## C1. Kill while replaying the log

**1. DB creation**
```powershell
Remove-Item demo\c1.db* -EA 0; 1..5 | % { .\kvdb put demo\c1.db "k$_" "v$_" }
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="recover-replay-frame:3"; .\kvdb open demo\c1.db; $env:ASTAROTH_FAILPOINT=$null
```

**3. File accessed — `demo\c1.db`**  
Contains five valid records. Recovery is interrupted while replaying frame 3.

**4. Recovery and dump**
```powershell
.\kvdb open demo\c1.db; .\kvdb dump demo\c1.db
```
**Expected:** `k1` through `k5` are present.

**5. Why?** The log remains intact. A later open can replay it again and rebuild the index.

## C2. Kill while truncating a torn tail

**1. DB creation and tail damage**
```powershell
Remove-Item demo\c2.db* -EA 0; .\kvdb put demo\c2.db base 1; Add-Content demo\c2.db -Value "JUNKJUNKJUNK" -NoNewline
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="recover-before-truncate"; .\kvdb open demo\c2.db; $env:ASTAROTH_FAILPOINT=$null
```

**3. File accessed — `demo\c2.db`**
- Contains valid `base = 1`.
- Has 12 extra junk bytes at the end.
- Recovery stops before truncating the invalid tail.

**4. Recovery and dump**
```powershell
.\kvdb open demo\c2.db; .\kvdb dump demo\c2.db
```
**Expected:** Recovery reports a 12-byte torn tail; `base 1` remains.

**5. Why?** The second recovery attempt detects the same invalid tail, retries cleanup and preserves the valid record.

## C3. Kill during compaction, before verification

**1. DB creation**
```powershell
Remove-Item demo\c3.db* -EA 0; 1..30 | % { .\kvdb put demo\c3.db key "v$_" }
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="compact-before-verify"; "compact" | .\kvdb serve demo\c3.db; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\c3.db` — original database, generation 1.
- Compaction temporary file — unfinished replacement.

**4. Recovery, check and dump**
```powershell
.\kvdb open demo\c3.db; .\kvdb check demo\c3.db; .\kvdb dump demo\c3.db
```
**Expected:** Generation 1 remains active; `key` has value `v30`.

**5. Why?** The temporary replacement was not verified or installed. The original file remains authoritative.

## C4. Kill immediately before rename

**1. DB creation**
```powershell
Remove-Item demo\c4.db* -EA 0; 1..30 | % { .\kvdb put demo\c4.db key "v$_" }
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="compact-before-rename"; "compact" | .\kvdb serve demo\c4.db; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\c4.db` — original generation 1.
- Compaction temporary file — prepared replacement.

**4. Recovery, check and dump**
```powershell
.\kvdb open demo\c4.db; .\kvdb check demo\c4.db; .\kvdb dump demo\c4.db
```
**Expected:** Generation 1 remains active; `key` has value `v30`.

**5. Why?** Rename did not happen, so the original database is still active.

## C5. Kill immediately after rename

**1. DB creation**
```powershell
Remove-Item demo\c5.db* -EA 0; 1..30 | % { .\kvdb put demo\c5.db key "v$_" }
```

**2. Actual command**
```powershell
$env:ASTAROTH_FAILPOINT="compact-after-rename"; "compact" | .\kvdb serve demo\c5.db; $env:ASTAROTH_FAILPOINT=$null
```

**3. Files accessed**
- `demo\c5.db` — database path, expected to contain generation 2 after replacement.
- Compaction temporary file — source of the replacement.

**4. Recovery, check, dump and list**
```powershell
.\kvdb open demo\c5.db; .\kvdb check demo\c5.db; .\kvdb dump demo\c5.db; Get-ChildItem demo\c5.db*
```
**Expected:** Generation 2, `key = v30`, and no leftover `.compact.tmp` file.

**5. Why?** Rename has already happened. Recovery must use generation 2 and finish the expected cleanup.

## C6. Acknowledged write survives close and reopen

**1. DB creation**  
Handled internally by the Go test; no separate setup command is supplied.

**2. Actual command**
```powershell
go test ./tests -run '^TestKillImmediatelyAfterSuccess$' -v
```

**3. File accessed**  
The test's internally created database. Its exact path is determined by the test.

**4. Dump**  
No separate dump command is supplied.

**5. Why?** This is a close/reopen persistence check for an acknowledged write. It is not a crash injected inside `Close()`, so it should not be presented as a dedicated close-crash test.

# Group D: Concurrency

## D1. Concurrent writers killed at random

**1. DB creation**  
The Go test creates its own database.

**2. Actual command**
```powershell
go test ./tests -run '^TestConcurrentWritersKilledAtRandom$' -v
```

**3. File accessed**  
The test-managed database used by concurrent writers. Its exact temporary path is determined by the test.

**4. Dump**  
No separate dump command is supplied.

**5. Why?** The test checks that concurrent writes and random process termination do not leave an unrecoverable database.

## D2. Readers never see a partial batch

**1. DB creation**  
The Go test creates its own database.

**2. Actual command**
```powershell
go test ./tests -run '^TestReadersNeverSeePartialBatch$' -v
```

**3. File accessed**  
The test-managed database used by concurrent readers and batch writers.

**4. Dump**  
No separate dump command is supplied.

**5. Why?** A reader must see the state before or after a batch, never an intermediate state containing only part of it.

## D3. Concurrent batches, PUTs and DELs

**1. DB creation**  
The Go test creates a temporary database and starts multiple writer goroutines.

**2. Actual command**
```powershell
go test ./tests -run '^TestConcurrentWritersKilledAtRandom$' -v
```

**3. File accessed**  
The test-managed database. The workload combines single PUTs, batches, concurrent deletes and updates to a shared key. The parent process kills the child at a random time.

**4. Dump**  
The test reopens the database and checks the recovered state against the operations acknowledged by each writer. It does not provide a separate CLI dump command.

**5. Why?** Acknowledged operations must remain present, and a batch must appear as one complete state rather than a mixture of two batches.

**Additional torture smoke test**
```powershell
.\torture.exe --iterations 30 --seed 5 --dir demo\tout --keep
.\kvdb dump demo\tout\torture.db
```
This is an additional randomized workload, not the D3 concurrency proof test.

# Group E: Disk full

## E1. Capacity limit reached during PUTs

**1. DB creation**  
The Go test creates a fresh temporary database and opens it with a 64 KiB limit.

**2. Actual command**
```powershell
go test ./tests -run '^TestPutRejectedAtLimit$' -v
```

**3. File accessed**  
The test-managed database file. It repeatedly writes 1 KiB values until the next PUT is rejected.

**4. Dump**  
The test checks the database directly; it does not provide a separate CLI dump command.

**Expected:** All acknowledged keys remain readable, the rejected key is absent, and the file never exceeds 64 KiB.

**5. Why?** The size check rejects a write before it changes the file or logical database state.

## E2. Capacity limit reached during a batch

**1. DB creation and input file**
```powershell
Remove-Item demo\e2.db* -EA 0; 1..32 | % { "put e$_ " + ("x"*40) } | Set-Content demo\e2.txt -Encoding ascii
```

**2. Actual command**
```powershell
.\kvdb put demo\e2.db base 1 --max-bytes 600; .\kvdb batch demo\e2.db demo\e2.txt --max-bytes 600
```

**3. Files accessed**
- `demo\e2.db` — database with a 600-byte limit.
- `demo\e2.txt` — input containing 32 PUT operations.

**4. Dump**
```powershell
.\kvdb dump demo\e2.db
```
**Expected:** `base 1` only.

**5. Why?** The batch is checked as a whole. If it exceeds the limit, none of its operations should be committed.

**Automated test:** `go test ./tests -run '^TestBatchRejectedAtLimit$' -v`

## E3. Reopen after reaching the capacity limit

**1. DB creation**  
The Go test creates a temporary database, fills it to the configured limit and closes it.

**2. Actual command**
```powershell
go test ./tests -run '^TestRestartAfterReachingLimit$' -v
```

**3. File accessed**  
The test-managed database file, reopened with the same configured capacity limit.

**4. Dump**  
The Go test checks that the previously acknowledged values remain readable. It does not provide a separate CLI dump command.

**5. Why?** A full database must still be able to reopen and recover its existing records.

## E4. Capacity limit reached during compaction

**1. DB creation**  
The Go tests create their own database and temporary compaction files.

**2. Actual command**
```powershell
go test ./tests -run '^(TestCompactionFailsCleanlyAtLimit|TestDiskFullWhileWritingCompactedFile)$' -v
```

**3. Files accessed**  
Test-managed database and compacted replacement files. Exact paths are chosen by the tests.

**4. Dump**  
No separate dump command is supplied.

**5. Why?** The tests check that running out of space during compaction does not destroy the original valid database.

**Automated tests:** `go test ./tests -run '^(TestCompactionFailsCleanlyAtLimit|TestDiskFullWhileWritingCompactedFile)$' -v`

## E5. Delete, then write again

**1. DB creation**  
The Go test creates a fresh temporary database, fills it to the configured 64 KiB limit, and records the acknowledged keys.

**2. Actual command**
```powershell
go test ./tests -run '^TestDeleteThenWriteAgainWithoutManualCompaction$' -v
```

**3. File accessed**  
The test-managed database. It deletes half of the existing keys, then writes new keys without requesting manual compaction.

**4. Dump**  
The test checks the database before and after reopening; no separate CLI dump command is provided.

**Expected:** Deleted keys stay absent, surviving keys retain their values, and new writes succeed. The test also checks that compaction advanced the file generation.

**5. Why?** Deletes free logical space. When needed, the implementation compacts obsolete records so later writes can fit without manual intervention.

# Group F: Repeated abuse

## F1. 500 iterations

**1. DB creation**  
The torture executable manages its own database.

**2. Actual command**
```powershell
.\torture.exe --iterations 500 --seed 12345
```

**3. File accessed**  
The database and workload files managed internally by the torture executable. No explicit output directory is supplied.

**4. Dump**  
No separate dump command is supplied.

**5. Why?** Repeated randomized workloads test whether database invariants continue to hold over many iterations.

**Expected output:**
```text
PASS: 500 of 500
```

## F2. Recovery crash mode

**1. DB creation**  
The torture executable manages its own recovery test files.

**2. Actual command**
```powershell
.\torture.exe --iterations 100 --seed 42 --mode recovery-crash
```

**3. File accessed**  
Internally managed recovery-crash database and test files. Exact paths depend on the executable.

**4. Dump**  
No separate dump command is supplied.

**5. Why?** Repeatedly interrupted recovery should remain restartable and must not permanently corrupt the database.

## F3. Recovery time for one million keys

**1. DB creation**  
The Go test prepares its own large database.

**2. Actual command**
```powershell
go test ./tests -run '^TestRecoveryTimeOneMillionKeys$' -v
```

**3. File accessed**  
The test-managed database containing one million keys. Its temporary path is determined by the test.

**4. Dump**  
No separate dump command is supplied.

**5. Why?** The test measures the time needed to rebuild the in-memory index from a large log.

**Expected:** The test reports its recovery timings; the stated target is a median below two seconds.

## F4. File truncated by one byte

**1. DB creation**
```powershell
Remove-Item demo\f4.db* -EA 0; 1..3 | % { .\kvdb put demo\f4.db "k$_" "v$_" }
```

**2. Actual command**
```powershell
$f=[IO.File]::Open("$PWD\demo\f4.db","Open","ReadWrite"); $f.SetLength($f.Length-1); $f.Close()
```

**3. File accessed — `demo\f4.db`**
- Opens the database for reading and writing.
- Removes exactly one byte from the end of the file.
- This makes the final frame incomplete.

**4. Recovery and dump**
```powershell
.\kvdb open demo\f4.db; .\kvdb dump demo\f4.db
```
**Expected:**
```text
k1 v1
k2 v2
```
`k3` should be absent.

**5. Why?** The last frame is incomplete after truncation. Recovery rejects that frame and preserves the preceding valid records.

