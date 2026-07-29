// Package atomicfile is the write discipline that stands in for a database
// (wasi-server-plan §3, A.9). Every server-owned file — state.json,
// ayllu.toml, guardians.toml, the kipu day-files — is rewritten in full on
// every change, so what a database would otherwise have given us for free
// (atomicity, crash consistency, no torn writes) has to be built explicitly
// here instead:
//
//  1. write a temp file in the SAME directory as the target (so the eventual
//     rename is same-filesystem and therefore atomic),
//  2. fsync the temp file,
//  3. rename it over the target,
//  4. fsync the directory (rename is atomic, but on most filesystems the
//     directory entry itself isn't durable until the directory is synced —
//     skipping this step can lose the rename across a power cut even though
//     the file content was fully synced).
//
// A crash at any point before the rename leaves the original file untouched;
// a crash after the rename leaves the new file complete. There is no state in
// which a reader can observe a partially written file (test V-12).
//
// WriteFile is guarded by a single package-level mutex, per §3's "behind a
// single writer mutex": at the sync volumes this server sees (~10/day) a
// single writer for the whole of /data costs nothing and removes an entire
// class of interleaved-write bugs between independent callers (state, ayllu,
// guardians, kipu).
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// mu serializes every atomic write across the whole process, satisfying the
// "single writer for all of /data" requirement without callers having to
// share an explicit handle (§3).
var mu sync.Mutex

// WriteFile atomically replaces path with data: temp file, fsync, rename,
// fsync directory, per the package doc. perm applies to the final file.
//
// path's parent directory must already exist and be on the same filesystem
// as path itself (true for every caller in this codebase: they all write
// into /data or a subdirectory of it).
func WriteFile(path string, data []byte, perm os.FileMode) error {
	mu.Lock()
	defer mu.Unlock()
	return writeFile(path, data, perm, nil)
}

// writeFile is the real implementation. testHook, when non-nil, runs after
// the temp file has been written and fsynced but before the rename — the
// single most dangerous instant to crash at. If it returns true, writeFile
// stops right there without renaming, simulating a process death at that
// instant; the subprocess test in crash_unix_test.go does the same thing for
// real, with an actual SIGKILL. testHook is always nil in production;
// WriteFile never sets it.
func writeFile(path string, data []byte, perm os.FileMode, testHook func() (crash bool)) (err error) {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".atomicfile-*.tmp")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// If we return before a successful rename, the temp file is ours to clean
	// up; once renamed, tmpPath no longer exists under that name so this is a
	// harmless no-op.
	defer os.Remove(tmpPath)

	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: fsync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close temp file: %w", err)
	}
	if err = os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("atomicfile: chmod temp file: %w", err)
	}

	if testHook != nil && testHook() {
		return fmt.Errorf("atomicfile: simulated crash before rename")
	}

	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomicfile: rename into place: %w", err)
	}

	// The rename is atomic, but the directory entry it changed isn't durable
	// until the directory itself is synced — without this, a crash right
	// after a successful rename can still lose it on some filesystems.
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("atomicfile: open dir for fsync: %w", err)
	}
	defer dirFile.Close()
	if err = dirFile.Sync(); err != nil {
		return fmt.Errorf("atomicfile: fsync dir: %w", err)
	}

	return nil
}
