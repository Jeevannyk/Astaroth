# Astaroth

Crash-safe embedded key-value database. Go, standard library only. One data file, append-only log, in-memory index.

**Status:** `Put`, `Get`, `Delete`, atomic `Batch`, durable writes, crash recovery, crash-safe compaction and the file-size limit (disk-full protection) every CLI command and the torture tool work. `DESIGN.md` has the full design.

## Build

Requires Go 1.22 or newer. No other dependencies and no network access. From the repository root:

```
go build -o . ./cmd/kvdb ./torture
```

This builds `kvdb` and `torture` into the repository root (`kvdb.exe` and `torture.exe` on Windows; `./kvdb` works in PowerShell and Git Bash alike). On Linux or macOS the source directory `torture/` has the same name as the binary, so build into a directory instead: `go build -o bin/ ./cmd/kvdb ./torture` and run `./bin/kvdb`.

## Run

```
./kvdb open  <path> [--max-bytes N]   open or create, run recovery, print key count and recovery time
./kvdb put   <path> <key> <value>     store value under key
./kvdb get   <path> <key>             print the value, or NOTFOUND
./kvdb del   <path> <key>             delete key (deleting a missing key is not an error)
./kvdb batch <path> <file>            apply a file of "put k v" / "del k" lines as one atomic batch ("-" = stdin)
./kvdb dump  <path>                   print every key and value, sorted by key
./kvdb check <path>                   verify the file without changing it; print OK or the problem
./kvdb serve <path>                   read operations from stdin, print ACK <id> after each once it is durable
```

Every command accepts `--max-bytes N` (anywhere on the line) to cap the file size; 0, the default, is unlimited. `./kvdb help` prints the list.

A session, as printed by Windows PowerShell:

```
> ./kvdb open demo.db
keys=0 recovery_ms=3.320 frames=0 torn_tail_bytes=0
> ./kvdb put demo.db name astaroth
OK
> ./kvdb put demo.db greeting "hello world"
OK
> ./kvdb get demo.db greeting
hello world
> ./kvdb get demo.db missing
NOTFOUND
> ./kvdb del demo.db name
OK
> Set-Content ops.txt "put a 1", "put b 2", "del a", "put c three words"
> ./kvdb batch demo.db ops.txt
OK 4 operations
> ./kvdb dump demo.db
b 2
c three words
greeting hello world
> ./kvdb check demo.db
OK keys=3 frames=4 bytes=221 generation=1
```

- **`open`** prints `keys=` (live keys), `recovery_ms=` (time for the whole open, recovery included), `frames=` (committed frames replayed) and `torn_tail_bytes=` (bytes of an interrupted write that recovery removed; 0 after a clean shutdown).
- **`get`** prints the value, or `NOTFOUND`. Both exit 0.
- **`batch`** reads the whole file first: a line that does not parse rejects the file before anything is written (`kvdb: ops.txt line 3: ... (nothing was written)`). Blank lines and lines starting with `#` are skipped; Windows line endings and a byte order mark are accepted. At most 10,000 operations. Operations apply in file order, so the last operation on a key wins.
- **`dump`** prints `<key> <value>` per line, sorted by the key's bytes, from one consistent snapshot.
- **`check`** never changes the file. It verifies the header, every frame's CRC, the transaction-id sequence and every record, and prints `OK keys=N frames=N bytes=N generation=N`, followed in parentheses by anything the next `open` would repair (a torn tail, an abandoned compaction file). Damage that no crash can produce prints `CORRUPT: <reason>` and exits 1.
- `get`, `dump` and `check` never create a database: on a missing path they print `kvdb: no database at <path>` and exit 1.
- **Exit codes:** 0 success; 1 the operation failed (the message on stderr says why: disk full, database in use by another process, I/O error, corrupt file); 2 wrong command line (usage on stderr).

### Keys and values as text

Inside the database keys and values are arbitrary bytes. On the command line, in batch files and in the serve protocol they are written as text:

- **Input:** `hex:<hex digits>` stands for those bytes (`hex:00ff`, and `hex:` alone is the empty value). Anything else is taken literally, byte for byte. A value starting with `-` is a value, not an option (`./kvdb put db k -5`); `--` ends options if ever needed.
- **Output** (`get`, `dump`, serve `VALUE`): a key or value is printed literally when that is unambiguous: printable ASCII, and for keys no spaces. Anything else (binary bytes, line breaks, non-ASCII, an empty value, leading or trailing spaces, text starting with `hex:`, a value that is exactly `NOTFOUND`) is printed as `hex:...`. Feeding any printed key or value back in gives the same bytes.
- In batch files and serve lines, the key is one field and the value is the rest of the line after the key, so values may contain spaces: `put greeting hello world`.

### serve

`./kvdb serve <path> [--max-bytes N]` opens the database, prints `READY keys=... recovery_ms=...`, then reads one request per line from standard input and replies on standard output:

| Request | Reply |
|---|---|
| `[<id>] put <key> <value>` | `ACK <id>` once the put is durable |
| `[<id>] del <key>` | `ACK <id>` once the delete is durable |
| `[<id>] get <key>` | `VALUE <id> <value>` or `NOTFOUND <id>` |
| `[<id>] begin` | nothing; opens a batch for this id |
| `[<id>] put ...` / `del ...` while the id has an open batch | nothing; the operation is added to the batch |
| `[<id>] commit` | `ACK <id>` once the whole batch is durable, as one atomic write |
| `[<id>] abort` | `ABORTED <id>`; the batch is discarded |
| `[<id>] compact` | `ACK <id>` once compaction is done |

- **ACK is a promise.** It is written only after the operation's `fsync` has returned. A failed operation is answered with `ERR <id> <CODE> <message>` (CODE: `PARSE`, `INVALID`, `DISKFULL`, `FAILED` or `IO`) and never with `ACK`.
- **Batches:** nothing of a batch is written before `commit`. Killing serve before the `ACK` of a commit leaves none of the batch in the database.
- **Ids:** `<id>` is optional (any word that is not a command). A line without one gets its line number as id, so a person can simply type `put a 1` and read `ACK 1`; a batch without ids is `begin`, the operations, `commit`, acknowledged with the line number of its `begin`.
- **Concurrent writers:** ids of the form `<writer>.<n>` (for example `w1.17`) belong to writer `w1`. Each writer's operations run in the order they arrive; different writers run concurrently inside the database. Ids without a `.` and lines without ids all belong to one writer, in input order.
- At end of input serve completes the queued operations, closes the database and exits 0. Batches never committed are discarded.

