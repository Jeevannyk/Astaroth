package tests

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The torture tool itself, run end to end in every mode for a few iterations
// against the real kvdb binary. The full runs (500 iterations and more) are
// done from the command line; see README.md, "Torture tool".
func TestTortureToolAllModes(t *testing.T) {
	iterations := "4"
	if testing.Short() {
		iterations = "2"
	}
	name := "torture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(filepath.Dir(kvdbBinary(t)), name)
	for _, mode := range []string{"normal", "large-values", "large-batches", "recovery-crash", "compaction", "disk-full"} {
		t.Run(mode, func(t *testing.T) {
			out, err := exec.Command(bin, "--iterations", iterations, "--seed", "4242", "--mode", mode, "--dir", t.TempDir()).CombinedOutput()
			text := string(out)
			if err != nil || !strings.Contains(text, "PASS: "+iterations+" of "+iterations+" iterations") {
				t.Fatalf("torture --mode %s: %v\n%s", mode, err, text)
			}
			if !strings.Contains(text, "landed while operations were in flight") {
				t.Fatalf("summary missing:\n%s", text)
			}
		})
	}
}

func TestTortureToolRejectsBadArguments(t *testing.T) {
	name := "torture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(filepath.Dir(kvdbBinary(t)), name)
	for _, args := range [][]string{{"--mode", "nope"}, {"--writers", "1"}, {"--iterations", "0"}, {"stray"}} {
		cmd := exec.Command(bin, args...)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "usage") {
			t.Fatalf("torture %v: %v\n%s", args, err, out)
		}
	}
}
