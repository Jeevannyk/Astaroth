package index

import (
	"testing"

	"astaroth/src/storage"
)

func put(k, v string, off int) storage.Op {
	return storage.Op{Type: storage.OpPut, Key: []byte(k), Value: []byte(v), Off: off}
}

func del(k string, off int) storage.Op {
	return storage.Op{Type: storage.OpDel, Key: []byte(k), Off: off}
}

func TestNewIndexIsEmpty(t *testing.T) {
	ix := New()
	if ix.Len() != 0 {
		t.Fatalf("Len = %d, want 0", ix.Len())
	}
	if _, ok := ix.Get([]byte("x")); ok {
		t.Fatal("Get found a key in an empty index")
	}
}

func TestApplyPutRecordsAbsoluteOffset(t *testing.T) {
	ix := New()
	ix.Apply(1000, []storage.Op{put("a", "hello", 20)})
	e, ok := ix.Get([]byte("a"))
	if !ok || e.RecOff != 1020 || e.ValLen != 5 {
		t.Fatalf("entry = %+v, %v", e, ok)
	}
}

func TestApplyOverwriteAndDelete(t *testing.T) {
	ix := New()
	ix.Apply(100, []storage.Op{put("a", "1", 20)})
	ix.Apply(200, []storage.Op{put("a", "22", 20)})
	if e, _ := ix.Get([]byte("a")); e.RecOff != 220 || e.ValLen != 2 {
		t.Fatalf("overwrite: entry = %+v", e)
	}
	ix.Apply(300, []storage.Op{del("a", 20)})
	if _, ok := ix.Get([]byte("a")); ok || ix.Len() != 0 {
		t.Fatal("key still present after delete")
	}
	ix.Apply(300, []storage.Op{del("never-existed", 20)}) // no-op
}

// The last op on a key wins inside one frame (spec scenario B5).
func TestApplyOrderWithinFrame(t *testing.T) {
	ix := New()
	ix.Apply(0, []storage.Op{put("x", "v", 20), del("x", 30)})
	if _, ok := ix.Get([]byte("x")); ok {
		t.Fatal("put then delete: key should be absent")
	}
	ix.Apply(0, []storage.Op{del("x", 20), put("x", "v", 30)})
	if e, ok := ix.Get([]byte("x")); !ok || e.RecOff != 30 {
		t.Fatalf("delete then put: entry = %+v, %v", e, ok)
	}
}

func TestReserveKeepsEntriesAndNeverShrinks(t *testing.T) {
	ix := New()
	ix.Apply(100, []storage.Op{put("a", "1", 20), put("b", "22", 40)})
	ix.Reserve(1000)
	if ix.Len() != 2 {
		t.Fatalf("Len = %d after Reserve, want 2", ix.Len())
	}
	if e, ok := ix.Get([]byte("b")); !ok || e.RecOff != 140 || e.ValLen != 2 {
		t.Fatalf("entry b = %+v, %v", e, ok)
	}
	ix.Reserve(1) // smaller than what it holds: no effect
	ix.Reserve(0)
	if ix.Len() != 2 {
		t.Fatalf("Len = %d after small Reserve, want 2", ix.Len())
	}
	ix.Apply(200, []storage.Op{put("c", "3", 20)}) // still usable
	if ix.Len() != 3 {
		t.Fatalf("Len = %d, want 3", ix.Len())
	}
}

// LiveBytes is what compaction would keep: the size of every live PUT record.
func TestLiveBytesTracksOverwritesAndDeletes(t *testing.T) {
	rec := func(k, v string) int64 { return int64(storage.PutHeaderSize + len(k) + len(v)) }
	ix := New()
	ix.Apply(0, []storage.Op{put("a", "12345", 20)})
	if got, want := ix.LiveBytes(), rec("a", "12345"); got != want {
		t.Fatalf("after put: %d, want %d", got, want)
	}
	ix.Apply(100, []storage.Op{put("a", "1", 20), put("bb", "", 40)}) // overwrite + new key
	if got, want := ix.LiveBytes(), rec("a", "1")+rec("bb", ""); got != want {
		t.Fatalf("after overwrite: %d, want %d", got, want)
	}
	ix.Apply(200, []storage.Op{del("a", 20), del("missing", 30)})
	if got, want := ix.LiveBytes(), rec("bb", ""); got != want {
		t.Fatalf("after delete: %d, want %d", got, want)
	}
	ix.Reserve(100)
	if got, want := ix.LiveBytes(), rec("bb", ""); got != want {
		t.Fatalf("after Reserve: %d, want %d", got, want)
	}
	ix.Apply(300, []storage.Op{del("bb", 20)})
	if ix.LiveBytes() != 0 || ix.Len() != 0 {
		t.Fatalf("empty index: LiveBytes %d Len %d", ix.LiveBytes(), ix.Len())
	}
}
