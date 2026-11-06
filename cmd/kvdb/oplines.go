package main

import (
	"fmt"
	"strings"

	"astaroth/src/engine"
	"astaroth/src/token"
)

// The operation line format, shared by batch files and the serve protocol:
//
//	put <key> <value>
//	del <key>
//
// Fields are separated by one space. The key is one field (no spaces; use hex:
// for a key with spaces or other bytes). The value is everything after the
// space that follows the key, so it may contain spaces; use hex: for an empty
// value, line breaks or other bytes.

// parseOp parses the part of a line after the command word.
func parseOp(cmd, rest string) (engine.Operation, error) {
	switch cmd {
	case "put":
		k, v, ok := strings.Cut(rest, " ")
		if !ok || k == "" {
			return engine.Operation{}, fmt.Errorf(`put needs a key and a value: "put <key> <value>" (hex: for an empty value)`)
		}
		key, err := token.Decode(k)
		if err != nil {
			return engine.Operation{}, err
		}
		value, err := token.Decode(v)
		if err != nil {
			return engine.Operation{}, err
		}
		return engine.Operation{Kind: engine.OpPut, Key: key, Value: value}, nil
	case "del":
		if rest == "" || strings.Contains(rest, " ") {
			return engine.Operation{}, fmt.Errorf(`del needs exactly one key: "del <key>"`)
		}
		key, err := token.Decode(rest)
		if err != nil {
			return engine.Operation{}, err
		}
		return engine.Operation{Kind: engine.OpDelete, Key: key}, nil
	}
	return engine.Operation{}, fmt.Errorf("unknown operation %q (want put or del)", cmd)
}

// cleanLine removes a trailing carriage return (files written on Windows) and
// leading blanks, and reports whether anything is left that is not a comment.
// On the first line it also removes a UTF-8 byte order mark, which Notepad
// puts at the start of files and Windows PowerShell at the start of text it
// pipes into a program.
func cleanLine(line string, lineNo int) (string, bool) {
	if lineNo == 1 {
		line = strings.TrimPrefix(line, "\ufeff")
	}
	line = strings.TrimRight(line, "\r")
	line = strings.TrimLeft(line, " \t")
	return line, line != "" && !strings.HasPrefix(line, "#")
}
