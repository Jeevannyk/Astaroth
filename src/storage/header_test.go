package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	want := Header{Version: Version, Flags: 0, Generation: 7}
	b := want.Encode()
	got, err := DecodeHeader(b[:])
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHeaderLayout(t *testing.T) {
	b := NewHeader().Encode()
	if string(b[0:8]) != "ASTAROTH" {
		t.Fatalf("magic = %q", b[0:8])
	}
	if b[8] != 1 || b[9] != 0 || b[10] != 0 || b[11] != 0 {
		t.Fatalf("version bytes = % x", b[8:12])
	}
	if b[16] != 1 {
		t.Fatalf("generation low byte = %d, want 1", b[16])
	}
	if b[28] != 0 || b[29] != 0 || b[30] != 0 || b[31] != 0 {
		t.Fatalf("reserved bytes = % x", b[28:32])
	}
}

func TestDecodeHeaderRejects(t *testing.T) {
	good := NewHeader().Encode()

	t.Run("short", func(t *testing.T) {
		for n := 0; n < HeaderSize; n++ {
			if _, err := DecodeHeader(good[:n]); !errors.Is(err, ErrShortHeader) {
				t.Fatalf("len %d: err = %v, want ErrShortHeader", n, err)
			}
		}
	})

	t.Run("bad magic", func(t *testing.T) {
		b := good
		b[0] ^= 0xFF
		if _, err := DecodeHeader(b[:]); !errors.Is(err, ErrBadMagic) {
			t.Fatalf("err = %v, want ErrBadMagic", err)
		}
	})

	t.Run("any flipped byte in CRC-covered range", func(t *testing.T) {
		for i := 8; i < 24; i++ {
			b := good
			b[i] ^= 0x01
			if _, err := DecodeHeader(b[:]); !errors.Is(err, ErrBadHeaderCRC) {
				t.Fatalf("byte %d: err = %v, want ErrBadHeaderCRC", i, err)
			}
		}
	})

	t.Run("corrupt crc field", func(t *testing.T) {
		b := good
		b[24] ^= 0x01
		if _, err := DecodeHeader(b[:]); !errors.Is(err, ErrBadHeaderCRC) {
			t.Fatalf("err = %v, want ErrBadHeaderCRC", err)
		}
	})

	t.Run("unsupported version with valid crc", func(t *testing.T) {
		b := Header{Version: 2, Generation: 1}.Encode()
		if _, err := DecodeHeader(b[:]); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
		}
	})
}

func TestOpenFileCreatesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != 0 {
		t.Fatalf("size = %d, want 0", st.Size())
	}
}

func TestOpenFileMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "db")
	if f, err := OpenFile(path); err == nil {
		f.Close()
		t.Fatal("OpenFile succeeded in a missing directory")
	}
}

func TestWriteHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := WriteHeader(f, NewHeader()); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != HeaderSize {
		t.Fatalf("file size = %d, want %d", len(raw), HeaderSize)
	}
	if _, err := DecodeHeader(raw); err != nil {
		t.Fatalf("DecodeHeader(file): %v", err)
	}
}
