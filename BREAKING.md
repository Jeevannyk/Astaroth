# How We Try to Break Astaroth

This document explains how we test Astaroth against unexpected crashes, how recovery works, and what limitations remain.

Our goal is simple: **Previously acknowledged data must survive a process crash, and incomplete operations must never be treated as valid data.**

## 1. Crash Scenarios

| No. | Crash Point | Expected Recovery |
|---|---|---|
| 1 | Before writing a frame | Previous data remains unchanged. |
| 2 | During frame writing | Incomplete frame is detected and removed. |
| 3 | During failed-write rollback | Any remaining partial data is removed. |
| 4 | After writing but before Sync completes | A valid frame may be recovered, even without ACK. |
| 5 | After Sync but before index update | Recovery restores the saved operation. |
| 6 | Before sending ACK | Data remains saved, although the client receives no confirmation. |
| 7 | During database file creation | An incomplete header is repaired during startup. |
| 8 | During recovery | Recovery can safely run again. |
| 9 | During incomplete-frame cleanup | Cleanup can be repeated safely. |
| 10 | During temporary file creation | Original database remains safe. |
| 11 | After temporary file Sync but before replacement | Original database remains available. |
| 12 | During atomic file replacement | Either the old or new complete file should remain available. |
| 13 | After file replacement | New compacted database is recovered normally. |
| 14 | During temporary-file cleanup | Cleanup can be repeated during startup. |
| 15 | During database Close | Previously acknowledged data remains safe. |

## 2. Why Our Write Process Is Safe

Every write follows this order:

1. Encode the operation into a frame.
2. Write the frame to the database file.
3. Sync the file to storage.
4. Update the in-memory hash index.
5. Return success.
6. Send ACK to the client.

**Important: ACK is sent only after Sync succeeds.**

Each frame contains:
- Header: Information about the frame.
- Records: Actual database operations.
- CRC32C: Checksum used to detect incomplete or corrupted frames.

During recovery, Astaroth validates each frame. If the final frame is incomplete or invalid, recovery removes it using file truncation.

A lock ensures that only one writer modifies the database at a time.

### Unacknowledged Writes

If a write does not receive an ACK, recovery may find the complete operation or no operation at all.

A partially written operation must never be restored as valid data.

### Batch Atomicity

All operations in a batch are stored in one frame.

During recovery, the entire batch is either restored or discarded. Partial batch execution is not allowed.

## 3. Safe Compaction

Compaction creates a new database file containing the latest data without directly modifying the original file.

1. Create a temporary file.
2. Write the latest database state.
3. Sync the temporary file.
4. Verify the temporary file.
5. Atomically replace the original file.
6. Sync the directory on Unix systems.

If the process crashes before replacement, the original database remains available.

If it crashes after replacement, the new database can be recovered.

## 4. Known Limitations

- More than 1,000 torture kills did not produce a real torn write.
- Some torture test failures are difficult to reproduce exactly.
- Mid-file corruption may cause valid data after the damaged frame to be removed.
- Power failure scenarios have not been fully tested.
- Unix implementation has not yet been tested in a real Unix environment.
- Recovery time increases with database size.
- A single writer lock and one Sync per write affect performance.
- Compaction requires additional temporary disk space.
- Large batches are limited to 64 MiB.

## 5. What We Tried

- Replaced a separate commit-marker record with a CRC32C checksum.
- Tested recovery performance and rejected an ineffective garbage-collector optimization.
- Fixed incorrect tests involving disk-full handling, reader isolation and compaction.
- Fixed PowerShell input, command parsing and test race-condition bugs.
- Tested deliberately broken implementations.

Our torture tool detected deliberately introduced problems such as:
- Sending ACK before Sync.
- Writing each batch operation as a separate frame.
- Skipping frames during recovery.

**No integrity violation was found in the engine during our tested runs.**

## 6. What We Would Improve With 24 More Hours

1. **Better crash testing:** Make torture tests reproducible and improve torn-write testing.
2. **Power failure testing:** Run the complete test suite on Linux and simulate sudden VM power failures.
3. **Better corruption detection:** Distinguish incomplete final frames from corruption in the middle of the database.
4. **Faster recovery:** Introduce periodic index checkpoints so recovery does not need to replay the entire database file.

## 7. Final Summary

Astaroth follows one critical rule:

**Never acknowledge a write before its data has been synced to storage.**

Our design combines:
- Append-only log storage.
- CRC32C validation.
- Recovery through log replay.
- An in-memory hash index.
- Atomic file replacement during compaction.
- Crash and torture testing.

Our testing has not revealed an integrity violation, but power-loss handling, corruption detection and performance still need improvement.

**We do not claim that Astaroth can never fail. We aim to ensure that crashes do not silently destroy previously acknowledged data.**