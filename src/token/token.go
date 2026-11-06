// Package token is the text form of keys and values, used wherever they cross
// a text boundary: command-line arguments, batch files, the serve protocol,
// and the output of get and dump. Keys and values are arbitrary bytes inside
// the database; this encoding only decides how they are written as text.
//
// Input: a token that starts with "hex:" is the bytes of the hexadecimal digits
// that follow ("hex:" alone is the empty value). Any other token is taken
// literally, byte for byte.
//
// Output: a key or value is written literally when that is unambiguous and
// readable, otherwise as "hex:" and lowercase hex digits. Decode(Key(b)) and
// Decode(Value(b)) always give back b.
package token

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// HexPrefix marks a hex-encoded token.
const HexPrefix = "hex:"

// NotFound is what get prints for a missing key, so no value is printed as
// this literal text.
const NotFound = "NOTFOUND"

// Decode returns the bytes a token stands for.
func Decode(s string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(s, HexPrefix); ok {
		b, err := hex.DecodeString(rest)
		if err != nil {
			return nil, fmt.Errorf("token %q: invalid hex after %q", s, HexPrefix)
		}
		return b, nil
	}
	return []byte(s), nil
}

// Key returns the text form of a key. It is written literally if it is made
// only of printable ASCII characters other than space (so it can be the first
// field of a line), else in hex.
func Key(b []byte) string {
	if len(b) > 0 && !hasHexPrefix(b) && allIn(b, 0x21, 0x7E) {
		return string(b)
	}
	return HexPrefix + hex.EncodeToString(b)
}

// Value returns the text form of a value. It is written literally if it is
// printable ASCII (spaces allowed inside, since a value is always the rest of
// its line), does not start or end with a space, and is not the word
// NOTFOUND; else in hex. The empty value is "hex:".
func Value(b []byte) string {
	if len(b) > 0 && !hasHexPrefix(b) && allIn(b, 0x20, 0x7E) &&
		b[0] != ' ' && b[len(b)-1] != ' ' && string(b) != NotFound {
		return string(b)
	}
	return HexPrefix + hex.EncodeToString(b)
}

func hasHexPrefix(b []byte) bool {
	return len(b) >= len(HexPrefix) && string(b[:len(HexPrefix)]) == HexPrefix
}

func allIn(b []byte, lo, hi byte) bool {
	for _, c := range b {
		if c < lo || c > hi {
			return false
		}
	}
	return true
}
