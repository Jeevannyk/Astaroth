package storage

import "errors"

// ErrSimulatedDiskFull is what the disk-full failpoints inject in place of a
// real "no space left on device" error, so tests can exercise the handling of
// one without filling a disk. IsDiskFull recognizes it.
var ErrSimulatedDiskFull = errors.New("storage: simulated disk full")

// FrameSize returns the number of bytes EncodeFrame would produce for ops,
// which must be valid.
func FrameSize(ops []Op) int64 {
	n := int64(FrameOverhead)
	for _, op := range ops {
		n += int64(opSize(op))
	}
	return n
}

// IsDiskFull reports whether err means the disk (or a quota) is full.
func IsDiskFull(err error) bool {
	return errors.Is(err, ErrSimulatedDiskFull) || isOSDiskFull(err)
}
