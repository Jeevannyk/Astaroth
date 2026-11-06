package tests

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"sync"
	"testing"

	"astaroth/src/engine"
	"astaroth/src/storage"
	"astaroth/tests/testdb"
)

// These tests run against the permanent database, which keeps its data between
// runs. Every key comes from pk/keyOfLen, so it is unique to this run, and
// every assertion is about those keys only. Nothing asserts on Len().

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func put(t *testing.T, db *engine.DB, k, v string) {
	t.Helper()
	if err := db.Put([]byte(k), []byte(v)); err != nil {
		t.Fatalf("Put(%q): %v", k, err)
	}
}

func del(t *testing.T, db *engine.DB, k string) {
	t.Helper()
	if err := db.Delete([]byte(k)); err != nil {
		t.Fatalf("Delete(%q): %v", k, err)
	}
}

func wantValue(t *testing.T, db *engine.DB, k, want string) {
	t.Helper()
	got, err := db.Get([]byte(k))
	if err != nil {
		t.Fatalf("Get(%q): %v", k, err)
	}
	if string(got) != want {
		t.Fatalf("Get(%q) = %q, want %q", k, got, want)
	}
}

func wantMissing(t *testing.T, db *engine.DB, k string) {
	t.Helper()
	if v, err := db.Get([]byte(k)); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("Get(%q) = %q, %v; want ErrNotFound", k, v, err)
	}
}

// pattern returns n deterministic bytes that differ per seed, so a value
// swapped between keys or shifted by one byte is detected.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed ^ byte(i>>8)
	}
	return b
}

func TestPutGet(t *testing.T) {
	db := openPerm(t)
	defer db.Close()

	hello, second := pk(t, "hello"), pk(t, "second")
	put(t, db, hello, "world")
	wantValue(t, db, hello, "world")

	put(t, db, second, "value")
	wantValue(t, db, hello, "world")
	wantValue(t, db, second, "value")
}

func TestOverwrite(t *testing.T) {
	db := openPerm(t)
	defer db.Close()

	k := pk(t, "k")
	put(t, db, k, "first")
	put(t, db, k, "second, longer")
	put(t, db, k, "3")
	wantValue(t, db, k, "3")
}

func TestGetMissing(t *testing.T) {
	db := openPerm(t)
	defer db.Close()

	wantMissing(t, db, pk(t, "nothing"))
	present := pk(t, "present")
	put(t, db, present, "x")
	wantMissing(t, db, pk(t, "nothing"))
	wantMissing(t, db, present[:len(present)-1]) // a prefix of an existing key is a different key
	wantMissing(t, db, present+"x")
}

func TestDeleteExisting(t *testing.T) {
	db := openPerm(t)
	defer db.Close()

	a, b := pk(t, "a"), pk(t, "b")
	put(t, db, a, "1")
	put(t, db, b, "2")
	del(t, db, a)
	wantMissing(t, db, a)
	wantValue(t, db, b, "2")

	put(t, db, a, "again") // a deleted key can be written again
	wantValue(t, db, a, "again")
}

func TestDeleteMissing(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	defer db.Close()

	start := fileSize(t, path)
	del(t, db, pk(t, "never-written"))
	if fileSize(t, path) != start {
		t.Fatal("deleting a missing key grew the file")
	}

	k := pk(t, "k")
	put(t, db, k, "v")
	del(t, db, k)
	afterFirst := fileSize(t, path)
	if afterFirst <= start {
		t.Fatal("put and delete wrote nothing")
	}
	del(t, db, k) // already deleted
	if fileSize(t, path) != afterFirst {
		t.Fatal("deleting an already-deleted key grew the file")
	}
	wantMissing(t, db, k)
}