```
> @("put x 1", "get x", "b1 begin", "b1 put y 2", "b1 put z 3", "b1 commit", "get z") | ./kvdb serve demo.db
READY keys=0 recovery_ms=3.676 frames=0 torn_tail_bytes=0
ACK 1
VALUE 2 1
ACK b1
VALUE 7 3
```

To kill serve the way a crash would (no cleanup at all): `Stop-Process -Name kvdb -Force` in PowerShell or `taskkill /F /IM kvdb.exe` (both use TerminateProcess), `kill -9 <pid>` on Linux or macOS, or Ctrl+C in its terminal. Then `./kvdb check <path>` and `./kvdb dump <path>` show what survived: every acknowledged operation, and no part of an uncommitted batch.

## Torture tool

`torture` (source in `torture/`, built by the same `go build` command as `kvdb`) tries to break the database the way the judge does: it runs `kvdb serve` as a child process, sends it concurrent random operations, kills it with SIGKILL at a random moment, restarts it, and compares what survived with its own record of what was promised.

```
./torture --iterations 500 --seed 12345
./torture --iterations 100 --seed 999 --mode large-values
./torture --iterations 100 --seed 7 --mode compaction
./torture --iterations 100 --seed 42 --mode recovery-crash
```

| Flag | Default | Meaning |
|---|---|---|
| `--iterations N` | 500 | kill-and-restart iterations |
| `--seed N` | from the clock (printed) | the same seed gives the same operations and the same kill delays |
| `--mode M` | `normal` | see below |
| `--writers N` | 3 | concurrent writers (at least 2) |
| `--kvdb PATH` | `kvdb` next to `torture`, else `./kvdb` | the database binary under test |
| `--dir DIR` | a new temporary directory | where the database and `oracle.log` live; kept if the run fails |
| `--keep` | off | keep the directory after a successful run too |
| `--max-bytes N` | the mode's | override the size limit given to kvdb |

Exit code: 0 all iterations passed, 1 a violation was found, 2 bad arguments or setup failure.

### Modes

| Mode | Workload | Kill after |
|---|---|---|
| `normal` | puts, deletes and batches of 2 to 50 operations; text and binary values up to 64 bytes; 8 shared keys | 5 to 250 ms |
| `large-values` | values up to 1 MiB, a quarter of them exactly 1 MiB, in puts and in batches | 20 to 800 ms |
| `large-batches` | batches of 1,000 to 10,000 operations, a quarter of them exactly 10,000, next to single puts | 20 to 1,500 ms |
| `recovery-crash` | the normal workload on a database preloaded with 200,000 keys; after the kill, 1 to 3 more kills while `open()` is recovering, timed from the measured start-up and recovery time | 5 to 250 ms |
| `compaction` | overwrite-heavy, 1 to 6 KB values, and explicit `compact` requests, so kills land during compaction | 20 to 800 ms |
| `disk-full` | `--max-bytes 131072` with a delete-heavy workload, so writes keep hitting the limit and deletes keep freeing room | 10 to 400 ms |

### One iteration

1. Start `kvdb serve <db>` and wait for `READY`.
2. Start the writers. Each sends one request, waits for its reply, and sends the next at once, so **every writer always has an operation in flight**: the kill lands during operations, not between them. Each writer's request carries ids `w<n>.<seq>`, so serve runs the writers concurrently.
3. After a random delay drawn from the seed, kill the child with `Process.Kill` (SIGKILL on Linux and macOS, TerminateProcess on Windows): no signal handler and no cleanup run. Then read every line it had written, up to EOF, so no ACK printed before the kill is missed.
4. `recovery-crash` only: start serve again and kill it while it is still recovering, one to three times.
5. Run `kvdb open` (recovery), then `kvdb check` (must print `OK`, with no torn tail left), then `kvdb dump`.
6. Compare the dump with the oracle. Stop at the first violation.

### The oracle

The oracle is built only from the replies the tool received. An operation counts as promised when its `ACK` arrived; it is applied to the oracle and written to `oracle.log` in the work directory (`iter=… ACK <id> <kind> ops=N`, then one line per put or delete with the key, the value length and an FNV-64a hash). `kvdb dump` is only ever the actual state being judged, never an input to the oracle. The one thing the dump decides is the outcome of an operation that was in flight at the kill, whose outcome the spec leaves open; once a legal outcome has been observed after a restart it is durable, and the oracle moves on from there.

How the dump is judged:

