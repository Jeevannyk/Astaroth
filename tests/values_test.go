package tests

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
)

// Self-describing values for the concurrency tests. A value names the key it
// was written for, the writer that wrote it and that writer's sequence number,
// carries a body derived from those, and ends with a CRC of everything before
// it. checkValue rejects anything else, so a torn value, a mix of two writers'
// values, or another key's value cannot pass.

func mkValue(key, writer string, n int) []byte {
	b := []byte(fmt.Sprintf("%s|%s|%d|", key, writer, n))
	b = append(b, pattern(n%257, byte(len(writer)+n))...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

// checkValue validates v as a value written for key and returns its writer and
// sequence number.
func checkValue(key string, v []byte) (writer string, n int, err error) {
	if len(v) < 4 {
		return "", 0, fmt.Errorf("value of %d bytes is too short", len(v))
	}
	body, sum := v[:len(v)-4], binary.BigEndian.Uint32(v[len(v)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return "", 0, fmt.Errorf("value checksum mismatch: torn or mixed value % x", v)
	}
	// Keys may contain '|', so match the key as a prefix, then split the rest.
	rest, ok := strings.CutPrefix(string(body), key+"|")
	if !ok {
		return "", 0, fmt.Errorf("value was not written for key %q: % x", key, v)
	}
	parts := strings.SplitN(rest, "|", 3)
	if len(parts) != 3 {
		return "", 0, fmt.Errorf("malformed value header")
	}
	n, err = strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("bad sequence number in value: %v", err)
	}
	if !bytes.Equal(mkValue(key, parts[0], n), v) {
		return "", 0, fmt.Errorf("value body does not match its header")
	}
	return parts[0], n, nil
}
