# Astaroth

A crash-safe key-value database written from scratch in Go. It uses an append-only log, an in-memory hash index, CRC32C checksums and `fsync` to provide durable writes and atomic batches.

## 1. Build

**Requirement:** Go 1.22 or later. No external dependencies.

Run from the repository root:

```powershell
go build -o . ./cmd/kvdb ./torture
```

Creates `kvdb.exe` and `torture.exe` in the project root.

## 2. Database commands

| Command | Purpose |
|---|---|
| `open` | Create/open database and perform recovery |
| `put` | Insert or update a key |
| `get` | Read a key |
| `del` | Delete a key |
| `batch` | Execute multiple operations atomically |
| `dump` | Display all stored key-value pairs |
| `check` | Verify database integrity |
| `serve` | Run the database as a long-running process |

### A. Open / Create

```powershell
.\kvdb.exe open demo.db
```

Expected output for a new database:

```text
keys=0 recovery_ms=2.254 frames=0 torn_tail_bytes=0
```

Creates the database, validates its header and performs recovery.

### B. Put

```powershell
.\kvdb.exe put demo.db name astaroth
```

Expected output:

```text
OK
```

Stores `name = astaroth`. The operation is acknowledged only after the file is synced.

### C. Get

```powershell
.\kvdb.exe get demo.db name
```

Expected output:

```text
astaroth
```

For a missing key:

```powershell
.\kvdb.exe get demo.db missing
```

Expected output:

```text
NOTFOUND
```

### D. Delete

```powershell
.\kvdb.exe del demo.db name
```

Expected output:

```text
OK
```

Appends a delete record. Deleting a missing key is also valid.

### E. Batch

Create a batch file:

```powershell
Set-Content ops.txt "put a 1", "put b 2", "del a", "put c three words"
```

Execute:

```powershell
.\kvdb.exe batch demo.db ops.txt
```

Expected output:

```text
OK 4 operations
```

All four operations are stored in one transaction frame. Either the entire batch commits or none of it does.

### F. Dump

```powershell
.\kvdb.exe dump demo.db
```

Expected output after the above operations:

```text
b 2
c three words
```

Displays all current keys, sorted by key.

### G. Check

```powershell
.\kvdb.exe check demo.db
```

Expected output:

```text
OK keys=2 frames=4 bytes=...
```

Checks the header, frame checksums, transaction sequence and records without modifying the database. The byte count depends on the actual file.

## 3. Serve and manual crash test

Start the database:

```powershell
.\kvdb.exe serve demo.db
```

Expected:

```text
READY keys=0 recovery_ms=2.635 frames=0 torn_tail_bytes=0
```

Type these commands into the running process:

```text
put a 1
put b 2
b1 begin
b1 put x 10
b1 put y 20
b1 commit
b2 begin
b2 put never 1
```

Expected acknowledgements:

```text
ACK 1
ACK 2
ACK b1
```

The second batch has not been committed.

Open another PowerShell window and kill the process:

```powershell
Stop-Process -Name kvdb -Force
```

Now verify recovery:

```powershell
.\kvdb.exe check demo.db
.\kvdb.exe dump demo.db
```

Expected database contents:

```text
a 1
b 2
x 10
y 20
```

**Why:** Acknowledged writes and the committed batch survive. The uncommitted batch is absent.

## 4. Torture testing

The torture tool launches `kvdb.exe serve` as a child process, sends concurrent workloads, kills it, restarts it and compares the recovered database against its independent in-memory oracle.

Run each mode from the project root.

### A. Normal mode

```powershell
.\torture.exe --iterations 500 --seed 12345 --mode normal
```

**Tests:** Random PUT, DELETE, shared-key operations and batches.

**Expected:** All iterations pass without acknowledged data loss or partial transactions.

### B. Large values

```powershell
.\torture.exe --iterations 100 --seed 999 --mode large-values
```

**Tests:** Values ranging up to 1 MiB, including binary data.

**Expected:** Large values are recovered correctly after process termination.

### C. Large batches

```powershell
.\torture.exe --iterations 100 --seed 7 --mode large-batches
```

