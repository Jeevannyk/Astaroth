package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"
)

func putOp(k, v string) Op { return Op{Type: OpPut, Key: []byte(k), Value: []byte(v)} }
func delOp(k string) Op    { return Op{Type: OpDel, Key: []byte(k)} }

func mustEncode(t *testing.T, seq uint64, ops ...Op) []byte {
	t.Helper()
	b, err := EncodeFrame(seq, ops)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	return b
}

func readOne(b []byte) (Frame, error) {
	return ReadFrame(bytes.NewReader(b), int64(len(b)))
}

// rawFrame builds a frame with a correct CRC around an arbitrary payload, so
// tests can make checksum-valid frames that the encoder would refuse to write.
func rawFrame(seq uint64, count uint32, payload []byte) []byte {
	b := make([]byte, FrameOverhead+len(payload))
	binary.LittleEndian.PutUint32(b[0:4], FrameMagic)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(payload)))
	binary.LittleEndian.PutUint64(b[8:16], seq)
	binary.LittleEndian.PutUint32(b[16:20], count)
	copy(b[FrameHeaderSize:], payload)
	end := FrameHeaderSize + len(payload)
	binary.LittleEndian.PutUint32(b[end:], crc32.Checksum(b[:end], castagnoli))
	return b
}

// Exact bytes of a one-op frame, written out by hand from the format table.
func TestEncodeFrameGoldenBytes(t *testing.T) {
	got := mustEncode(t, 7, putOp("ab", "xyz"))

	body := []byte{
		0x01, 0x0F, 0x7A, 0xA5, // frame_magic 0xA57A0F01
		12, 0, 0, 0, // payload_len = 7 + 2 + 3
		7, 0, 0, 0, 0, 0, 0, 0, // seq
		1, 0, 0, 0, // op_count
		0x01, // PUT
		2, 0, // key_len
		3, 0, 0, 0, // val_len
		'a', 'b', 'x', 'y', 'z',
	}
	crc := crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	want := binary.LittleEndian.AppendUint32(body, crc)

	if !bytes.Equal(got, want) {
		t.Fatalf("frame bytes differ\n got % x\nwant % x", got, want)
	}
}

func TestEncodeDeleteGoldenBytes(t *testing.T) {
	got := mustEncode(t, 1, delOp("k"))
	if len(got) != FrameOverhead+DelHeaderSize+1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[FrameHeaderSize] != OpDel || got[FrameHeaderSize+1] != 1 || got[FrameHeaderSize+2] != 0 || got[FrameHeaderSize+3] != 'k' {
		t.Fatalf("delete op bytes = % x", got[FrameHeaderSize:FrameHeaderSize+4])
	}
}

func TestRoundTripMultiOp(t *testing.T) {
	ops := []Op{putOp("a", "1"), delOp("b"), putOp("c", ""), putOp("dd", "value")}
	b := mustEncode(t, 42, ops...)
	f, err := readOne(b)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if f.Seq != 42 || f.Size != int64(len(b)) || len(f.Ops) != len(ops) {
		t.Fatalf("frame = seq %d size %d ops %d", f.Seq, f.Size, len(f.Ops))
	}
	for i, op := range f.Ops {
		if op.Type != ops[i].Type || !bytes.Equal(op.Key, ops[i].Key) || !bytes.Equal(op.Value, ops[i].Value) {
			t.Fatalf("op %d = %+v, want %+v", i, op, ops[i])
		}
		if op.Off != ops[i].Off {
			t.Fatalf("op %d: decoder Off %d != encoder Off %d", i, op.Off, ops[i].Off)
		}
	}
	// Op.Off must point at the op's type byte.
	if b[f.Ops[3].Off] != OpPut {
		t.Fatal("Off does not point at the op")
	}
}

func TestMaxSizedOpsRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{'k'}, MaxKeyLen)
	val := bytes.Repeat([]byte{0xEE}, MaxValueLen)
	b := mustEncode(t, 1, Op{Type: OpPut, Key: key, Value: val})
	f, err := readOne(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.Ops[0].Key, key) || !bytes.Equal(f.Ops[0].Value, val) {
		t.Fatal("max-sized key/value changed in round trip")
	}
}

