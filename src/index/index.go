// Package index is the in-memory key index rebuilt from the log on open.
// It maps each live key to where its latest value sits in the data file;
// values themselves stay on disk.
package index

import "astaroth/src/storage"

// Entry locates a key's latest PUT record in the data file.
type Entry struct {
	RecOff int64  // file offset of the op (its type byte)
	ValLen uint32 // value length, so a read needs no second lookup
}

// Index maps keys to entries. It is not safe for concurrent use; the engine's
// lock protects it.
type Index struct {
	m    map[string]Entry
	live int64 // bytes of the live PUT records (what compaction would keep)
}

// New returns an empty index.
func New() *Index {
	return &Index{m: make(map[string]Entry)}
}

// Len returns the number of live keys.
func (ix *Index) Len() int {
	return len(ix.m)
}

// Reserve makes room for n keys in total so that a large replay does not grow
// the map step by step. It never shrinks the index or loses entries.
func (ix *Index) Reserve(n int) {
	if n <= len(ix.m) {
		return
	}
	m := make(map[string]Entry, n)
	for k, e := range ix.m {
		m[k] = e
	}
	ix.m = m
}

// LiveBytes returns the total size of the live PUT records, the payload a
// compacted file would hold.
func (ix *Index) LiveBytes() int64 {
	return ix.live
}

// Range calls fn for every live key until fn returns false.
func (ix *Index) Range(fn func(key string, e Entry) bool) {
	for k, e := range ix.m {
		if !fn(k, e) {
			return
		}
	}
}

func recordSize(key string, e Entry) int64 {
	return int64(storage.PutHeaderSize + len(key) + int(e.ValLen))
}

// Get returns the entry for key.
func (ix *Index) Get(key []byte) (Entry, bool) {
	e, ok := ix.m[string(key)]
	return e, ok
}

// Apply applies the ops of the frame that starts at frameStart, in order, so
// the last op on a key wins. The engine's write path and recovery replay both
// call it, so a live write and a replay of it cannot disagree.
func (ix *Index) Apply(frameStart int64, ops []storage.Op) {
	for _, op := range ops {
		k := string(op.Key)
		if old, ok := ix.m[k]; ok {
			ix.live -= recordSize(k, old)
		}
		switch op.Type {
		case storage.OpPut:
			e := Entry{RecOff: frameStart + int64(op.Off), ValLen: uint32(len(op.Value))}
			ix.m[k] = e
			ix.live += recordSize(k, e)
		case storage.OpDel:
			delete(ix.m, k)
		}
	}
}