**Tests:** Large transactions containing up to 10,000 operations.

**Expected:** Each batch is recovered completely or not at all.

### D. Recovery crash

```powershell
.\torture.exe --iterations 100 --seed 42 --mode recovery-crash
```

**Tests:** Preloads 200,000 keys and kills the database during recovery.

**Expected:** Repeated recovery eventually completes without losing previously committed data.

### E. Compaction

```powershell
.\torture.exe --iterations 100 --seed 7 --mode compaction
```

**Tests:** Terminates the database while compaction is in progress.

**Expected:** Recovery uses a valid old or new database file, without losing acknowledged data.

### F. Disk full

```powershell
.\torture.exe --iterations 100 --seed 42 --mode disk-full
```

**Tests:** Writes against a 128 KiB database-size limit.

**Expected:** Writes exceeding the limit are rejected cleanly. Previously acknowledged data remains intact.

### Additional options

```text
--writers N       Number of concurrent writers (default 3)
--kvdb PATH       Path to kvdb executable
--dir PATH        Working directory for test files
--keep            Keep automatically created directory after success
--max-bytes N     Override database size limit
```

**Expected result:** Each iteration reports its result, followed by a final PASS or FAIL. Exit code `0` means all iterations passed.

A fixed seed makes the workload reproducible to a degree, but does not guarantee identical operating-system timing.

## 5. On-disk format

The database uses one file. All integers are little-endian.

```text
DATABASE FILE
+-----------------------------------+
| Header (32 bytes)                 |
| Magic | Version | Generation     |
| Header CRC32C                     |
+-----------------------------------+
| Frame 1                           |
| Magic | Length | Sequence         |
| Operation Count                   |
| Records                           |
| Frame CRC32C                      |
+-----------------------------------+
| Frame 2                           |
| Magic | Length | Sequence         |
| Operation Count                   |
| Records                           |
| Frame CRC32C                      |
+-----------------------------------+
|               ...                 |
+-----------------------------------+
```

**Header (32 bytes):**
- Magic: `ASTAROTH`
- Version and flags
- Generation number
- CRC32C checksum

**Transaction frame:**

| Field | Size |
|---|---:|
| Frame magic | 4 bytes |
| Payload length | 4 bytes |
| Sequence number | 8 bytes |
| Operation count | 4 bytes |
| Records | Variable |
| CRC32C | 4 bytes |

**Records:**
- PUT: type, key length, value length, key and value.
- DELETE: type, key length and key.

The CRC32C at the end validates the entire frame and acts as its commit marker. A batch is stored as one frame.

Additional files:
- `<path>.lock`: Process lock file.
- `<path>.compact.tmp`: Temporary file used during compaction.

## 6. Recovery algorithm

Whenever the database opens:

1. **Lock:** Acquire the database lock to prevent concurrent access.
2. **Compaction cleanup:** Handle any leftover temporary compaction file.
3. **Header validation:** Verify the database magic, version and checksum.
4. **Replay:** Read frames sequentially and validate their magic, size, sequence and CRC32C. Apply valid records to rebuild the in-memory index.
5. **Truncate:** Stop at an incomplete or invalid trailing frame. Truncate the file to the end of the last valid frame and sync it.
6. **Resume:** Continue writing after the last valid transaction sequence.

A partially written transaction or batch is never replayed.

## 7. Durability guarantees

- **Durability:** `WriteAt()` is followed by `File.Sync()` before an operation is acknowledged.
- **Atomicity:** A batch is stored as one transaction frame.
- **Recovery:** Valid frames are replayed; incomplete trailing writes are removed.
- **Concurrency:** Writes are serialized and readers see consistent states.
- **Compaction:** Live records are written to a temporary file, synced, verified and then used to replace the original file.

The torture tests validate process-crash recovery. Actual power-loss testing has not been performed.

## 8. Run automated tests

```powershell
go test ./...
go test -short ./...
```

- `go test ./...`: Full test suite.
- `go test -short ./...`: Short test suite.



**Astaroth: Write. Sync. Recover. Verify.**