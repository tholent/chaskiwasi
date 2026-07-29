package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunOneBackup_CopiesDataExcludingKipuAndTransientFiles(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()

	mustWrite(t, filepath.Join(dataDir, "ayllu.toml"), "version = 1\n")
	mustWrite(t, filepath.Join(dataDir, "ayllu-log.jsonl"), `{"action":"add"}`+"\n")
	mustWrite(t, filepath.Join(dataDir, "state.json"), `{"pututu_counter":3}`)
	mustWrite(t, filepath.Join(dataDir, "kipu", "2026-07-29.jsonl"), `{"battery_pct":90}`)
	// Neither of these is documented state (§3); a backup that copied them
	// would be clutter at best and misleading at worst.
	mustWrite(t, filepath.Join(dataDir, ".atomicfile-abc123.tmp"), "torn write in progress")
	mustWrite(t, filepath.Join(dataDir, dataLockName), "")

	dest, err := runOneBackup(dataDir, backupDir)
	if err != nil {
		t.Fatalf("runOneBackup: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "kipu")); !os.IsNotExist(err) {
		t.Fatal("backup must exclude kipu/ (§3): otherwise its retention window silently extends by backup.retain_days")
	}
	if _, err := os.Stat(filepath.Join(dest, ".atomicfile-abc123.tmp")); !os.IsNotExist(err) {
		t.Fatal("backup must not copy atomicfile's own temp files")
	}
	if _, err := os.Stat(filepath.Join(dest, dataLockName)); !os.IsNotExist(err) {
		t.Fatal("backup must not copy the data-directory lock file")
	}

	for name, want := range map[string]string{
		"ayllu.toml":      "version = 1\n",
		"ayllu-log.jsonl": `{"action":"add"}` + "\n",
		"state.json":      `{"pututu_counter":3}`,
	} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("reading backed-up %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestRunOneBackup_StorageInvariant pins I-1/V-11's storage invariant at the
// point where this command is the thing that could violate it: whatever
// lives only under kipu/ must never reach the backup, regardless of its
// content. The real end-to-end guarantee (no letter content anywhere under
// /data in the first place) is asserted elsewhere in the suite; what this
// command owns is "a backup never contains more than /data does," and that
// the one excluded directory is excluded regardless of what's in it.
func TestRunOneBackup_StorageInvariant(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()
	const forbidden = "letter-shaped content that must never leave kipu/"

	mustWrite(t, filepath.Join(dataDir, "ayllu.toml"), "version = 1\n")
	mustWrite(t, filepath.Join(dataDir, "kipu", "2026-07-29.jsonl"), forbidden)

	dest, err := runOneBackup(dataDir, backupDir)
	if err != nil {
		t.Fatalf("runOneBackup: %v", err)
	}

	err = filepath.WalkDir(dest, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), forbidden) {
			t.Errorf("backup file %s carries content that must have stayed under the excluded kipu/", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking backup: %v", err)
	}
}

func TestUniqueBackupPath_AvoidsCollision(t *testing.T) {
	backupDir := t.TempDir()
	at := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	first := uniqueBackupPath(backupDir, at)
	if err := os.MkdirAll(first, 0o700); err != nil {
		t.Fatal(err)
	}
	second := uniqueBackupPath(backupDir, at)
	if second == first {
		t.Fatal("uniqueBackupPath returned the same path for an already-occupied timestamp")
	}
}

func TestSweepOldBackups_DeletesOnlyExpiredBackupsItMinted(t *testing.T) {
	backupDir := t.TempDir()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	old := uniqueBackupPath(backupDir, now.Add(-10*24*time.Hour))
	recent := uniqueBackupPath(backupDir, now.Add(-1*24*time.Hour))
	for _, dir := range []string{old, recent} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Not this command's file to delete: it doesn't match the naming
	// scheme uniqueBackupPath produces.
	stray := filepath.Join(backupDir, "not-a-backup")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := sweepOldBackups(backupDir, 7, now)
	if err != nil {
		t.Fatalf("sweepOldBackups: %v", err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Fatalf("removed = %v, want [%s]", removed, old)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old backup was not removed")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent backup was removed")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("a directory this command did not create was removed")
	}
}

func TestRunBackup_EndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()
	mustWrite(t, filepath.Join(dataDir, "ayllu.toml"), "version = 1\n")

	err := runBackup([]string{
		"-data", dataDir,
		"-config", filepath.Join(dataDir, "does-not-exist.toml"), // falls back to defaults
		"-dir", backupDir,
	})
	if err != nil {
		t.Fatalf("runBackup: %v", err)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one backup directory, got %d", len(entries))
	}
	if _, err := os.Stat(filepath.Join(backupDir, entries[0].Name(), "ayllu.toml")); err != nil {
		t.Errorf("ayllu.toml missing from the backup: %v", err)
	}
}

func TestRunBackup_UsesConfiguredBackupSettings(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()
	mustWrite(t, filepath.Join(dataDir, "ayllu.toml"), "version = 1\n")

	configPath := filepath.Join(t.TempDir(), "wasi.toml")
	mustWrite(t, configPath, `
[owner]
name = "Maya"
[mail]
imap = "mail.test:993"
smtp = "mail.test:465"
address = "kid@chaski.test"
[device]
token_hash = "7053fe692ce151a1a4e066d93850420b420ce95d823a0c7e8609fddf5272438d"
listen = "127.0.0.1:0"
[guardian]
listen = "127.0.0.1:0"
[backup]
dir = "`+backupDir+`"
retain_days = 3
`)

	err := runBackup([]string{"-data", dataDir, "-config", configPath})
	if err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backup did not land in the configured backup.dir: entries=%v err=%v", entries, err)
	}
}