func TestValidation(t *testing.T) {
	k1024 := bytes.Repeat([]byte{'a'}, MaxKeyLen)
	k1025 := bytes.Repeat([]byte{'a'}, MaxKeyLen+1)
	big := make([]byte, MaxValueLen+1)

	cases := []struct {
		name string
		op   Op
		want error
	}{
		{"nil key", Op{Type: OpPut, Key: nil}, ErrInvalidKey},
		{"empty key", Op{Type: OpPut, Key: []byte{}}, ErrInvalidKey},
		{"1025-byte key", Op{Type: OpPut, Key: k1025}, ErrInvalidKey},
		{"1025-byte delete key", Op{Type: OpDel, Key: k1025}, ErrInvalidKey},
		{"value over 1 MiB", Op{Type: OpPut, Key: []byte("k"), Value: big}, ErrValueTooLarge},
		{"delete with value", Op{Type: OpDel, Key: []byte("k"), Value: []byte("v")}, ErrInvalidOp},
		{"bad type", Op{Type: 9, Key: []byte("k")}, ErrInvalidOp},
		{"zero type", Op{Key: []byte("k")}, ErrInvalidOp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateOp(c.op); !errors.Is(err, c.want) {
				t.Fatalf("ValidateOp = %v, want %v", err, c.want)
			}
			if _, err := EncodeFrame(1, []Op{c.op}); !errors.Is(err, c.want) {
				t.Fatalf("EncodeFrame = %v, want %v", err, c.want)
			}
		})
	}

	for _, ok := range []Op{
		{Type: OpPut, Key: []byte("k")}, // empty value is legal
		{Type: OpPut, Key: k1024, Value: make([]byte, MaxValueLen)},
		{Type: OpDel, Key: k1024},
	} {
		if err := ValidateOp(ok); err != nil {
			t.Fatalf("ValidateOp(%d-byte key, %d-byte value) = %v", len(ok.Key), len(ok.Value), err)
		}
	}
}

func TestEncodeFrameOpCountLimits(t *testing.T) {
	if _, err := EncodeFrame(1, nil); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("no ops: %v", err)
	}
	many := make([]Op, MaxOpsPerFrame+1)
	for i := range many {
		many[i] = delOp("k")
	}
	if _, err := EncodeFrame(1, many); !errors.Is(err, ErrTooManyOps) {
		t.Fatalf("10001 ops: %v", err)
	}
	if _, err := EncodeFrame(1, many[:MaxOpsPerFrame]); err != nil {
		t.Fatalf("10000 ops: %v", err)
	}
}

func TestEncodeFrameTooLarge(t *testing.T) {
	big := bytes.Repeat([]byte{1}, MaxValueLen)
	ops := make([]Op, 65) // 65 x ~1 MiB > 64 MiB
	for i := range ops {
		ops[i] = Op{Type: OpPut, Key: []byte("k"), Value: big}
	}
	if _, err := EncodeFrame(1, ops); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameCleanEOF(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil), 0); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// A write cut off at any byte must read as torn, never as a frame.
func TestEveryTruncationIsTorn(t *testing.T) {
	b := mustEncode(t, 3, putOp("key", "some value"), delOp("other"))
	for n := 1; n < len(b); n++ {
		if _, err := readOne(b[:n]); !errors.Is(err, ErrTornFrame) {
			t.Fatalf("truncated to %d of %d bytes: err = %v, want ErrTornFrame", n, len(b), err)
		}
	}
}

// Damage to any byte must be detected, whether it hits magic, length, seq,
// count, payload or the checksum itself.
func TestEveryFlippedByteIsDetected(t *testing.T) {
	b := mustEncode(t, 3, putOp("key", "some value"), delOp("other"))
	for i := range b {
		for _, mask := range []byte{0x01, 0x80, 0xFF} {
			c := append([]byte(nil), b...)
			c[i] ^= mask
			f, err := readOne(c)
			if err == nil {
				t.Fatalf("byte %d ^ %#x: damaged frame accepted: %+v", i, mask, f)
			}
			if !errors.Is(err, ErrTornFrame) {
				t.Fatalf("byte %d ^ %#x: err = %v, want ErrTornFrame", i, mask, err)
			}
		}
	}
}

