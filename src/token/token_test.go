package token

import (
	"bytes"
	"testing"
)

func TestDecode(t *testing.T) {
	cases := map[string][]byte{
		"abc":            []byte("abc"),
		"hello world":    []byte("hello world"),
		"hex:":           {},
		"hex:00ff10":     {0x00, 0xFF, 0x10},
		"hex:48656C6C6F": []byte("Hello"),  // upper case accepted
		"HEX:00":         []byte("HEX:00"), // the prefix is case-sensitive
		"-5":             []byte("-5"),
	}
	for in, want := range cases {
		got, err := Decode(in)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Decode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"hex:0", "hex:zz", "hex:0g"} {
		if _, err := Decode(bad); err == nil {
			t.Fatalf("Decode(%q) accepted invalid hex", bad)
		}
	}
}

func TestEncodingRules(t *testing.T) {
	keys := map[string]string{
		"user:42":     "user:42",
		"a b":         "hex:612062",       // space: not allowed in a key
		"":            "hex:",             //
		"hex:12":      "hex:6865783a3132", // would be misread as hex
		"\x00\xff":    "hex:00ff",
		"tab\there":   "hex:7461620968657265",
		"NOTFOUND":    "NOTFOUND",       // fine as a key: keys are never printed by get
		"caf\xc3\xa9": "hex:636166c3a9", // non-ASCII is written in hex
	}
	for in, want := range keys {
		if got := Key([]byte(in)); got != want {
			t.Fatalf("Key(%q) = %q, want %q", in, got, want)
		}
	}
	values := map[string]string{
		"hello world": "hello world",
		"":            "hex:",
		" lead":       "hex:206c656164",
		"trail ":      "hex:747261696c20",
		"NOTFOUND":    "hex:4e4f54464f554e44",
		"hex:":        "hex:6865783a",
		"line\nbreak": "hex:6c696e650a627265616b",
		"cr\r":        "hex:63720d",
	}
	for in, want := range values {
		if got := Value([]byte(in)); got != want {
			t.Fatalf("Value(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoundTripAllBytes(t *testing.T) {
	var inputs [][]byte
	for c := 0; c < 256; c++ {
		inputs = append(inputs, []byte{byte(c)}, []byte{'a', byte(c), 'b'})
	}
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	inputs = append(inputs, all, nil, []byte("hex:"), []byte("hex:00"), []byte(" "))
	for _, b := range inputs {
		for _, enc := range []func([]byte) string{Key, Value} {
			s := enc(b)
			got, err := Decode(s)
			if err != nil || !bytes.Equal(got, b) {
				t.Fatalf("round trip of % x via %q gave % x, %v", b, s, got, err)
			}
			if bytes.ContainsAny([]byte(s), "\r\n") {
				t.Fatalf("encoding of % x contains a line break: %q", b, s)
			}
		}
		if s := Key(b); bytes.ContainsRune([]byte(s), ' ') {
			t.Fatalf("key encoding of % x contains a space: %q", b, s)
		}
	}
}
