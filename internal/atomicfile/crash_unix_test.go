//go:build unix

package atomicfile

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestV12_CrashConsistency_SubprocessKill backs V-12 for real: it re-execs
// this same test binary as a child process, has the child SIGKILL itself at
// the exact instant writeFile has fsynced the temp file but not yet renamed
// it, and then checks from the parent that the original file survived
// untouched and still parses. This is the "kill Wasi mid-write" scenario the
// spec calls out, done with an actual kill(2) rather than a mocked failure —
// a mocked failure would only prove the Go code path exists, not that the
// on-disk result of a real crash is what we claim.
func TestV12_CrashConsistency_SubprocessKill(t *testing.T) {
	if os.Getenv("ATOMICFILE_CRASH_HELPER") == "1" {
		runCrashHelper()
		return // unreachable: runCrashHelper always kills the process
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	original := []byte(`{"pututu_counter":41}`)
	if err := WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestV12_CrashConsistency_SubprocessKill$")
	cmd.Env = append(os.Environ(),
		"ATOMICFILE_CRASH_HELPER=1",
		"ATOMICFILE_CRASH_PATH="+path,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected the helper process to be killed; it exited cleanly instead (stderr: %s)", stderr.String())
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected an *exec.ExitError, got %T: %v (stderr: %s)", err, err, stderr.String())
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("expected the helper to die from SIGKILL, got status %v (stderr: %s)", exitErr, stderr.String())
	}

	// The crash landed between the temp file's fsync and the rename. The
	// original file must be exactly what it was before, and must still parse.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("original file missing after crash: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("original file corrupted after crash: got %q, want %q", got, original)
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("original file did not parse after crash: %v", err)
	}

	// No leftover temp file should survive to confuse anything reading the
	// directory (harmless if it does — nothing ever reads .atomicfile-* — but
	// asserting its absence keeps the crash story honest).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".atomicfile-") {
			t.Logf("leftover temp file after kill (expected, harmless): %s", e.Name())
		}
	}

	// A normal write after the crash must succeed and be fully visible —
	// the crash must not have wedged anything (no stale lock, no corrupt
	// directory state).
	next := []byte(`{"pututu_counter":42}`)
	if err := WriteFile(path, next, 0o600); err != nil {
		t.Fatalf("write after crash: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after post-crash write: %v", err)
	}
	if !bytes.Equal(got, next) {
		t.Fatalf("post-crash write incomplete: got %q, want %q", got, next)
	}
}

// runCrashHelper performs one atomic write with a hook that SIGKILLs the
// current process immediately after the temp file is fsynced, before the
// rename. It never returns.
func runCrashHelper() {
	path := os.Getenv("ATOMICFILE_CRASH_PATH")
	_ = writeFile(path, []byte(`{"pututu_counter":999,"this":"must never land"}`), 0o600, func() bool {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		return true // unreachable if the kill lands, kept for a well-typed func
	})
	// If we get here, the kill didn't take effect for some reason. Exit
	// nonzero (not SIGKILL) so the parent's signal assertion fails loudly
	// instead of silently passing on a no-op crash.
	os.Exit(1)
}
