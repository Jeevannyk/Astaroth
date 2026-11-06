package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"astaroth/src/index"
	"astaroth/src/storage"
)

func openTemp(t *testing.T, initial []byte) (*os.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	if initial != nil {
		if err := os.WriteFile(path, initial, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := storage.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, path
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func recoverFile(f *os.File) (Result, *index.Index, error) {
	idx := index.New()
	res, err := Recover(f, idx)
	return res, idx, err
}

func header() []byte {
	h := storage.NewHeader().Encode()
	return h[:]
}

func frame(t *testing.T, seq uint64, ops ...storage.Op) []byte {
	t.Helper()
	b, err := storage.EncodeFrame(seq, ops)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func put(k, v string) storage.Op {
	return storage.Op{Type: storage.OpPut, Key: []byte(k), Value: []byte(v)}
}
func del(k string) storage.Op { return storage.Op{Type: storage.OpDel, Key: []byte(k)} }

func cat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// --- header handling ---------------------------------------------------

func TestRecoverEmptyFileWritesHeader(t *testing.T) {
	f, path := openTemp(t, nil)
	res, idx, err := recoverFile(f)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !res.Created || res.End != storage.HeaderSize || res.NextSeq != 1 || res.Header.Generation != 1 || idx.Len() != 0 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := storage.DecodeHeader(fileBytes(t, path)); err != nil {
		t.Fatalf("header on disk invalid: %v", err)
	}
}

// A kill during creation can leave any prefix of the header, or zeros.
func TestRecoverTornHeaderPrefixes(t *testing.T) {
	want := header()
	for n := 0; n < storage.HeaderSize; n++ {
		f, path := openTemp(t, want[:n])
		res, _, err := recoverFile(f)
		if err != nil {
			t.Fatalf("prefix %d: Recover: %v", n, err)
		}
		if !res.Created {
			t.Fatalf("prefix %d: Created = false", n)
		}
		if got := fileBytes(t, path); !bytes.Equal(got, want) {
			t.Fatalf("prefix %d: header not repaired: % x", n, got)
		}
	}
}

func TestRecoverZeroFilledShortFile(t *testing.T) {
	f, path := openTemp(t, make([]byte, 10))
	if _, _, err := recoverFile(f); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := storage.DecodeHeader(fileBytes(t, path)); err != nil {
		t.Fatalf("header not repaired: %v", err)
	}
}

func TestRecoverShortForeignFile(t *testing.T) {
	f, path := openTemp(t, []byte("hello"))
	if _, _, err := recoverFile(f); !errors.Is(err, ErrNotOurFile) {
		t.Fatalf("err = %v, want ErrNotOurFile", err)
	}
	if got := fileBytes(t, path); string(got) != "hello" {
		t.Fatalf("foreign file was modified: %q", got)
	}
}

func TestRecoverValidHeaderIsIdempotent(t *testing.T) {
	h := storage.Header{Version: storage.Version, Generation: 5}.Encode()
	f, path := openTemp(t, h[:])
	for i := 0; i < 3; i++ {
		res, _, err := recoverFile(f)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if res.Created || res.End != storage.HeaderSize || res.Header.Generation != 5 {
			t.Fatalf("run %d: result = %+v", i, res)
		}
	}
	if got := fileBytes(t, path); !bytes.Equal(got, h[:]) {
		t.Fatal("recovery modified a valid file")
	}
}

func TestRecoverRejectsBadHeaders(t *testing.T) {
	good := header()
	badMagic := append([]byte(nil), good...)
	badMagic[0] ^= 0xFF
	badCRC := append([]byte(nil), good...)
	badCRC[16] ^= 0x01
	badVersion := storage.Header{Version: 9, Generation: 1}.Encode()
	garbage := bytes.Repeat([]byte{0xAB}, 100)

	cases := map[string][]byte{
		"bad magic":   badMagic,
		"bad crc":     badCRC,
		"bad version": badVersion[:],
		"garbage":     garbage,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			f, path := openTemp(t, content)
			if _, _, err := recoverFile(f); !errors.Is(err, ErrNotOurFile) {
				t.Fatalf("err = %v, want ErrNotOurFile", err)
			}
			if got := fileBytes(t, path); !bytes.Equal(got, content) {
				t.Fatal("rejected file was modified")
			}
		})
	}
}

// --- replay ------------------------------------------------------------

func TestReplayRebuildsIndex(t *testing.T) {
	f1 := frame(t, 1, put("a", "1"))
	f2 := frame(t, 2, put("b", "22"))
	f3 := frame(t, 3, put("a", "333"))
	f4 := frame(t, 4, del("b"))
	content := cat(header(), f1, f2, f3, f4)
	f, path := openTemp(t, content)

	res, idx, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Frames != 4 || res.NextSeq != 5 || res.End != int64(len(content)) || res.TruncatedBytes != 0 {
		t.Fatalf("result = %+v", res)
	}
	if idx.Len() != 1 {
		t.Fatalf("Len = %d, want 1", idx.Len())
	}
	e, ok := idx.Get([]byte("a"))
	if !ok || e.ValLen != 3 {
		t.Fatalf("a = %+v, %v", e, ok)
	}
	// The entry must point at the PUT op of f3, i.e. its type byte.
	wantOff := int64(storage.HeaderSize + len(f1) + len(f2) + storage.FrameHeaderSize)
	if e.RecOff != wantOff || content[e.RecOff] != storage.OpPut {
		t.Fatalf("a.RecOff = %d, want %d", e.RecOff, wantOff)
	}
	if _, ok := idx.Get([]byte("b")); ok {
		t.Fatal("deleted key b is still indexed")
	}
	if !bytes.Equal(fileBytes(t, path), content) {
		t.Fatal("clean replay modified the file")
	}
}

// Every way a write can be cut short leaves a tail that must be removed.
func TestTornTailIsTruncated(t *testing.T) {
	f1 := frame(t, 1, put("a", "1"))
	f2 := frame(t, 2, put("b", "2"))
	base := cat(header(), f1)

	for cut := 0; cut < len(f2); cut++ {
		content := cat(base, f2[:cut])
		f, path := openTemp(t, content)
		res, idx, err := recoverFile(f)
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if res.End != int64(len(base)) || res.TruncatedBytes != int64(cut) || res.NextSeq != 2 || idx.Len() != 1 {
			t.Fatalf("cut %d: result = %+v, Len = %d", cut, res, idx.Len())
		}
		if got := fileBytes(t, path); !bytes.Equal(got, base) {
			t.Fatalf("cut %d: file not truncated to last good frame (len %d, want %d)", cut, len(got), len(base))
		}
	}
}

func TestRecoveryIsIdempotent(t *testing.T) {
	f1 := frame(t, 1, put("a", "1"))
	f2 := frame(t, 2, put("b", "2"))
	f, path := openTemp(t, cat(header(), f1, f2[:len(f2)-1]))

	first, idx1, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	after1 := fileBytes(t, path)

	second, idx2, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if first.End != second.End || first.NextSeq != second.NextSeq || idx1.Len() != idx2.Len() {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	if second.TruncatedBytes != 0 || !bytes.Equal(after1, fileBytes(t, path)) {
		t.Fatal("second recovery changed the file")
	}
}

func TestGarbageAfterHeaderIsTornTail(t *testing.T) {
	f, path := openTemp(t, cat(header(), []byte{1, 2, 3}))
	res, idx, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.End != storage.HeaderSize || res.TruncatedBytes != 3 || idx.Len() != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !bytes.Equal(fileBytes(t, path), header()) {
		t.Fatal("garbage not removed")
	}
}

func TestZeroFilledTailIsTruncated(t *testing.T) {
	f1 := frame(t, 1, put("a", "1"))
	f, path := openTemp(t, cat(header(), f1, make([]byte, 4096)))
	res, _, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.TruncatedBytes != 4096 || res.Frames != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !bytes.Equal(fileBytes(t, path), cat(header(), f1)) {
		t.Fatal("zero tail not removed")
	}
}

// After truncation the next append position is exact: a frame written there
// is found by the next recovery (nothing hides behind leftover garbage).
func TestAppendAfterTruncationIsVisible(t *testing.T) {
	f1 := frame(t, 1, put("a", "1"))
	f2 := frame(t, 2, put("b", "2"))
	f, path := openTemp(t, cat(header(), f1, f2[:10]))

	res, _, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	next := frame(t, res.NextSeq, put("c", "3"))
	if _, err := f.WriteAt(next, res.End); err != nil {
		t.Fatal(err)
	}

	f.Close()
	f2h, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f2h.Close()
	res2, idx, err := recoverFile(f2h)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Frames != 2 || idx.Len() != 2 {
		t.Fatalf("result = %+v, Len = %d", res2, idx.Len())
	}
	if _, ok := idx.Get([]byte("c")); !ok {
		t.Fatal("frame appended after truncation was lost")
	}
}

// --- things that are not torn tails -------------------------------------

func TestSeqGapIsCorrupt(t *testing.T) {
	content := cat(header(), frame(t, 1, put("a", "1")), frame(t, 3, put("b", "2")))
	f, path := openTemp(t, content)
	if _, _, err := recoverFile(f); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	if !bytes.Equal(fileBytes(t, path), content) {
		t.Fatal("file was modified")
	}
}

func TestChecksumValidButMalformedFrameIsCorrupt(t *testing.T) {
	// op_count 1 but the payload is a PUT with key_len 0, with a correct CRC.
	payload := []byte{storage.OpPut, 0, 0, 0, 0, 0, 0}
	raw := make([]byte, storage.FrameOverhead+len(payload))
	binary.LittleEndian.PutUint32(raw[0:4], storage.FrameMagic)
	binary.LittleEndian.PutUint32(raw[4:8], uint32(len(payload)))
	binary.LittleEndian.PutUint64(raw[8:16], 1)
	binary.LittleEndian.PutUint32(raw[16:20], 1)
	copy(raw[storage.FrameHeaderSize:], payload)
	end := storage.FrameHeaderSize + len(payload)
	binary.LittleEndian.PutUint32(raw[end:], crc32.Checksum(raw[:end], crc32.MakeTable(crc32.Castagnoli)))

	content := cat(header(), raw)
	f, path := openTemp(t, content)
	if _, _, err := recoverFile(f); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	if !bytes.Equal(fileBytes(t, path), content) {
		t.Fatal("recovery truncated checksum-valid data")
	}
}

// More invalid bytes than one interrupted write could leave: refuse to delete.
func TestOversizedInvalidRegionIsNotTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 64 MiB")
	}
	content := cat(header(), make([]byte, storage.MaxFrameSize+1))
	f, path := openTemp(t, content)
	if _, _, err := recoverFile(f); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	if st, _ := os.Stat(path); st.Size() != int64(len(content)) {
		t.Fatal("recovery truncated an oversized invalid region")
	}
}

// --- replay order ----------------------------------------------------------

// valueAt reads the value of the PUT record the index points at, straight from
// the file, so the test checks the offsets as well as the keys.
func valueAt(t *testing.T, raw []byte, idx *index.Index, key string) (string, bool) {
	t.Helper()
	e, ok := idx.Get([]byte(key))
	if !ok {
		return "", false
	}
	op, _, err := storage.ParseOp(raw[e.RecOff:])
	if err != nil || op.Type != storage.OpPut || string(op.Key) != key || len(op.Value) != int(e.ValLen) {
		t.Fatalf("index entry for %q points at a wrong record (%+v, %v)", key, op, err)
	}
	return string(op.Value), true
}

// Frames replay in file order, and ops within a frame in list order: the last
// op on a key wins, whether it is a put or a delete (spec scenario B5).
func TestReplayOrderWithinAndAcrossFrames(t *testing.T) {
	raw := cat(header(),
		frame(t, 1, put("x", "1"), del("x")),                   // put then delete in one batch: absent
		frame(t, 2, del("y"), put("y", "2")),                   // delete then put in one batch: present
		frame(t, 3, put("z", "1")),                             //
		frame(t, 4, put("z", "2"), put("z", "3")),              // later op in a frame wins
		frame(t, 5, put("w", "1")),                             //
		frame(t, 6, del("w")),                                  // delete in a later frame
		frame(t, 7, put("w", "9")),                             // and revived after that
		frame(t, 8, put("gone", "g")),                          //
		frame(t, 9, put("a", "A"), del("gone"), put("b", "B")), // mixed frame
	)
	f, _ := openTemp(t, raw)
	res, idx, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Frames != 9 || res.NextSeq != 10 {
		t.Fatalf("result = %+v", res)
	}
	want := map[string]string{"y": "2", "z": "3", "w": "9", "a": "A", "b": "B"}
	for k, v := range want {
		if got, ok := valueAt(t, raw, idx, k); !ok || got != v {
			t.Fatalf("%s = %q, %v; want %q", k, got, ok, v)
		}
	}
	for _, k := range []string{"x", "gone"} {
		if _, ok := idx.Get([]byte(k)); ok {
			t.Fatalf("%s should be deleted", k)
		}
	}
	if idx.Len() != len(want) {
		t.Fatalf("Len = %d, want %d", idx.Len(), len(want))
	}
}

// Recovering the same file any number of times gives the same index and the
// same bytes, including when the first recovery had to cut a torn tail.
func TestRecoveryRepeatedGivesIdenticalResult(t *testing.T) {
	f2 := frame(t, 2, put("b", "22"), del("a"))
	raw := cat(header(), frame(t, 1, put("a", "1")), f2, frame(t, 3, put("c", "3"))[:20])
	f, path := openTemp(t, raw)

	var firstBytes []byte
	var first Result
	for i := 0; i < 5; i++ {
		res, idx, err := recoverFile(f)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got := fileBytes(t, path)
		if i == 0 {
			first, firstBytes = res, got
			if res.TruncatedBytes != 20 {
				t.Fatalf("first run truncated %d bytes, want 20", res.TruncatedBytes)
			}
		} else {
			res.TruncatedBytes = first.TruncatedBytes // only the first run has anything to cut
			if res != first {
				t.Fatalf("run %d: result %+v differs from first %+v", i, res, first)
			}
			if !bytes.Equal(got, firstBytes) {
				t.Fatalf("run %d changed the file", i)
			}
		}
		if _, ok := idx.Get([]byte("a")); ok || idx.Len() != 1 {
			t.Fatalf("run %d: wrong index content (Len %d)", i, idx.Len())
		}
		if v, ok := valueAt(t, got, idx, "b"); !ok || v != "22" {
			t.Fatalf("run %d: b = %q, %v", i, v, ok)
		}
	}
}

// A log bigger than the sample size takes the index-reservation path. The
// content must come out exactly as it would without it, overwrites and deletes
// included, and the estimate must not be fooled by an overwrite-heavy log.
func TestReplayPastSampleSizeIsExact(t *testing.T) {
	val := string(bytes.Repeat([]byte{'v'}, 100))
	var parts [][]byte
	parts = append(parts, header())
	seq := uint64(1)
	add := func(ops ...storage.Op) {
		parts = append(parts, frame(t, seq, ops...))
		seq++
	}
	const n = 20000 // about 3 MiB of single-op frames
	for i := 0; i < n; i++ {
		add(put(fmt.Sprintf("key%06d", i), val))
	}
	for i := 0; i < n; i += 2 { // overwrite every other key
		add(put(fmt.Sprintf("key%06d", i), "new"))
	}
	for i := 0; i < n; i += 5 { // delete every fifth
		add(del(fmt.Sprintf("key%06d", i)))
	}
	raw := cat(parts...)
	if len(raw) < 3*sampleBytes {
		t.Fatalf("test log is only %d bytes; it must exceed the %d-byte sample", len(raw), sampleBytes)
	}
	f, _ := openTemp(t, raw)

	res, idx, err := recoverFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Frames != int(seq-1) || res.TruncatedBytes != 0 {
		t.Fatalf("result = %+v", res)
	}
	want := 0
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key%06d", i)
		got, ok := valueAt(t, raw, idx, key)
		switch {
		case i%5 == 0:
			if ok {
				t.Fatalf("%s should be deleted", key)
			}
		case i%2 == 0:
			want++
			if !ok || got != "new" {
				t.Fatalf("%s = %q, %v; want new", key, got, ok)
			}
		default:
			want++
			if !ok || got != val {
				t.Fatalf("%s = %q, %v; want the original value", key, got, ok)
			}
		}
	}
	if idx.Len() != want {
		t.Fatalf("Len = %d, want %d", idx.Len(), want)
	}
}

