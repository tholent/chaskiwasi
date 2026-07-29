package atomicfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFile_CreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first write: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("content = %q, want %q", got, "first")
	}

	if err := WriteFile(path, []byte("second, longer content"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after second write: %v", err)
	}
	if string(got) != "second, longer content" {
		t.Fatalf("content = %q, want %q", got, "second, longer content")
	}
}

func TestWriteFile_Permissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guardians.toml")

	if err := WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %v, want %v", got, os.FileMode(0o640))
	}
}

func TestWriteFile_NoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ayllu.toml")

	if err := WriteFile(path, []byte("contacts"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".atomicfile-") {
			t.Fatalf("leftover temp file after successful write: %s", e.Name())
		}
	}
}

// TestWriteFile_CrashBeforeRenameLeavesOriginalIntact simulates the crash
// V-12 cares about without needing a real process kill: it drives the
// unexported writeFile with a hook that stops just short of the rename, and
// checks that the original file (and only the original file) is what a
// reader sees afterward. The real process-level version of this same
// scenario lives in crash_unix_test.go.
func TestWriteFile_CrashBeforeRenameLeavesOriginalIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	original := []byte(`{"version":1}`)

	if err := WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	crashed := false
	stopBeforeRename := func() bool { crashed = true; return true }

	err := writeFile(path, []byte(`{"version":2,"never":"lands"}`), 0o600, stopBeforeRename)
	if err == nil {
		t.Fatalf("expected an error from a write that never renamed, got nil")
	}
	if !crashed {
		t.Fatalf("test hook never ran; test is not exercising the intended path")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("original file missing after simulated crash: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("original file changed after simulated crash: got %q, want %q", got, original)
	}
}

func TestWriteFile_ParentMustExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-subdir", "state.json")

	if err := WriteFile(path, []byte("x"), 0o600); err == nil {
		t.Fatalf("expected an error writing into a nonexistent directory")
	}
}
