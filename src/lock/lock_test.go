package lock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireReleaseReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.lock")

	l, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	l2.Release()
}

func TestSecondAcquireFailsWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.lock")

	l, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	start := time.Now()
	_, err = Acquire(path, 50*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("Acquire gave up before its timeout")
	}
}

func TestAcquireWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.lock")

	l, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		l.Release()
	}()

	l2, err := Acquire(path, 2*time.Second)
	if err != nil {
		t.Fatalf("Acquire did not pick up the released lock: %v", err)
	}
	l2.Release()
}

func TestAcquireMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "db.lock")
	_, err := Acquire(path, 50*time.Millisecond)
	if err == nil || errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want an open error that is not ErrLocked", err)
	}
}