// --- Check: the read-only verification behind `kvdb check` ------------------

func checkFile(t *testing.T, content []byte) (Report, error, []byte) {
	t.Helper()
	f, path := openTemp(t, content)
	rep, err := Check(f)
	return rep, err, fileBytes(t, path)
}

func TestCheckCleanLog(t *testing.T) {
	content := cat(header(), frame(t, 1, put("a", "1")), frame(t, 2, put("b", "22"), del("a")))
	rep, err, after := checkFile(t, content)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Frames != 2 || rep.Keys != 1 || rep.TornTail != 0 || rep.End != int64(len(content)) || rep.HeaderIncomplete {
		t.Fatalf("report = %+v", rep)
	}
	if !bytes.Equal(after, content) {
		t.Fatal("Check modified the file")
	}
}

// A torn tail is reported, and left in place: only open repairs.
func TestCheckReportsTornTailWithoutRemovingIt(t *testing.T) {
	good := cat(header(), frame(t, 1, put("a", "1")))
	content := cat(good, frame(t, 2, put("b", "2"))[:9])
	rep, err, after := checkFile(t, content)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TornTail != 9 || rep.End != int64(len(good)) || rep.Keys != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if !bytes.Equal(after, content) {
		t.Fatal("Check removed the torn tail; it must only read")
	}
}

func TestCheckIncompleteHeader(t *testing.T) {
	h := header()
	rep, err, after := checkFile(t, h[:7])
	if err != nil || !rep.HeaderIncomplete {
		t.Fatalf("report = %+v, err = %v", rep, err)
	}
	if !bytes.Equal(after, h[:7]) {
		t.Fatal("Check rewrote the header")
	}
}

func TestCheckRejectsDamage(t *testing.T) {
	if _, err, _ := checkFile(t, []byte("not a database at all, longer than a header")); !errors.Is(err, ErrNotOurFile) {
		t.Fatalf("foreign file: err = %v", err)
	}
	gap := cat(header(), frame(t, 1, put("a", "1")), frame(t, 5, put("b", "2")))
	if _, err, _ := checkFile(t, gap); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("sequence gap: err = %v", err)
	}
}