- **Each writer's own keys** (`w<n>/k<i>`, written by no one else) must be exactly what that writer's acknowledged operations produce, or exactly that plus its in-flight operation applied as a whole. Anything else is reported: an acknowledged write that is missing or changed, a value nobody wrote, or a **partially committed batch** (some of the in-flight batch's keys show it applied, others show it absent).
- **Shared keys** (`s/k<i>`, written by every writer, D2): the order between two writers is not visible to a client, so the value must be the last acknowledged write of one of the writers that wrote it in this iteration, or an in-flight write to it (or the previous state, if nobody acknowledged a write to it). A value that is none of these, a mix of two, is reported.
- **Preloaded keys** (`recovery-crash`) must all be there, unchanged.
- **No other key** may exist.
- **Rejected writes** (`ERR ... DISKFULL` in `disk-full` mode) must have had no effect at all, and the data file must stay within the limit. Any other `ERR` reply, any reply to an unknown id, and any `open` or `check` failure is a violation too.

### Output

One line per iteration and a summary:

```
[ 17/500] killed  143ms after READY |  290 ops acknowledged | 3/3 writers had an op in flight | torn tail 0B | recovery 4.17ms | 101 keys | OK
...
PASS: 500 of 500 iterations, mode normal, seed 12345, 3 writers, 2m17s
  acknowledged operations: 114332 (puts 68403, deletes 23029, batches 22900 with 593431 operations, compactions 0)
  kills: 500 with SIGKILL; 500 landed while operations were in flight; 0 tore a write in progress (torn tail removed by recovery)
  in-flight operations whose outcome was unknown: 396 found applied in full, 793 found absent, none partial
  slowest recovery: 22.69 ms
```

"Found applied in full" counts operations that were durable when the kill came but whose ACK had not been printed yet: the kill landed between the fsync and the reply.

On the first violation the tool stops and prints what was expected (from the oracle), what was found (from the dump), the operation ids involved, the seed, the iteration, the failure scenario (when the kill came and what each writer had in flight), and the command to reproduce it, and keeps the work directory. This is a real report, from a deliberately broken build that wrote batches as one frame per operation:

```
FAIL: correctness violation
  what:      partially committed batch: 22 of the 26 keys batch w1.8 changes show it applied, 4 show it absent
  iteration: 1 of 50
  seed:      12345
  mode:      normal
  failure scenario:
    - iteration 1, mode normal, seed 12345
    - kvdb serve killed with SIGKILL 160ms after READY
    - writer w0 had w0.9 batch of 8 operations in flight (sent 43ms before the kill); last acknowledged: w0.8
    - writer w1 had w1.8 batch of 44 operations in flight (sent 44ms before the kill); last acknowledged: w1.7
    - writer w2 had w2.10 put s/k5 in flight (sent 44ms before the kill); last acknowledged: w2.9
    - open reported: keys=74 frames=242 torn_tail_bytes=0
  expected state (from the oracle):
    w1/k16 = w1.5:u (6 bytes)  [written by w1.5, acknowledged]   or, if w1.8 was applied: w1.8:ddrx... (57 bytes)
    ...
  actual state (from kvdb dump after recovery):
    w1/k16 = w1.8:rjyh... (42 bytes)
    ...
  operation ids involved: w1.5, w1.8 (in flight)
  reproduce: torture --iterations 1 --seed 12345 --mode normal --writers 3
```

**Reproducibility:** the seed fixes every operation, every value and every kill delay. Exactly where a kill lands inside an operation also depends on how fast the machine runs, so a failure that depends on a narrow timing window may need a few runs of the same command.

### Results on the development laptop

Run on 2026-11-06 (Windows 11, i5-1240P, NVMe SSD), 3 writers, all passed with zero violations:

| Command | Time | Acknowledged operations | What the kills hit |
|---|---|---|---|
| `--iterations 500 --seed 12345` (normal) | 2m17s | 114,332, incl. 22,900 batches with 593,431 operations | all 500 kills with operations in flight; 396 in-flight operations found applied in full, 793 absent, none partial |
| `--iterations 100 --seed 999 --mode large-values` | 1m10s | 4,749, values up to 1 MiB | 35 in-flight found applied, 231 absent |
| `--iterations 100 --seed 999 --mode large-batches` | 2m4s | 2,366, incl. 1,838 batches with 11,732,221 operations | 5 in-flight found applied, 294 absent |
| `--iterations 100 --seed 999 --mode recovery-crash` | 2m35s | 26,344 on 200,000 preloaded keys | 182 kills during restart, 136 of them before recovery finished; slowest recovery 111.56 ms |
| `--iterations 100 --seed 999 --mode compaction` | 1m1s | 20,919, incl. 1,045 compactions | 67 of the 100 kills landed while a compaction was in flight |
| `--iterations 100 --seed 999 --mode disk-full` | 33s | 3,793 | 1,083 writes rejected by the 131,072-byte limit, every one with no effect |

The tool was also checked against deliberately broken builds of `kvdb`; each was caught in the first iteration:

| Broken build | Reported as |
|---|---|
| serve sends `ACK` for a put or delete before writing it | `acknowledged write lost or changed` |
| a batch written as one frame per operation (not atomic) | `partially committed batch: 22 of the 26 keys batch w1.8 changes show it applied, 4 show it absent` |
| recovery skips every seventh committed frame | `unexpected value` (an older value visible again) |

## Use as a library

```go
db, err := engine.Open("data.db", engine.Options{})
if err != nil { ... }
defer db.Close()

err = db.Put([]byte("key"), []byte("value")) // nil means the record is fsynced
v, err := db.Get([]byte("key"))              // engine.ErrNotFound if absent
err = db.Delete([]byte("key"))               // deleting a missing key is not an error

err = db.Batch([]engine.Operation{           // all or nothing; nil means the whole batch is fsynced
	{Kind: engine.OpPut, Key: []byte("a"), Value: []byte("1")},
	{Kind: engine.OpDelete, Key: []byte("b")},
})
```

Limits: key 1 to 1024 bytes (`engine.ErrInvalidKey`), value 0 to 1,048,576 bytes (`engine.ErrValueTooLarge`). `engine.Options{MaxBytes: n}` caps the file size; a write that does not fit returns `engine.ErrDiskFull` (see "Disk full"). `db.Compact()` compacts on demand.

## Test

```
go test ./...            # about 100 s (includes every torture mode for a few iterations)
go test -short ./...     # about 25 s
go test -race ./...      # about 3 minutes; needs cgo and a C compiler (gcc), as Go's race detector does
```

Under `-race` the 1,000,000-key timing test skips itself: instrumented code runs several times slower, so its timings mean nothing.

Besides unit and API tests, `tests/` has the process-kill scenarios (see "Crash tests") and the 1,000,000-key recovery measurement (skipped with `-short`; see "Measured recovery time").

### The permanent database file

Tests of normal behaviour (`tests/` and `cmd/kvdb`) run against one permanent database file:

```
data/astaroth.db        in the project root, created on first use (data/ is created if missing)
```

- **It is never deleted or truncated by any test.** Tests close the database (which releases its lock) and leave the file on disk. It keeps growing with every run, by about 2 MB per `go test -short ./...` (the log only appends and compaction is not built yet). To start over, delete `data/` yourself. `data/` is in `.gitignore`.
- **You can inspect it at any time** with `kvdb open data/astaroth.db`, which prints the key count and recovery time. Do not run it while a test is running: the file is locked while a test holds it open.
- **Tests never assume it is empty.** Every key a test writes starts with a prefix made from the run's timestamp, process id and test name (`tests/testdb`), and tests assert only on their own keys, never on the total key count.
- **Package test processes take turns.** `go test ./...` runs packages in parallel, so the first test to use the file takes `data/tests.lock` for the life of its process; the other package waits for it. Besides this and the database's own `astaroth.db.lock`, there is only the one data file.
- **Tests that destroy data on purpose do not use it.** The truncation, corruption and damaged-file tests, and the unit tests in `src/`, build or copy their files under `t.TempDir()` and delete them afterwards.

## Layout

| Path | Role |
|---|---|
| `src/engine` | public `DB`: `Open`, `Put`, `Get`, `Delete`, `Len`, `Close`; owns the RWMutex and the write path |
| `src/storage` | on-disk format: header and frame codec, data-file open |
| `src/recovery` | validates the file, replays frames into the index, truncates a torn tail, cleans up after an interrupted compaction |
| `src/failpoint` | test-only kill points (inert unless `ASTAROTH_FAILPOINT` is set) |
| `src/index` | in-memory key to file-offset index |
| `src/lock` | cross-process lock on `<path>.lock` (Windows `CreateFile` share mode 0, Unix `flock`) |
| `cmd/kvdb` | CLI: `main.go` (arguments, exit codes), `commands.go` (open to check), `serve.go` (serve protocol), `oplines.go` (the `put k v` / `del k` line format) |
| `src/token` | text form of keys and values (`hex:` encoding) |
| `torture/` | torture tool: `main.go` (iterations, verification, report), `workload.go` (modes, writers, oracle log), `proc.go` (running kvdb) |
| `tests/` | black-box tests through the public API |
| `diagrams/` | architecture diagram |

## On-disk format

All integers are little-endian. The file is a 32-byte header followed by zero or more frames, back to back, with nothing between them:

```
+--------+---------+---------+-----+---------+
| header | frame 1 | frame 2 | ... | frame N |
+--------+---------+---------+-----+---------+
 32 bytes
```

### File header (32 bytes)

```
off size field
  0    8 magic       "ASTAROTH"
  8    4 version     1
 12    4 flags       0
 16    8 generation  1 at creation, +1 per compaction
 24    4 header_crc  CRC32C (Castagnoli) of bytes 0..23
 28    4 reserved    0
```

### Frame

A frame is the unit of writing and of commit: one transaction. Every `Put` and `Delete` is one frame holding one operation; every `Batch` is one frame holding all its operations. `seq` is the transaction id and `frame_crc` is the commit marker (see "Write path: transactions and the commit marker").

```
off    size  field
  0       4  frame_magic  0xA57A0F01   (bytes 01 0F 7A A5)
  4       4  payload_len  N: number of payload bytes, at most 64 MiB
  8       8  seq          1 for the first frame, then previous seq + 1
 16       4  op_count     number of operations in the payload, 1..10000
 20       N  payload      op_count operations, back to back
 20+N     4  frame_crc    CRC32C of bytes 0..20+N-1 (the header and the payload)
```

Total size is `24 + N`.

### Operation (inside the payload)

```
PUT   type(1)=0x01  key_len(2)  val_len(4)  key[key_len]  value[val_len]
DEL   type(1)=0x02  key_len(2)  key[key_len]               (a tombstone)
```

`key_len` is 1..1024. `val_len` is 0..1,048,576 (an empty value is allowed and is not the same as a missing key). A PUT operation takes `7 + key_len + val_len` bytes; a DEL takes `3 + key_len`.

### Example: `Put("k", "vv")` on a new file

```
offset  bytes                       meaning
  0     41 53 54 41 52 4f 54 48     magic "ASTAROTH"
  8     01 00 00 00                 version 1
 12     00 00 00 00                 flags
 16     01 00 00 00 00 00 00 00     generation 1
 24     .. .. .. ..                 header_crc
 28     00 00 00 00                 reserved
 32     01 0f 7a a5                 frame_magic
 36     0a 00 00 00                 payload_len = 10 (7 + 1 + 2)
 40     01 00 00 00 00 00 00 00     seq 1
 48     01 00 00 00                 op_count 1
 52     01                          type PUT
 53     01 00                       key_len 1
 55     02 00 00 00                 val_len 2
 59     6b                          key "k"
 60     76 76                       value "vv"
 62     .. .. .. ..                 frame_crc
 66                                 end of file
```

### Why this format detects incomplete writes

A frame is committed if and only if its whole CRC is valid. `frame_magic` rejects zero-filled or foreign bytes quickly, `payload_len` must fit inside the file, and the trailing CRC covers the header and every payload byte. A write cut off at any byte leaves a frame that fails one of those checks, so a torn frame can never be read as data. The CRC is the commit marker; there is no separate marker record, and one `fsync` per operation is enough.

### Overwrites and deletes

A later PUT for the same key supersedes the earlier one; a DEL (tombstone) removes the key. Old records stay in the file until compaction (not built yet). Deleting a key that does not exist writes nothing: the index holds exactly the durable state, so such a key is already gone after any crash.

## Write path: transactions and the commit marker

`Put`, `Delete` and `Batch` share one write path (`appendLocked` in `src/engine/engine.go`). Every call becomes exactly one frame: a single `Put` or `Delete` is a frame of one operation, a `Batch` is a frame of all its operations.

```
+-------------------------------------------+-----------------------------+-----------------------+
| frame header (20 bytes)                   | records                     | commit marker (4 B)   |
| frame_magic | payload_len | seq | op_count| op 1 | op 2 | ... | op N    | frame_crc = CRC32C of |
|             |             | = transaction id                            | header + all records  |
+-------------------------------------------+-----------------------------+-----------------------+
```

- **Transaction id.** The frame's 8-byte `seq` is the transaction id. Ids are consecutive: recovery refuses a file in which a committed frame's id is not the previous id + 1, and continues numbering after the last committed frame (an uncommitted frame's id is reused by the next commit, since that frame never existed).
- **Commit marker.** The last 4 bytes of the frame, `frame_crc`: a CRC32C over the header (transaction id, op count, length) and every record. A frame is committed if and only if its commit marker is present and matches. Without it the frame, and every operation in it, does not exist.

### Exact order of writes, fsync and the commit marker

Under the database's exclusive lock, for every `Put`, `Delete` and `Batch`:

1. **Validate** every operation (key 1 to 1024 bytes, value up to 1 MiB, at most 10,000 operations, at most 64 MiB encoded). One invalid operation rejects the whole batch; nothing has been written.
2. **Encode** the frame into one buffer: header, then the records in the caller's order, then the commit marker computed over everything before it.
3. **`WriteAt(frame, end)`**: one write call puts header, records and commit marker into the file at the end offset the engine owns. There is no `fsync` between the records and the marker, and none is needed: the marker is a checksum of the records, not a flag, so it cannot validate unless every byte it covers reached the file, whatever order the bytes got there in. A flag-style marker would need "write records, fsync, write marker, fsync"; the checksum makes that ordering unnecessary.
4. **`File.Sync()`** (`fsync`; `FlushFileBuffers` on Windows), once. When it returns, the whole frame is durable.
5. **Apply** every operation to the in-memory index, in order, still holding the exclusive lock. `Get` takes the lock shared, so it sees the state before the batch or after it, never part of it. Nothing becomes visible before step 4 has returned.
6. **Return** `nil`. Only now may the caller (or `serve`) acknowledge the batch.

If `WriteAt` fails, the partial frame is cut off with `Truncate` (and if that fails, the handle stops accepting writes). If `Sync` fails, the handle stops accepting writes (`ErrFailed`) and the sync is never retried, because after a failed `fsync` the OS may have dropped the data and a second `Sync` could report success falsely.

### What a crash leaves, by step

| Killed | On disk | After restart |
|---|---|---|
| before step 3 | nothing of this batch | batch absent |
| during step 3, after 3 of 10 records | header and 3 records, no marker | frame incomplete: batch absent, tail truncated |
| during step 3, after all records, before the marker | header and all records, no marker | `payload_len` says the frame is 4 bytes longer than the file: batch absent, tail truncated |
| after step 3, before step 4 returns | whole frame in the OS cache | after a process kill, the OS still writes it: batch present in full. (After a power loss either outcome is possible; the marker decides, never part of a batch.) |
| after step 4, before step 6 | whole frame durable | batch present in full |
| after step 6 | whole frame durable | batch present in full |

### Batch semantics

- Operations apply in list order; when several touch the same key the last one wins. `[put x, delete x]` leaves `x` absent, `[delete x, put x=v]` leaves `x = v`, `[put k=1, put k=2]` leaves `k = 2`. Recovery replays a frame's operations in the same order, so a crash cannot change the outcome (spec scenario B5).
- An empty batch succeeds and writes nothing.
- Limits: 10,000 operations (`ErrTooManyOps`), 64 MiB encoded (`ErrBatchTooLarge`, so 10,000 operations of 1 MiB values do not fit in one batch), and the per-key and per-value limits.
- A single `Delete` of a key that does not exist writes nothing. Inside a batch, deletes are always written, because an earlier operation of the same batch may have created the key.

## Concurrency

Any number of goroutines may call `Put`, `Delete`, `Batch`, `Get` and `Len` on one `DB` at the same time. The model is one `sync.RWMutex` per database (the spec allows a single global lock):

- **Writers** (`Put`, `Delete`, `Batch`, `Close`) take it exclusively for the whole write path: encode, `WriteAt`, `Sync`, index update. Writes are therefore serialized, one frame at a time, at the offset the engine owns, and transaction ids are handed out in that order. Two writers can never interleave bytes in the file or updates in the index.
- **Readers** (`Get`, `Len`) take it shared, so any number of them run together, and in between writes. A reader sees the index either before a write's update or after all of it (a whole batch at once), and the index only ever reflects synced data.
- Validation and argument checks run before the lock; the in-memory index, the append offset, the next transaction id and the failure state are only touched under it.
- Between processes, `<path>.lock` keeps a second process from opening the same database.

Consequences, and what each guarantee rests on:

| Guarantee | Why it holds |
|---|---|
| No corrupted records | one writer at a time appends one complete frame at the engine-owned offset; nothing else writes the file |
| No mixed values from concurrent writers | a value is written by one frame and read back from one record; writes to the same key are serialized, so the key holds the value of whichever write came last |
| No partially visible batches | the whole batch is applied to the index under the exclusive lock, after its fsync |
| No races in the in-memory index | the index is only accessed under the lock; verified with `go test -race ./...` |
| Every acknowledged operation is durable | each writer's `Sync` returns before its lock is released and before it returns |

The cost: a `Get` waits while a write is being fsynced, and writers do not share an fsync (no group commit). The spec values correctness over speed here.

### Concurrency tests

- **`TestConcurrencyStress`** (permanent database): 4 putters, 2 deleters and 2 batch writers, partly on shared keys, with 4 readers running throughout. Every value is self-describing (key, writer, sequence number, CRC; `tests/values_test.go`), so a torn value, a mix of two writers' values or another key's value fails the check. Readers check during the run that values are intact, that a batch is never visible half applied, and that every acknowledged put is visible to other goroutines. Afterwards: the bytes appended parse as complete frames with consecutive transaction ids, and before and after a restart every acknowledged operation's result is present (a shared key must hold some writer's last action on it).
- **`TestConcurrentWritersKilledAtRandom`** (spec D1, D2, D3): a child process runs two putters (with a shared key), a batch writer and a deleter at once and is killed with `Process.Kill` (TerminateProcess) after a random 80 to 480 ms, so the kill lands wherever the writers are, inside writes and fsyncs included. 15 iterations, the seed is logged. Per writer: every acknowledged operation present, at most the one in-flight operation extra, the shared key holding a value one of the writers actually sent, the batch group one whole batch (the last acknowledged or the one in flight), and no unknown keys.
- **`TestReadersNeverSeePartialBatch`** and **`TestConcurrentBatchesAndSinglePuts`** (from the batch phase).

The tests were checked against deliberate bugs: removing `Get`'s read lock made `go test -race` report 24 data races; removing `Put`'s writer lock made the stress test fail with `index points at the wrong record`; acknowledging a `Put` before its frame was written made the random-kill test fail with `acknowledged c/w0/91 lost`.

## Compaction

The log only appends, so overwritten values, deleted keys and tombstones pile up. Compaction (`DB.Compact`, `src/engine/compact.go`) rewrites the file so it holds only the latest value of every existing key, and nothing for deleted keys. The database content does not change.

It runs automatically before a write when the file is at least 4 MiB and less than half of it is live data, and can be called explicitly. If an automatic compaction fails, the write goes ahead on the old file and automatic compaction is not retried for the life of that handle (`Compact` still works). After 1,000,000 puts followed by 1,000,000 deletes, the file shrinks to the 32-byte header.

### Exact order

Under the exclusive lock (no reads or writes run during compaction):

1. Create `<path>.compact.tmp`, truncating any leftover.
2. Write the header with `generation + 1`, then every live record. The old log is read once, sequentially; a PUT record is copied only if the index points at exactly that record, so overwritten values and tombstones are dropped. Dropping tombstones is safe because the new file is a complete snapshot: no older record is left in it for them to hide. Records go into frames of up to 1,000 records (or 8 MiB), with transaction ids continuing from the old file.
3. `Sync` the temporary file.
4. **Verify** it: run the real recovery code on it and require exactly the index built while writing it (same keys, same offsets, same end). Any difference aborts the compaction.
5. Close the temporary file and the old data file (Windows cannot replace an open file).
6. **Atomically replace** the data file with the temporary file: `rename(2)` on Unix; on Windows one `MoveFileEx` call with `MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH` (through `syscall`, since `os.Rename` does not pass write-through). This is the commit point: before it the old file is the data file, after it the new one, and at no moment is there neither.
7. **Sync the directory** so the rename itself is durable: `fsync` on the directory on Unix. Windows offers no directory fsync through `os`; `MOVEFILE_WRITE_THROUGH` in step 6 makes the call return only after the rename is on disk instead.
8. Reopen the data file and switch to the new index.

The old file is never modified, and never removed except by the atomic replace in step 6, so it stays complete and authoritative until the new one has been written, synced and verified. A failure before step 6 removes the temporary file and leaves the database as it was; a failure after step 6 leaves the complete new file as the data file and marks the handle failed (reopen to continue).

### What a crash leaves, by step

| Killed | Data file | Temporary file | Next `Open` |
|---|---|---|---|
| before step 1 | old | none | normal recovery of the old file |
| during step 2 (some frames written) | old | incomplete | removes the temporary file; old file |
| after step 2, before or after step 3 | old | complete, maybe unsynced | removes it; old file |
| after step 4, before step 6 (old file closed) | old | complete, verified | removes it; old file |
| during step 6 | old or new (one atomic call) | present if old, gone if new | as the row above, or as the row below |
| after step 6, before step 7 or 8 | new | none | normal recovery of the new file |

There is nothing left to clean up after step 6: the temporary file became the data file. The lock lives on `<path>.lock`, a separate file, so the swap does not touch it.

On `Open`, before the data file is opened (`recovery.CleanupCompaction`): if both files exist, the temporary file is removed (if removing fails it is ignored, and the next compaction truncates it). If the data file is missing but a temporary file exists, which only a non-atomic "remove old, then rename new" replace could leave, the temporary file is renamed into place: it is renamed only after step 4, so it is complete. Both actions are safe to repeat if `Open` itself is killed.

## Disk full: the size limit

`engine.Open(path, engine.Options{MaxBytes: n})` (CLI: `--max-bytes n`) caps the data file at `n` bytes; 0 means unlimited, otherwise `n` must be at least 32 (the header). This is how the spec simulates a full disk. A real "no space left on device" error from the OS is handled the same way.

**Checked before anything is written.** In the write path (`appendLocked`, step 0 before the order in "Write path"), the size of the whole frame is computed first and compared with the limit: `end + frame size <= limit`. A write that does not fit returns `ErrDiskFull` and leaves no byte behind. A batch is one frame, so it is checked as a whole: the limit can never commit part of a batch, even when its first operations alone would fit. The check comes before the frame is encoded, because making room may compact the file, which moves the transaction ids on.

**The error says what happened**, for example:

```
engine: database is full: a 1059-byte write would grow the file from 61159 to 62218 bytes,
over its limit of 61440 (MaxBytes 65536); delete keys to make room
```

**Deletes always have room.** A completely full file could not even append a tombstone, so it could never free space. Puts, and batches containing any put, may only grow the file to `MaxBytes - reserve`; batches of only deletes may use the reserve. The reserve is `MaxBytes / 16`, at least 4 KiB and at most 4 MiB, and never more than a quarter of `MaxBytes`. A tombstone frame is at most 1,051 bytes. A single `Delete` of a missing key writes nothing at all.

**Compaction makes room by itself.** When a write does not fit but compacting would leave enough room (estimated from the live data), the write compacts the file first and then goes ahead. So after deletes free space, writes succeed again without any manual step. When compacting would not help, it is not attempted.

**Compaction is limited too.** The temporary file a compaction writes may not grow past `MaxBytes` either; if the live data does not fit, compaction fails with `ErrDiskFull` before the replace, the temporary file is removed and the old file is untouched (see "Compaction": nothing changes before the atomic replace). A real disk-full error while writing the temporary file is handled the same way.

**A real disk-full error during a write** (`ENOSPC` or `EDQUOT` on Unix; `ERROR_DISK_FULL` or `ERROR_HANDLE_DISK_FULL` on Windows) may leave part of the frame in the file. The engine cuts it off with `Truncate(end)` (which needs no free space), returns `ErrDiskFull`, and stays usable. If that `Truncate` fails too, the handle stops accepting writes. If the disk fills up during `fsync` instead, the handle stops accepting writes, as for any `fsync` failure.

**Opening is never refused for size.** A file that is already over its limit (the limit was lowered) opens and reads normally; writes are rejected until compaction or a higher limit makes room.

## Recovery

On every `Open`:

1. Take `<path>.lock`; fail with "held by another process" if it stays taken for the timeout.
2. Deal with an interrupted compaction: remove `<path>.compact.tmp` if the data file exists, or rename it into place if the data file is missing (see "Compaction").
3. Open or create the data file.
4. File shorter than 32 bytes and empty, zero-filled, or a prefix of our header: a crash during creation. Rewrite the header and sync. No data can exist yet.
5. File of 32 bytes or more: the header must have the right magic, CRC and version, otherwise "not an Astaroth database file" and the file is left untouched.
6. Read frames from offset 32. Each frame must have the right magic, a length that fits in the file, a valid CRC, and `seq` equal to the previous `seq + 1`. Each valid frame is applied to the index in order, and within a frame the ops are applied in list order, so the last op on a key wins whether it is a put or a delete. The index maps each key to the file offset of its latest PUT record; values are not kept in memory.
7. The first frame that is incomplete or fails its CRC ends the log. Everything from there to the end of the file is a torn tail: it is cut off with `Truncate` and synced, and new writes begin at that offset. The tail must be removed, otherwise later acknowledged writes would sit behind garbage and be invisible after the next restart.
8. Two situations are not treated as a torn tail, and the file is left untouched with an error: a frame with a valid CRC but malformed contents or a sequence gap, and an invalid region larger than one maximum-size frame (a crash can leave at most one frame in flight).

Recovery only reads, except for step 2, step 4 and step 7, which are safe to repeat. A crash during recovery therefore leaves a file that the next `Open` handles the same way: recovering any number of times, interrupted or not, gives the same data and the same final bytes.

A read error while scanning is never mistaken for a torn tail; it fails `Open` instead of truncating anything.

### What makes replay fast

Profiling 1,000,000 keys showed the cost was the Go map (inserting and repeatedly growing it) and per-frame allocation, not the checksum (about 2% of the time). Two changes, each measured separately:

- `storage.FrameReader` reuses its buffers between frames instead of allocating per frame (about 540 ms to 445 ms on the single-op log).
- After replaying the first 1 MiB of the log, recovery estimates the final number of keys from the distinct keys seen per byte and sizes the index map once (`Index.Reserve`), capped at 4,000,000 entries (about 445 ms to 312 ms). An overwrite-heavy log gives a small estimate, so it does not over-allocate.

I tried and dropped turning the garbage collector off during replay: the gain was inside the noise and it changes a process-wide setting.

### Measured recovery time

Measured by `TestRecoveryTimeOneMillionKeys` (`go test -run TestRecoveryTime -v ./tests`, run alone) on the development laptop: Intel i5-1240P (12 cores, 16 threads), 15.7 GB RAM, NVMe SSD, Windows 11, Go 1.27, "Balanced" power plan. Times are `engine.Open` on a file of 1,000,000 distinct keys (14-byte keys), median of 3 to 5 runs:

| Log shape | File size | `open()` median | min to max |
|---|---|---|---|
| 1,000,000 single-op frames, 64 B values | 109 MB | 292 ms | 261 to 383 ms |
| 100 frames of 10,000 ops, 64 B values | 85 MB | 292 ms | 254 to 343 ms |
| 100 frames of 10,000 ops, 256 B values | 277 MB | 395 ms | 386 to 493 ms |
| 100 frames of 10,000 ops, 1,024 B values (opt-in, `ASTAROTH_BENCH_BIG=1`) | 1,045 MB | 791 ms | 731 to 814 ms |

The same files opened with the real binary (`kvdb open`, timed from outside) took 0.29 to 0.38 s of wall time, with one exception: the very first launch of a freshly built `kvdb.exe` took 1.29 s of wall time while its own reported `recovery_ms` was 360 ms. The extra second is process start, probably antivirus scanning a new executable, not recovery. It is still under 2 s.

Before the two optimizations the first three shapes took about 625, 431 and 704 ms (medians). The 2-second target is met on this machine for these shapes, with a margin of about 5x on the first three. What these numbers do **not** cover:

- **Cold cache.** The files were just written, so they sit in the OS page cache, as they would after a process kill. A first open after a reboot reads the file from disk. I could not drop the cache to measure that.
- **Time grows with file size, not only key count.** Roughly 0.3 s for the 1,000,000 keys plus about 0.5 s per GB of log, taken from the 85 MB and 1 GB rows. Extrapolating, the 2 s budget would be reached around a 3 GB log; that is an extrapolation, not a measurement.
- **There is no compaction yet**, so the log is never shortened by overwrites and deletes.
- Other key sizes, other value distributions, and a slower or busier machine were not measured.

## Crash tests

Crash safety is tested by killing real processes. `src/failpoint` lets a test arm a named point with the environment variable `ASTAROTH_FAILPOINT=name[:N]` (the Nth time the point is reached). When it fires, the process calls `os.Exit(86)` on the spot: no deferred calls, no `Close`, no flushing, so the files are left as a killed process leaves them. It is inert when the variable is unset. A test that arms a point checks for exit code 86, so a point that never fired cannot make a test pass by accident.

`tests/child_test.go` re-runs the test binary as a child process that executes a scripted workload and prints `ACK i` after operation `i` returns. The parent knows exactly which operations were acknowledged, recovers the file, and checks the result. Crash tests work on files in temp dirs; they never touch `data/astaroth.db`.

| Scenario | Failpoint | Required result after restart |
|---|---|---|
| Kill before any byte of the record is written | `append-before-write` | exactly the acknowledged operations |
| Kill inside the frame header | `append-partial-header` | exactly the acknowledged operations |
| Kill in the middle of the record | `append-partial-write` | exactly the acknowledged operations |
| Kill after the full write, before fsync | `append-after-write` | acknowledged, plus the in-flight operation fully present or fully absent |
| Kill after fsync, before success is returned | `append-after-sync` | acknowledged plus the in-flight operation (it was durable) |
| Kill immediately after success is returned | none (`os.Exit` after the last ACK) | all acknowledged operations |
| B1: batch of 10, kill after 3 records are written | `append-after-3-records` | none of the batch visible |
| B2: batch of 10, kill after all records, before the commit marker | `append-before-commit-marker` | none of the batch visible |
| Batch, kill in the middle of the frame | `append-partial-write` | none of the batch visible |
| B3: batch, kill after the commit marker, before fsync | `append-after-write` | the whole batch or none of it |
| Batch, kill after fsync, before acknowledgement | `append-after-sync` | the whole batch |
| B4: kill right after a batch is acknowledged | none | every acknowledged batch |
| B6: 10,000-op batch killed halfway (three ways) | `append-partial-write`, `append-after-3-records`, `append-before-commit-marker` | none of it visible; open took 60 to 90 ms |
| D1 to D3: two putters (sharing a key), a batch writer and a deleter, killed at a random moment | none: `Process.Kill` after a random delay | every acknowledged operation of every writer, at most the one in-flight operation per writer, whole batches only |
| Kill before compaction starts | `compact-start` | old file, all data |
| Kill while writing the temporary file (after 1, 2 or 3 of its 3 frames) | `compact-write-frame:N` | old file, temporary file removed |
| Kill after writing, before synchronizing the temporary file | `compact-before-sync` | old file, temporary file removed |
| Kill after sync, before verification | `compact-before-verify` | old file, temporary file removed |
| Kill immediately before the rename (old file already closed) | `compact-before-rename` | old file, temporary file removed |
| Replacement interrupted between "remove old" and "rename new" (state built by hand) | none | temporary file renamed into place; new file, all data |
| Kill right after the rename, before the directory sync | `compact-after-rename` | new file, all data, nothing to clean up |
| Kill after the directory sync, before reopening | `compact-after-dir-sync` | new file, all data |
| Kill after compaction finished | `compact-done` | new file, all data |
| Kill during `Open`'s cleanup (removing the temporary file, or renaming it into place) | `recover-before-tmp-remove`, `recover-before-tmp-promote` | same as an uninterrupted cleanup |
| Compaction produces a damaged temporary file | `compact-corrupt-tmp` (flips a byte) | verification refuses it; old file unchanged byte for byte |
| E1: limit reached during a put | none (limit) | put rejected with `ErrDiskFull`, file unchanged, every earlier key intact, deletes still accepted |
| E2: limit reached partway through a batch of 32 | none (limit) | whole batch rejected, file byte-identical, none of its operations visible (also after restart) |
| E3: limit reached, then the process exits without `Close` | none (child fills until rejected) | restart succeeds, every acknowledged put intact, limit still enforced |
| E4: limit reached during compaction (live data larger than the limit) | none (limit) | compaction fails with `ErrDiskFull`, temporary file removed, old file byte-identical |
| E4: real disk-full error while writing the temporary file | `compact-disk-full` | compaction fails with `ErrDiskFull`, old file byte-identical |
| E5: fill, delete half, write again with no manual step | none (limit) | the rejected write compacts and succeeds; file never over the limit |
| Real disk-full error halfway through a write (single ops and a batch) | `append-disk-full:N` | operation rejected, half frame cut off (raw file checked before recovery), later operations succeed |
| Filled to the exact byte | none (limit) | smallest put rejected, deletes still accepted (the reserve) |
| Kill during recovery | `recover-before-header`, `recover-after-header`, `recover-replay-frame:N`, `recover-before-truncate`, `recover-after-truncate`, `recover-after-truncate-sync` | same data and same final file bytes as an uninterrupted recovery |
| Restart after recovery was interrupted | several of the above in a row | same as above, plus nothing acknowledged is lost |

In the batch tests every batch rewrites the same keys, so a partly applied batch would show up as a mix of two batches' values; the recovered content must equal the state after a whole number of batches. A separate test cuts a committed batch frame at every byte, including the cut that keeps all records and drops only the marker, and requires the batch to vanish each time. Readers running during batches must never see one half applied (`TestReadersNeverSeePartialBatch`).

For compaction, the workload and the compaction run in separate processes, so the test knows the file size before compaction and the header generation tells which file survived (1: old, 2: compacted). After each scenario the database must open, hold exactly the acknowledged data, have no temporary file left, accept a write, and compact again.

After each recovery the tests also check that the log ends cleanly (every byte belongs to a valid frame), that a new write is appended and survives another restart, and that the database holds no keys it was never given. Killing at the Nth write is repeated for every N in the script.

What this model cannot show: a lost write after the OS reports `fsync` success (the spec assumes honest hardware), and a power loss (not required). The page cache survives a process kill, so "written but not synced" and "synced" look the same to these tests; that is why the "before fsync" scenario accepts either outcome, as the spec does.

## Known limits (this phase)

- Recovery time is measured above for four shapes on one machine; other conditions are not.
- Compaction blocks all reads and writes while it runs, and needs free disk space for a second copy of the live data.
- The automatic compaction thresholds (4 MiB, half dead) are fixed.
- `MaxBytes` limits the data file and the compaction temporary file separately; during a compaction both exist, so peak disk use is up to about twice the limit.
- A file already over a lowered limit rejects deletes too (they do not fit either); raise the limit or compact with room to spare.
- The atomic replace relies on `MoveFileEx(MOVEFILE_REPLACE_EXISTING)` being atomic on NTFS and `rename(2)` on Unix filesystems. The NTFS behaviour is widely relied on but not formally documented; open's "data file missing" rule covers a replace that is not atomic.
- Damage in the middle of the file (not a crash) is treated as the end of the log if the rest is smaller than one maximum frame. Everything after the damage is dropped. The spec excludes bit rot.
- Reads and writes share one lock, so a `Get` waits while a write is being fsynced, and concurrent writers each pay for their own fsync (no group commit).
- Whether a given `Sync` actually reaches the disk cannot be observed from a test; it is verified by the code (`appendLocked` in `src/engine/engine.go`) and will be exercised by the torture tool.
- Real kills rarely tear a write. On this machine the operating system finishes a `write` call even when the process is being killed, so in thousands of torture kills recovery has not had to remove a torn tail: kills land between system calls (between the write and the fsync, or between the fsync and the ACK, which the torture tool does see). Torn frames are covered instead by the byte-by-byte truncation test and the failpoint tests that write half a frame and exit.
- A stray `ASTAROTH_FAILPOINT` environment variable would kill the process at that point. It is only read by `src/failpoint`, and unset in normal use.
