# Prompt log

Every prompt sent to an AI tool during the hackathon, in chronological order, unedited. Responses are not included.

- **Tool:** Claude Code (VS Code extension), model Claude Sonnet 5.5
- **Date:** 2026-11-06. Times are approximate (local time).
- Each prompt is in a code block exactly as typed, typos included. The one-line "Purpose" under each heading is a label added for the reader, not part of the prompt.

| # | Time | Purpose |
|---|---|---|
| 1 | ~12:40 | Read the problem statement; produce requirements and design, no code |
| 2 | ~12:55 | Create the initial Go project structure |
| 3 | ~13:05 | Phase 2: append-only log, put/get/delete, durability |
| 4 | ~14:05 | Tests use one permanent database file, data/astaroth.db |
| 5 | ~14:10 | Phase 3: indexing, recovery, kill scenarios, 1M-key recovery time |
| 6 | ~14:55 | Phase 4: atomic batches, transaction ids, commit marker |
| 7 | ~15:30 | Phase 5: concurrency, race detector, stress tests |
| 8 | ~16:00 | Phase 6: crash-safe compaction |
| 9 | ~16:35 | Phase 7: disk-full protection (file-size limit) |
| 10 | ~16:50 | Phase 8: CLI commands and the serve protocol |
| 11 | ~17:05 | Phase 9: the torture tool |
| 12 | ~17:20 | Continue |

---

## Prompt 1 — Requirements and design (~12:40)

Purpose: read the problem statement and deliver the design before any code.

```text
go through references directory there is a problem statement...go through it once...build whwtever you are implementing in the astaroth directory...


\You are a senior database storage-engine engineer.

I am participating in a 24-hour engineering hackathon called "Kill It and Prove It".

Your task is to help me build a key-value database from scratch that survives process termination at arbitrary instructions.

First, understand the entire problem statement and its requirements.

We will use:

Go

Go standard library only

Append-only log storage

In-memory hash index

File-based persistence

IMPORTANT RULES:

Do not implement the entire project immediately.

Do not use SQLite, LevelDB, RocksDB, BoltDB or any database/storage-engine library.

Do not copy existing database implementations.

Do not introduce unnecessary abstractions.

Every implementation decision must prioritize crash safety.

Explain the reasoning behind important storage decisions.

Every phase must have working tests before proceeding.

First deliver:

Complete requirement breakdown.

Proposed architecture.

On-disk binary format with field sizes.

Write ordering and fsync strategy.

Batch atomicity strategy.

Crash recovery strategy.

Compaction strategy.

Disk-full handling strategy.

Concurrency strategy.

Potential failure points and their solutions.

Identify any design decisions that could violate the hackathon's correctness invariant.

Do not write implementation code yet.
```

---

## Prompt 2 — Project structure (~12:55)

Purpose: create the Go module skeleton with open/close only.

```text
create the initial Go project structure. Requirements: Create a clean Go module. Separate database engine, storage, recovery, indexing, concurrency and CLI components. Create a testing directory. Create a torture testing directory. Create documentation files. Suggested structure: team-name/ go.mod README.md ARCHITECTURE.md BREAKING.md PROMPTS.md src/ cmd/ torture/ tests/ diagrams/(also refer the original problem statement pdf once for file structure..) Keep the architecture simple. Implement only:Database initialization. Database opening. Database closing. File creation. Basic error handling. Do not implement the complete database yet. Write unit tests and verify that opening and closing the database works correctly.
```

---

## Prompt 3 — Phase 2: storage engine and log format (~13:05)

Purpose: implement put/get/delete over an append-only log with durable writes.

```text
PHASE 2 — Storage engine and binary log format
Implement the core storage engine using an append-only log.
Requirements:
1. Store keys and values as byte strings.
2. Support keys from 1 byte to 1024 bytes.
3. Support values up to 1 MB.
4. Use a well-defined binary record format.
5. Include record type, key length, value length and checksum.
6. Include enough framing information to detect incomplete writes.
7. Handle overwrites correctly.
8. Handle deletes using tombstone records.
Implement:
put(key, value)
get(key)
delete(key)
IMPORTANT:
A successful write must be durable.
Use the appropriate Go file synchronization operation.
Never return success before the required data has been synchronized to disk.
For now, implement single-operation durability.
Write tests for:
Normal insertion.
Updating existing keys.
Retrieving missing keys.
Deleting existing keys.
Deleting missing keys.
Large values.
Invalid key lengths.
Process restart and data persistence.
Explain the on-disk record format in README.md.
also update prompt.md with proper formatting...
```


