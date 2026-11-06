// Package storage defines the on-disk format and opens the data file.
// It performs no recovery and holds no state.
package storage

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	// HeaderSize is the fixed size of the file header in bytes.
	HeaderSize = 32
	// Magic identifies an Astaroth data file.
	Magic = "ASTAROTH"
	// Version is the only on-disk format version this code reads and writes.
	Version = 1
)

var (
	ErrShortHeader        = errors.New("storage: header is shorter than 32 bytes")
	ErrBadMagic           = errors.New("storage: bad magic")
	ErrBadHeaderCRC       = errors.New("storage: header checksum mismatch")
	ErrUnsupportedVersion = errors.New("storage: unsupported format version")
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Header is the 32-byte file header. Layout (little-endian):
//
//	off size field
//	  0    8 magic       "ASTAROTH"
//	  8    4 version     1
//	 12    4 flags       0
//	 16    8 generation  1 at creation, +1 per compaction
//	 24    4 header_crc  CRC32C of bytes 0..23
//	 28    4 reserved    0
type Header struct {
	Version    uint32
	Flags      uint32
	Generation uint64
}

// NewHeader returns the header written into a freshly created file.
func NewHeader() Header {
	return Header{Version: Version, Generation: 1}
}

// Encode returns the 32-byte on-disk form of h.
func (h Header) Encode() [HeaderSize]byte {
	var b [HeaderSize]byte
	copy(b[0:8], Magic)
	binary.LittleEndian.PutUint32(b[8:12], h.Version)
	binary.LittleEndian.PutUint32(b[12:16], h.Flags)
	binary.LittleEndian.PutUint64(b[16:24], h.Generation)
	binary.LittleEndian.PutUint32(b[24:28], crc32.Checksum(b[0:24], castagnoli))
	return b
}

// DecodeHeader parses and validates the first HeaderSize bytes of b.
func DecodeHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, ErrShortHeader
	}
	if string(b[0:8]) != Magic {
		return Header{}, ErrBadMagic
	}
	if binary.LittleEndian.Uint32(b[24:28]) != crc32.Checksum(b[0:24], castagnoli) {
		return Header{}, ErrBadHeaderCRC
	}
	h := Header{
		Version:    binary.LittleEndian.Uint32(b[8:12]),
		Flags:      binary.LittleEndian.Uint32(b[12:16]),
		Generation: binary.LittleEndian.Uint64(b[16:24]),
	}
	if h.Version != Version {
		return Header{}, ErrUnsupportedVersion
	}
	return h, nil
}