func TestReadFrameClaimedLengthBeyondFile(t *testing.T) {
	b := mustEncode(t, 1, putOp("k", "v"))
	// The reader has the whole frame, but the file is said to end earlier.
	_, err := ReadFrame(bytes.NewReader(b), int64(len(b)-1))
	if !errors.Is(err, ErrTornFrame) {
		t.Fatalf("err = %v, want ErrTornFrame", err)
	}
}

func TestReadFrameZeroFilledTail(t *testing.T) {
	if _, err := readOne(make([]byte, 100)); !errors.Is(err, ErrTornFrame) {
		t.Fatalf("err = %v, want ErrTornFrame", err)
	}
}

func TestReadFrameHugeLengthIsTorn(t *testing.T) {
	b := mustEncode(t, 1, putOp("k", "v"))
	binary.LittleEndian.PutUint32(b[4:8], 0xFFFFFFFF)
	if _, err := readOne(b); !errors.Is(err, ErrTornFrame) {
		t.Fatalf("err = %v, want ErrTornFrame", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

// A read failure is not a torn write; recovery must never truncate because of one.
func TestReadErrorIsNotTorn(t *testing.T) {
	_, err := ReadFrame(failingReader{}, 1000)
	if err == nil || errors.Is(err, ErrTornFrame) || errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want a plain read error", err)
	}
}

// Frames whose checksum is valid but whose contents are malformed.
func TestChecksumValidButMalformedIsCorrupt(t *testing.T) {
	put := func(klen uint16, vlen uint32, key, val string) []byte {
		p := []byte{OpPut}
		p = binary.LittleEndian.AppendUint16(p, klen)
		p = binary.LittleEndian.AppendUint32(p, vlen)
		return append(append(p, key...), val...)
	}
	cases := map[string][]byte{
		"zero op_count":     rawFrame(1, 0, put(1, 1, "k", "v")),
		"op_count too big":  rawFrame(1, MaxOpsPerFrame+1, put(1, 1, "k", "v")),
		"zero key length":   rawFrame(1, 1, put(0, 1, "", "v")),
		"key length > 1024": rawFrame(1, 1, put(1025, 0, string(make([]byte, 1025)), "")),
		"value > 1 MiB":     rawFrame(1, 1, put(1, MaxValueLen+1, "k", "")),
		"put overruns":      rawFrame(1, 1, put(1, 5, "k", "ab")),
		"unknown type":      rawFrame(1, 1, []byte{9, 1, 0, 'k'}),
		"delete overruns":   rawFrame(1, 1, []byte{OpDel, 5, 0, 'k'}),
		"count too high":    rawFrame(1, 2, put(1, 1, "k", "v")),
		"trailing bytes":    rawFrame(1, 1, append(put(1, 1, "k", "v"), 0)),
		"empty payload":     rawFrame(1, 1, nil),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := readOne(b); !errors.Is(err, ErrCorruptFrame) {
				t.Fatalf("err = %v, want ErrCorruptFrame", err)
			}
		})
	}
}

func TestReadFramesBackToBack(t *testing.T) {
	var file []byte
	for i := uint64(1); i <= 5; i++ {
		file = append(file, mustEncode(t, i, putOp("k", "v"))...)
	}
	r := bytes.NewReader(file)
	for i := uint64(1); i <= 5; i++ {
		f, err := ReadFrame(r, int64(r.Len()))
		if err != nil || f.Seq != i {
			t.Fatalf("frame %d: seq %d err %v", i, f.Seq, err)
		}
	}
	if _, err := ReadFrame(r, int64(r.Len())); err != io.EOF {
		t.Fatalf("after last frame: err = %v, want io.EOF", err)
	}
}