---

## Prompt 4 — Permanent database file for tests (~14:05)

Purpose: make tests use one persistent data/astaroth.db instead of temp files.

```text
Right now the tests create temporary database files with t.TempDir() and delete them after every run, so I never see a real database file. Change this so the project uses ONE permanent database file, data/astaroth.db in the project root, that is never deleted and still exists after every run.

1. Make data/astaroth.db the permanent file. Create the data/ directory if missing (os.MkdirAll). Add a small shared test helper that returns this path, and use it in the tests in tests/ and cmd/kvdb that currently use t.TempDir() for their main database.
2. Never delete or truncate this file in any test or cleanup. Tests must close the database (release the lock) but leave the file on disk.
3. Since the file now keeps data between runs, tests must not assume an empty database. Use unique keys per run (for example a key prefix with the current timestamp) and assert only on those keys, not on the total key count.
4. Leave the exhaustive truncation test, the crash and corruption tests, and the unit tests that deliberately damage or copy files on temp dirs. They destroy data on purpose and must not touch the permanent file.
5. Do not change the engine, storage, recovery or index code or the on-disk format.
6. Add data/ to .gitignore. Update the Test section of README.md to say where the permanent file is.
7. Run go test -short ./... twice, then show me the file path, its size after each run (it must grow), and the output of .\kvdb.exe open data\astaroth.db with its keys= count.
8. Append this prompt, as typed, to PROMPTS.md in the existing format.

Line 52: "A single-process, embedded key-value database. It runs as one program, stores everything in one file on disk…"
Lines 102–103: "Exactly one data file on disk. A separate lock file or a temporary file used during compaction is allowed, but the authoritative state must live in one file."

FOLLOW THIS
```


---

## Prompt 5 — Phase 3: in-memory indexing and recovery (~14:10)

Purpose: robust recovery, kill-scenario tests, and measured 1,000,000-key recovery time.

```text
PHASE 3 — In-memory indexing and recovery
Implement an in-memory hash index for fast key lookup.
Requirements:
1. The index must map keys to their latest valid values or log offsets.
2. Reconstruct the index when the database opens.
3. Replay valid log records in the correct order.
4. Ignore incomplete or torn trailing records.
5. Never return partially written values.
6. Handle deleted keys correctly.
7. Recovery must be idempotent.
Implement robust recovery.
Test these scenarios:
Kill before writing a record.
Kill during a partial record write.
Kill after a complete write but before fsync.
Kill after fsync but before returning success.
Kill immediately after returning success.
Kill during recovery.
Restart after recovery was interrupted.
The database must never lose an acknowledged operation.
Optimize recovery to support at least 1,000,000 keys within 2 seconds on the judging laptop.
Measure actual recovery time. Do not claim the target is achieved without benchmarking it
```


---

## Prompt 6 — Phase 4: atomic batch operations (~14:55)

Purpose: atomic batch with transaction ids and a commit marker, batch crash tests, documented write ordering.

```text
PHASE 4 — Atomic batch operations
Implement atomic batch operations.
Function:
batch([]Operation)
Supported operations:
Put
Delete
Requirements:
1. A batch must be fully committed or fully absent after a crash.
2. Partial batches must never become visible.
3. Use transaction identifiers.
4. Implement a commit marker.
5. Use checksums to validate records and commit information.
6. Ensure correct synchronization ordering.
7. Acknowledge a batch only after it is durable.
8. Ensure readers never observe partially applied batches.
Test:
Kill after 3 of 10 operations.
Kill after all records but before the commit marker.
Kill after the commit marker but before fsync.
Kill after successful batch acknowledgement.
Batch containing repeated keys.
Batch containing put followed by delete of the same key.
Batch containing 10,000 operations.
Document the exact ordering of writes, fsync calls and commit markers.
Do not sacrifice crash safety for speed.
```


---

## Prompt 7 — Phase 5: concurrency (~15:30)

Purpose: concurrent writers and readers, race detector, stress tests.

