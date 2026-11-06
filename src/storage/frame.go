package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Operation types stored in the log.
const (
	OpPut uint8 = 1
	OpDel uint8 = 2 // a tombstone: the key is deleted
)

// Limits.
const (
	MaxKeyLen      = 1024
	MaxValueLen    = 1 << 20 // 1 MiB
	MaxOpsPerFrame = 10000
	MaxPayload     = 64 << 20
)

// Frame geometry. A frame is:
//
//	off   size  field
//	  0      4  frame_magic  0xA57A0F01
//	  4      4  payload_len  N, at most MaxPayload
//	  8      8  seq          previous frame's seq + 1
//	 16      4  op_count     1..MaxOpsPerFrame
//	 20      N  payload      op_count ops back to back
//	20+N     4  frame_crc    CRC32C of bytes 0..20+N-1 (header and payload)
//
// A frame is committed if and only if its whole CRC validates, so the CRC is
// the commit marker: a torn write cannot produce a valid frame. All integers
// are little-endian.
//
// An op is:
//
//	PUT: type(1)=1  key_len(2)  val_len(4)  key  value
//	DEL: type(1)=2  key_len(2)  key
const (
	FrameMagic       uint32 = 0xA57A0F01
	FrameHeaderSize         = 20
	FrameTrailerSize        = 4
	FrameOverhead           = FrameHeaderSize + FrameTrailerSize
	MaxFrameSize            = FrameOverhead + MaxPayload

	PutHeaderSize = 7 // type + key_len + val_len
	DelHeaderSize = 3 // type + key_len
)

var (
	ErrInvalidKey    = errors.New("storage: key length must be 1 to 1024 bytes")
	ErrValueTooLarge = errors.New("storage: value is larger than 1 MiB")
	ErrInvalidOp     = errors.New("storage: invalid operation")
	ErrTooManyOps    = errors.New("storage: more than 10000 operations in one frame")
	ErrFrameTooLarge = errors.New("storage: frame payload is larger than 64 MiB")

	// ErrTornFrame means the bytes at this position are not a complete,
	// checksum-valid frame. At the end of a file this is what an interrupted
	// write leaves behind.
	ErrTornFrame = errors.New("storage: incomplete or damaged frame")
	// ErrCorruptFrame means a frame passed its checksum but its contents are
	// malformed. The writer cannot produce this, so it is never treated as a
	// torn write.
	ErrCorruptFrame = errors.New("storage: frame passed its checksum but is malformed")
)

// Op is one operation. For decoded ops, Key and Value alias the frame buffer.
type Op struct {
	Type  uint8
	Key   []byte
	Value []byte // PUT only; must be empty for DEL
	// Off is the offset of this op from the start of its frame. EncodeFrame
	// and ReadFrame both set it, so live writes and replay index identically.
	Off int
}

// Frame is a decoded, checksum-valid frame.
type Frame struct {
	Seq  uint64
	Ops  []Op
	Size int64 // total bytes on disk
}

// ValidateKey checks the key length limits.
func ValidateKey(key []byte) error {
	if n := len(key); n < 1 || n > MaxKeyLen {
		return fmt.Errorf("%w (got %d)", ErrInvalidKey, n)
	}
	return nil
}

// ValidateOp checks that op can be encoded.
func ValidateOp(op Op) error {
	if err := ValidateKey(op.Key); err != nil {
		return err
	}
	switch op.Type {
	case OpPut:
		if len(op.Value) > MaxValueLen {
			return fmt.Errorf("%w (got %d)", ErrValueTooLarge, len(op.Value))
		}
	case OpDel:
		if len(op.Value) != 0 {
			return fmt.Errorf("%w: delete carries a value", ErrInvalidOp)
		}
	default:
		return fmt.Errorf("%w: type %d", ErrInvalidOp, op.Type)
	}
	return nil
}

func opSize(op Op) int {
	if op.Type == OpPut {
		return PutHeaderSize + len(op.Key) + len(op.Value)
	}
	return DelHeaderSize + len(op.Key)
}

// EncodeFrame returns the on-disk bytes of one frame holding ops, in order.
// It validates every op and sets ops[i].Off.
func EncodeFrame(seq uint64, ops []Op) ([]byte, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: frame has no operations", ErrInvalidOp)
	}
	if len(ops) > MaxOpsPerFrame {
		return nil, ErrTooManyOps
	}
	payload := 0
	for _, op := range ops {
		if err := ValidateOp(op); err != nil {
			return nil, err
		}
		payload += opSize(op)
		if payload > MaxPayload {
			return nil, ErrFrameTooLarge
		}
	}

	buf := make([]byte, FrameOverhead+payload)
	binary.LittleEndian.PutUint32(buf[0:4], FrameMagic)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(payload))
	binary.LittleEndian.PutUint64(buf[8:16], seq)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(ops)))

	p := FrameHeaderSize
	for i := range ops {
		op := &ops[i]
		op.Off = p
		buf[p] = op.Type
		binary.LittleEndian.PutUint16(buf[p+1:], uint16(len(op.Key)))
		if op.Type == OpPut {
			binary.LittleEndian.PutUint32(buf[p+3:], uint32(len(op.Value)))
			p += PutHeaderSize
			p += copy(buf[p:], op.Key)
			p += copy(buf[p:], op.Value)
		} else {
			p += DelHeaderSize
			p += copy(buf[p:], op.Key)
		}
	}

	end := FrameHeaderSize + payload
	binary.LittleEndian.PutUint32(buf[end:], crc32.Checksum(buf[:end], castagnoli))
	return buf, nil
}