func TestEmptyValueIsNotMissing(t *testing.T) {
	db := openPerm(t)
	defer db.Close()

	empty, nilval := pk(t, "empty"), pk(t, "nilval")
	put(t, db, empty, "")
	got, err := db.Get([]byte(empty))
	if err != nil || len(got) != 0 {
		t.Fatalf("Get = %q, %v; want empty value, nil", got, err)
	}
	if err := db.Put([]byte(nilval), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := db.Get([]byte(nilval)); err != nil || len(got) != 0 {
		t.Fatalf("Get = %q, %v; want empty value, nil", got, err)
	}
}

func TestLargeValues(t *testing.T) {
	db := openPerm(t)

	sizes := []int{1, 4095, 4096, 4097, 65536, 500_000, storage.MaxValueLen}
	key := func(i int) []byte { return []byte(pk(t, fmt.Sprintf("big%d", i))) }
	for i, n := range sizes {
		if err := db.Put(key(i), pattern(n, byte(i))); err != nil {
			t.Fatalf("Put %d bytes: %v", n, err)
		}
	}
	check := func(db *engine.DB) {
		for i, n := range sizes {
			got, err := db.Get(key(i))
			if err != nil {
				t.Fatalf("Get %d bytes: %v", n, err)
			}
			if !bytes.Equal(got, pattern(n, byte(i))) {
				t.Fatalf("value of %d bytes came back different", n)
			}
		}
	}
	check(db)
	db.Close()
	db = openPerm(t)
	defer db.Close()
	check(db)
}

func TestValueOverLimitRejected(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	defer db.Close()

	k := pk(t, "k")
	put(t, db, k, "old")
	before := fileSize(t, path)
	err := db.Put([]byte(k), make([]byte, storage.MaxValueLen+1))
	if !errors.Is(err, engine.ErrValueTooLarge) {
		t.Fatalf("err = %v, want ErrValueTooLarge", err)
	}
	wantValue(t, db, k, "old") // a rejected write leaves the old value
	if fileSize(t, path) != before {
		t.Fatal("rejected write changed the file")
	}
}

func TestKeyLengths(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	defer db.Close()

	// Valid edges. The value carries the run id, so a stale value left by an
	// earlier run under the same (short) key could not pass for this write.
	val := []byte(testdb.RunID())
	for _, n := range []int{1, 2, 255, 256, 1023, 1024} {
		key := keyOfLen(n)
		if err := db.Put(key, val); err != nil {
			t.Fatalf("Put %d-byte key: %v", n, err)
		}
		if got, err := db.Get(key); err != nil || !bytes.Equal(got, val) {
			t.Fatalf("Get %d-byte key: %q, %v", n, got, err)
		}
	}

	// Invalid: nothing may be written for any of them.
	before := fileSize(t, path)
	for _, key := range [][]byte{nil, {}, bytes.Repeat([]byte{'x'}, 1025), bytes.Repeat([]byte{'x'}, 5000)} {
		if err := db.Put(key, val); !errors.Is(err, engine.ErrInvalidKey) {
			t.Fatalf("Put %d-byte key: err = %v, want ErrInvalidKey", len(key), err)
		}
		if _, err := db.Get(key); !errors.Is(err, engine.ErrInvalidKey) {
			t.Fatalf("Get %d-byte key: err = %v, want ErrInvalidKey", len(key), err)
		}
		if err := db.Delete(key); !errors.Is(err, engine.ErrInvalidKey) {
			t.Fatalf("Delete %d-byte key: err = %v, want ErrInvalidKey", len(key), err)
		}
	}
	if fileSize(t, path) != before {
		t.Fatal("invalid keys changed the file")
	}
}

func TestBinarySafeKeysAndValues(t *testing.T) {
	db := openPerm(t)
	defer db.Close()

	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	// Binary tails after a run-unique prefix; "a" and "ab" are prefixes of each other.
	tails := [][]byte{
		{0}, {0, 0}, {0xFF}, []byte("a"), []byte("ab"), []byte("a\x00b"), allBytes, {'\n'}, []byte("with space"),
	}
	prefix := []byte(pk(t, ""))
	keyOf := func(i int) []byte { return append(append([]byte(nil), prefix...), tails[i]...) }
	valOf := func(i int) []byte { return append(append([]byte(nil), allBytes[i:]...), byte(i)) }

	for i := range tails {
		if err := db.Put(keyOf(i), valOf(i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range tails {
		got, err := db.Get(keyOf(i))
		if err != nil || !bytes.Equal(got, valOf(i)) {
			t.Fatalf("key tail % x: got % x, %v", tails[i], got, err)
		}
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	db := openPerm(t)
	keep, overwrite, gone := pk(t, "keep"), pk(t, "overwrite"), pk(t, "gone")
	revived, empty, big := pk(t, "revived"), pk(t, "empty"), pk(t, "big")

	put(t, db, keep, "kept")
	put(t, db, overwrite, "old")
	put(t, db, overwrite, "new")
	put(t, db, gone, "x")
	del(t, db, gone)
	put(t, db, revived, "first")
	del(t, db, revived)
	put(t, db, revived, "second")
	put(t, db, empty, "")
	put(t, db, big, string(pattern(300_000, 9)))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 3; round++ { // survive repeated restarts
		db = openPerm(t)
		wantValue(t, db, keep, "kept")
		wantValue(t, db, overwrite, "new")
		wantMissing(t, db, gone)
		wantValue(t, db, revived, "second")
		wantValue(t, db, empty, "")
		wantValue(t, db, big, string(pattern(300_000, 9)))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWritesAfterRestartContinue(t *testing.T) {
	a, b := pk(t, "a"), pk(t, "b")
	db := openPerm(t)
	put(t, db, a, "1")
	db.Close()

	db = openPerm(t)
	put(t, db, b, "2")
	put(t, db, a, "3")
	db.Close()

	db = openPerm(t)
	defer db.Close()
	wantValue(t, db, a, "3")
	wantValue(t, db, b, "2")
}

func TestUseAfterClose(t *testing.T) {
	db := openPerm(t)
	db.Close()
	k := []byte(pk(t, "k"))
	if err := db.Put(k, []byte("v")); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("Put = %v, want ErrClosed", err)
	}
	if _, err := db.Get(k); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("Get = %v, want ErrClosed", err)
	}
	if err := db.Delete(k); !errors.Is(err, engine.ErrClosed) {
		t.Fatalf("Delete = %v, want ErrClosed", err)
	}
}

// lastSeq returns the seq of the last frame in raw, or 0 if there is none.
func lastSeq(t *testing.T, raw []byte) uint64 {
	t.Helper()
	r := bytes.NewReader(raw[storage.HeaderSize:])
	var seq uint64
	for r.Len() > 0 {
		f, err := storage.ReadFrame(r, int64(r.Len()))
		if err != nil {
			t.Fatalf("existing file does not parse: %v", err)
		}
		seq = f.Seq
	}
	return seq
}

// The exact bytes a single Put appends, checked against the documented format.
// The file already has history, so the check is on the appended bytes, and
// the earlier bytes must be exactly as they were (the log only appends).
func TestOnDiskBytesOfOnePut(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	defer db.Close()

	before := readFrom(t, path, 0)
	key, val := pk(t, "k"), "vv"
	put(t, db, key, val)
	after := readFrom(t, path, 0)

	if !bytes.Equal(after[:len(before)], before) {
		t.Fatal("a Put changed bytes that were already in the file")
	}

	seq := lastSeq(t, before) + 1
	body := []byte{0x01, 0x0F, 0x7A, 0xA5} // frame_magic
	body = binary.LittleEndian.AppendUint32(body, uint32(7+len(key)+len(val)))
	body = binary.LittleEndian.AppendUint64(body, seq)
	body = binary.LittleEndian.AppendUint32(body, 1) // op_count
	body = append(body, 0x01)                        // PUT
	body = binary.LittleEndian.AppendUint16(body, uint16(len(key)))
	body = binary.LittleEndian.AppendUint32(body, uint32(len(val)))
	body = append(body, key...)
	body = append(body, val...)
	want := binary.LittleEndian.AppendUint32(body, crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)))

	if got := after[len(before):]; !bytes.Equal(got, want) {
		t.Fatalf("appended bytes differ\n got % x\nwant % x", got, want)
	}
}

// When Put returns, the complete frame is already in the file. (That the data
// reached the platter rather than the OS cache is the job of the Sync call in
// the write path; it cannot be observed from inside a running process.)
func TestAckedPutIsCompleteInFile(t *testing.T) {
	path := testdb.Path(t)
	db := openPerm(t)
	defer db.Close()

	for i := 0; i < 20; i++ {
		before := fileSize(t, path)
		key := pk(t, fmt.Sprintf("key%d", i))
		put(t, db, key, fmt.Sprintf("value%d", i))

		r := bytes.NewReader(readFrom(t, path, before)) // only what this Put appended
		f, err := storage.ReadFrame(r, int64(r.Len()))
		if err != nil {
			t.Fatalf("after Put %d the appended bytes do not parse: %v", i, err)
		}
		if r.Len() != 0 || len(f.Ops) != 1 || string(f.Ops[0].Key) != key {
			t.Fatalf("after Put %d: %d bytes left over, ops %d, key %q", i, r.Len(), len(f.Ops), f.Ops[0].Key)
		}
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	db := openPerm(t)

	const writers, perWriter = 4, 50
	prefix := pk(t, "")
	own := func(w, i int) []byte { return []byte(fmt.Sprintf("%sw%d-k%d", prefix, w, i)) }
	shared := []byte(prefix + "shared")

	var wg sync.WaitGroup
	errc := make(chan error, writers*2)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				k := own(w, i)
				if err := db.Put(k, pattern(100+i, byte(w))); err != nil {
					errc <- err
					return
				}
				// Read back our own just-acknowledged write while others write.
				got, err := db.Get(k)
				if err != nil || !bytes.Equal(got, pattern(100+i, byte(w))) {
					errc <- fmt.Errorf("read-your-write %s: %v", k, err)
					return
				}
			}
		}(w)
	}
	// Same-key contention: the final value must be one writer's whole value.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := db.Put(shared, pattern(64, byte(w))); err != nil {
					errc <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}

	check := func(db *engine.DB) {
		for w := 0; w < writers; w++ {
			for i := 0; i < perWriter; i++ {
				got, err := db.Get(own(w, i))
				if err != nil || !bytes.Equal(got, pattern(100+i, byte(w))) {
					t.Fatalf("w%d-k%d wrong: %v", w, i, err)
				}
			}
		}
		got, err := db.Get(shared)
		if err != nil {
			t.Fatal(err)
		}
		for w := 0; w < writers; w++ {
			if bytes.Equal(got, pattern(64, byte(w))) {
				return
			}
		}
		t.Fatalf("shared holds a value no writer wrote: % x", got)
	}
	check(db)
	db.Close()
	db = openPerm(t)
	defer db.Close()
	check(db)
}