```text
PHASE 5 — Concurrency
Implement concurrency support.
Requirements:
At least two concurrent writers.
Concurrent put operations.
Concurrent delete operations.
Concurrent batch operations.
Readers must be able to call get during writes.
A single global mutex is acceptable.
Guarantee:
1. No corrupted records.
2. No mixed values from concurrent writers.
3. No partially visible batches.
4. No race conditions in the in-memory index.
5. Every acknowledged operation remains durable.
Use Go synchronization primitives.
Run:
go test -race ./...
Fix all detected data races.
Create stress tests involving multiple concurrent writers and readers.
```


---

## Prompt 8 — Phase 6: crash-safe compaction (~16:00)

Purpose: compaction through a temporary file and an atomic replace, with kill tests at every step.

```text
PHASE 6 — Crash-safe compaction
Implement crash-safe database compaction.
The database must not grow forever.
Compaction must remove obsolete records and deleted keys.
Requirements:
1. Build a compacted representation of the current database state.
2. Write it to a temporary file.
3. Synchronize the temporary file.
4. Atomically replace the old authoritative file using a safe rename strategy.
5. Synchronize the containing directory where supported and required.
6. Never destroy the old authoritative database before the new one is safely prepared.
7. Recover correctly if the process is killed during any compaction step.
8. Clean up or ignore abandoned temporary files during recovery.
Test:
Kill before compaction starts.
Kill while writing the temporary file.
Kill after writing but before synchronization.
Kill immediately before rename.
Kill during the replacement sequence.
Kill after rename but before temporary-file cleanup.
After every scenario, verify that the database opens and all acknowledged data remains intact
```


---

## Prompt 9 — Phase 7: disk-full protection (~16:35)

Purpose: configurable file-size limit, clean errors, no partial batches, recovery of space by delete and compaction.

```text
PHASE 7 — Disk-full protection
Implement configurable maximum database file size.
Example:
Open(path, maxBytes)
Requirements:
1. Reject writes that would exceed the configured limit.
2. Return a clear error.
3. Never corrupt previously acknowledged data.
4. Handle disk-full conditions during individual writes.
5. Handle disk-full conditions during batches.
6. Handle disk-full conditions during compaction.
7. Allow the database to become usable again after deletes and successful compaction.
Test:
File limit reached during put.
File limit reached during batch.
File limit reached during compaction.
Restart after reaching the limit.
Delete records, compact and write again.
IMPORTANT:
Never partially commit a batch because the file-size limit was reached.
```


---

## Prompt 10 — Phase 8: CLI implementation (~16:50)

Purpose: every mandatory kvdb command, the serve protocol, binary-safe text encoding, integration tests.

```text
PHASE 8 — CLI implementation
Implement the mandatory command-line interface.
The following commands must work:
./kvdb open [--max-bytes N]
./kvdb put
./kvdb get
./kvdb del
./kvdb batch
./kvdb dump
./kvdb check
./kvdb serve
Requirements:
Follow the exact expected behaviour in the hackathon specification.
Print clear error messages.
Implement binary-safe storage internally.
Make serve mode accept operations through stdin.
Print ACK only after successful durable completion.
Never print an ACK for a failed operation.
Ensure the judge can run everything using README instructions.
Add integration tests for every command.
```


---

## Prompt 11 — Phase 9: the torture tool (~17:05)

Purpose: sent while Phase 8 was being finished; asks for the torture tool once Phase 8 is complete.

```text
run this after completetion of phase 8
PHASE 9 — Build the torture testing tool
This is one of the most important components of the hackathon.
Build an independent torture testing tool.
It must:
1. Start the database in serve mode as a child process.
2. Generate random put, delete and batch operations.
3. Support at least two concurrent writers.
4. Maintain an independent oracle log.
5. Record every successfully acknowledged operation.
6. Kill the database using SIGKILL at genuinely random times.
7. Restart the database.
8. Run check and dump.
9. Compare actual database state against the independent oracle.
10. Report missing acknowledged writes, partially committed batches and unexpected values.
Support:
--iterations 500
--seed 12345
Required modes:
normal
large-values
large-batches
recovery-crash
compaction
disk-full
IMPORTANT:
The oracle must never depend on the database's own dump or internal index.
The random seed must reproduce failures.
The tool must kill the database during operations, not merely between operations.
Run at least 500 consecutive kill-and-restart iterations.
Stop immediately on the first correctness violation and print:
Expected state.
Actual state.
Operation IDs.
Random seed.
Failure scenario.
```

---

## Prompt 12 — Continue (~17:20)

Purpose: resume the work after an interruption.

```text
continue
```