// ParseOp decodes one op from the start of b and returns it with the number of
// bytes it occupies. The returned slices alias b.
func ParseOp(b []byte) (Op, int, error) {
	if len(b) < DelHeaderSize {
		return Op{}, 0, fmt.Errorf("%w: truncated op header", ErrCorruptFrame)
	}
	typ := b[0]
	klen := int(binary.LittleEndian.Uint16(b[1:3]))
	if klen < 1 || klen > MaxKeyLen {
		return Op{}, 0, fmt.Errorf("%w: key length %d", ErrCorruptFrame, klen)
	}
	switch typ {
	case OpPut:
		if len(b) < PutHeaderSize {
			return Op{}, 0, fmt.Errorf("%w: truncated put header", ErrCorruptFrame)
		}
		vlen := int(binary.LittleEndian.Uint32(b[3:7]))
		if vlen > MaxValueLen {
			return Op{}, 0, fmt.Errorf("%w: value length %d", ErrCorruptFrame, vlen)
		}
		n := PutHeaderSize + klen + vlen
		if len(b) < n {
			return Op{}, 0, fmt.Errorf("%w: put overruns payload", ErrCorruptFrame)
		}
		return Op{Type: OpPut, Key: b[PutHeaderSize : PutHeaderSize+klen], Value: b[PutHeaderSize+klen : n]}, n, nil
	case OpDel:
		n := DelHeaderSize + klen
		if len(b) < n {
			return Op{}, 0, fmt.Errorf("%w: delete overruns payload", ErrCorruptFrame)
		}
		return Op{Type: OpDel, Key: b[DelHeaderSize:n]}, n, nil
	default:
		return Op{}, 0, fmt.Errorf("%w: op type %d", ErrCorruptFrame, typ)
	}
}

// ReadFrame reads one frame from r. See FrameReader.Next for the arguments and
// errors. The returned frame owns its memory.
func ReadFrame(r io.Reader, remaining int64) (Frame, error) {
	return NewFrameReader(r).Next(remaining)
}

// FrameReader reads consecutive frames from a stream, reusing its buffers
// between frames so that replaying a long log does not allocate per frame.
type FrameReader struct {
	r    io.Reader
	hdr  [FrameHeaderSize]byte
	body []byte // payload and CRC of the current frame, reused
	ops  []Op   // ops of the current frame, reused
}

// NewFrameReader returns a FrameReader that reads from r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r}
}

// Next reads one frame. remaining is the number of bytes between the start of
// this frame and the end of the file; a frame that claims to be longer than
// that is torn.
//
// It returns io.EOF when the stream is at a clean end, ErrTornFrame for
// anything that is not a complete valid frame, ErrCorruptFrame for a
// checksum-valid frame with malformed contents, and a plain wrapped error for
// a read failure (which callers must not mistake for a torn write).
//
// The returned Frame, including Ops and every Key and Value slice inside it,
// is valid only until the next call to Next. Copy what must outlive it.
func (fr *FrameReader) Next(remaining int64) (Frame, error) {
	hdr := fr.hdr[:]
	if _, err := io.ReadFull(fr.r, hdr); err != nil {
		switch err {
		case io.EOF:
			return Frame{}, io.EOF
		case io.ErrUnexpectedEOF:
			return Frame{}, ErrTornFrame
		}
		return Frame{}, fmt.Errorf("storage: read frame header: %w", err)
	}
	if binary.LittleEndian.Uint32(hdr[0:4]) != FrameMagic {
		return Frame{}, ErrTornFrame
	}
	plen := int64(binary.LittleEndian.Uint32(hdr[4:8]))
	if plen > MaxPayload || FrameOverhead+plen > remaining {
		return Frame{}, ErrTornFrame
	}

	need := int(plen) + FrameTrailerSize
	if cap(fr.body) < need {
		fr.body = make([]byte, need)
	}
	body := fr.body[:need]
	if _, err := io.ReadFull(fr.r, body); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Frame{}, ErrTornFrame
		}
		return Frame{}, fmt.Errorf("storage: read frame body: %w", err)
	}
	payload := body[:plen]
	sum := crc32.Update(crc32.Update(0, castagnoli, hdr), castagnoli, payload)
	if sum != binary.LittleEndian.Uint32(body[plen:]) {
		return Frame{}, ErrTornFrame
	}

	count := binary.LittleEndian.Uint32(hdr[16:20])
	if count < 1 || count > MaxOpsPerFrame {
		return Frame{}, fmt.Errorf("%w: op_count %d", ErrCorruptFrame, count)
	}
	ops := fr.ops[:0]
	p := 0
	for i := uint32(0); i < count; i++ {
		op, n, err := ParseOp(payload[p:])
		if err != nil {
			return Frame{}, err
		}
		op.Off = FrameHeaderSize + p
		ops = append(ops, op)
		p += n
	}
	fr.ops = ops
	if int64(p) != plen {
		return Frame{}, fmt.Errorf("%w: %d payload bytes unused", ErrCorruptFrame, plen-int64(p))
	}
	return Frame{
		Seq:  binary.LittleEndian.Uint64(hdr[8:16]),
		Ops:  ops,
		Size: FrameOverhead + plen,
	}, nil
}
