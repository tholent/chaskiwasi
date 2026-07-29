package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tholent/chaskiwasi/internal/config"
)

// excludedTopLevelDirs are never copied into a backup, checked only at the
// top level of /data (a contact or guardian could plausibly be named
// "kipu" in some future per-file layout; nothing below the top level should
// ever match this by accident).
//
// kipu/ is the one wasi-server-plan §3 names explicitly, and it is not
// configurable — no flag, no wasi.toml key — on purpose: kipu's day-files
// are retained for exactly kipu.retention_days precisely so that whole-file
// deletion is the erasure story (design-spec §3.7). A backup that copied
// them would silently extend that retention window by backup.retain_days on
// top of it, putting deleted day-files back within reach of exactly the
// adversary §3.7 contemplates — an accumulated location history in a
// contested household. If a future change needs kipu in a backup, that is a
// new, explicit feature to design, not a flag to bolt on here.
var excludedTopLevelDirs = map[string]bool{
	"kipu": true,
}

// excludedNames skips two kinds of file that are never part of the
// documented state in §3 and would only be clutter — or, for the lock file,
// actively misleading — in a backup: atomicfile's own temp files, which
// briefly exist mid-write and are never a complete generation of anything a
// backup should preserve, and the data-directory lock file (lock.go),
// which holds nothing and whose presence or absence in a restored directory
// changes nothing either way.
func excludedNames(name string) bool {
	return strings.HasPrefix(name, ".atomicfile-") || name == dataLockName
}

// backupTimeFormat names each backup by the UTC instant runOneBackup
// started, sortable as a plain string and parseable back out by
// sweepOldBackups without keeping any separate index.
const backupTimeFormat = "20060102T150405Z"

// runBackup implements `wasi backup` (§3, §14): copy /data — excluding
// kipu/ — into a fresh timestamped directory under the backup volume, then
// delete whole backup directories older than the retention window.
//
// # Consistency with a running server
//
// Unlike `wasi contacts`, backup does not require — or even check for — a
// stopped server: periodic backups of a live deployment are the normal
// case, and refusing to run against a live /data would make routine backups
// impossible without an outage. What this command actually copies is a
// plain recursive directory read against files a running `serve` may be
// concurrently rewriting, and what that does and does not guarantee is
// worth being explicit about:
//
//   - Per file, atomicfile's write discipline (temp file, fsync, rename,
//     fsync directory — internal/atomicfile) means this command's read of
//     any single file — ayllu.toml, state.json, guardians.toml,
//     ayllu-log.jsonl — is always a complete, non-torn generation of that
//     file. There is no on-disk state in which a reader observes a
//     half-written file, so there is none in which a backup does either.
//   - Across files, this is a "fuzzy" snapshot, not one consistent instant:
//     if a mutation lands in the middle of the backup's directory walk, one
//     file it copies can reflect a moment slightly before or after another.
//     For state.json this is explicitly safe by design: §10.3 exists
//     precisely so that a restored state.json — arbitrarily stale relative
//     to the mailbox — heals over the next sync rather than silently
//     breaking the doorbell. For ayllu.toml and ayllu-log.jsonl, the
//     exposure is narrower still: ayllu.Store.Mutate holds its own lock
//     across writing both, so the only skew a backup can catch them in is
//     the width of one Mutate call (a couple of filesystem operations) — not
//     the width of the whole backup. A backup taken while nobody is actively
//     editing the contact list (the overwhelmingly common case: this list
//     changes a handful of times, not continuously) sees the pair
//     consistently. A guaranteed-consistent snapshot of the pair requires
//     stopping the server first, or a filesystem-level snapshot outside this
//     command's scope — this command does not claim either.
//
// # Restore
//
// Restore is `cp` back (the timestamped directory's contents, into /data)
// plus one sync — deliberately no more machinery than that, per §3. The
// pututu counter in state.json self-heals over the sync wire even from an
// arbitrarily old backup (§10.3); nothing else in a restored /data needs
// operator repair for the reasons above.
func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dataDir := fs.String("data", defaultDataDir, "path to the server-owned data directory to back up")
	configPath := fs.String("config", defaultConfigPath,
		"path to wasi.toml, read for backup.dir and backup.retain_days (§13)")
	dirFlag := fs.String("dir", "", "backup destination (overrides backup.dir from wasi.toml)")
	retainFlag := fs.Int("retain-days", 0, "retention in days (overrides backup.retain_days from wasi.toml)")
	skipRetention := fs.Bool("skip-retention", false, "take the backup but do not delete any old ones")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: wasi backup [flags]\n\n"+
			"Copies -data (excluding kipu/) into a new timestamped directory\n"+
			"under the backup destination, then deletes whole backup directories\n"+
			"older than the retention window. Safe to run against a live server\n"+
			"(§3) — see the comment on runBackup in backup.go for exactly what\n"+
			"consistency guarantee that does and does not carry.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("backup: unexpected arguments")
	}

	backupDir, retainDays, err := backupSettings(*configPath, *dirFlag, *retainFlag)
	if err != nil {
		return err
	}

	dest, err := runOneBackup(*dataDir, backupDir)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	fmt.Printf("Backed up %s to %s\n", *dataDir, dest)

	if *skipRetention {
		return nil
	}
	removed, err := sweepOldBackups(backupDir, retainDays, time.Now())
	if err != nil {
		return fmt.Errorf("backup: retention sweep: %w", err)
	}
	for _, r := range removed {
		fmt.Printf("Removed old backup %s (older than %d day(s))\n", r, retainDays)
	}
	return nil
}

// backupSettings resolves the backup destination and retention window from,
// in order: the CLI override flags, then wasi.toml's [backup] table, then
// §13's documented defaults. A missing config file falls back to the
// defaults rather than failing outright: `wasi backup` should still work
// against a data directory seeded before wasi.toml exists (e.g. restoring
// into a fresh volume before first boot), the same reasoning `wasi
// contacts` uses for its own max_contacts lookup.
func backupSettings(configPath, dirOverride string, retainOverride int) (dir string, retainDays int, err error) {
	dir, retainDays = config.DefaultBackupDir, config.DefaultBackupRetain
	if _, statErr := os.Stat(configPath); statErr == nil {
		cfg, loadErr := config.Load(configPath)
		if loadErr != nil {
			return "", 0, fmt.Errorf("backup: %w", loadErr)
		}
		dir, retainDays = cfg.Backup.Dir, cfg.Backup.RetainDays
	}
	if dirOverride != "" {
		dir = dirOverride
	}
	if retainOverride > 0 {
		retainDays = retainOverride
	}
	if retainDays <= 0 {
		return "", 0, fmt.Errorf("backup: retention must be positive, got %d", retainDays)
	}
	return dir, retainDays, nil
}

// runOneBackup copies dataDir into a new timestamped subdirectory of
// backupDir and returns its path. See runBackup's doc comment for the
// consistency guarantee this does and does not make against a live server.
func runOneBackup(dataDir, backupDir string) (string, error) {
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("creating backup directory %s: %w", backupDir, err)
	}

	dest := uniqueBackupPath(backupDir, time.Now().UTC())
	if err := copyTree(dataDir, dest, true); err != nil {
		// A half-written backup is worse than none: left in place, a later
		// restore could use it without anyone noticing files are missing.
		os.RemoveAll(dest)
		return "", err
	}

	// The new directory entry for dest isn't durable until backupDir itself
	// is synced — the same reasoning internal/atomicfile applies to a
	// single file's rename, applied here to the directory this command just
	// created.
	if dir, err := os.Open(backupDir); err == nil {
		dir.Sync()
		dir.Close()
	}

	return dest, nil
}

// uniqueBackupPath names a new backup by UTC timestamp, appending a "-N"
// suffix on collision so two backups requested within the same second (a
// test calling this in a loop, or an operator running the command twice by
// hand) never overwrite one another.
func uniqueBackupPath(backupDir string, at time.Time) string {
	base := at.Format(backupTimeFormat)
	path := filepath.Join(backupDir, base)
	for i := 1; ; i++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path
		}
		path = filepath.Join(backupDir, fmt.Sprintf("%s-%d", base, i))
	}
}

// copyTree copies every regular file and directory under src into dst,
// skipping excludedTopLevelDirs at the top level and excludedNames
// everywhere. dst is created (and every directory below it) as it goes.
//
// This walks by exclusion, not by an allowlist of the exact filenames §3
// documents today: a future state file added to /data is backed up
// automatically, without this command needing a matching change. kipu/
// alone is excluded by name because it is the one exception the spec
// carves out explicitly (§3) — everything else under /data is, by
// definition, backed up.
func copyTree(src, dst string, topLevel bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if excludedNames(name) {
			continue
		}
		if topLevel && entry.IsDir() && excludedTopLevelDirs[name] {
			continue
		}

		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)

		if entry.IsDir() {
			if err := copyTree(srcPath, dstPath, false); err != nil {
				return err
			}
			continue
		}
		if !entry.Type().IsRegular() {
			// /data holds only plain files and directories in every
			// documented shape (§3). Anything else — a symlink, a socket —
			// is not part of the state contract; skipping it rather than
			// following it also means a backup can never be tricked into
			// copying something from outside /data through one.
			continue
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies one regular file, preserving its permission bits and
// fsyncing the destination before returning — the same durability
// discipline internal/atomicfile applies to every server-owned write (§3),
// applied here to the backup's own output.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	// O_EXCL: dst sits inside a freshly minted timestamped directory, so an
	// existing file at this path would mean a name collision this function
	// has no business resolving silently.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	return out.Close()
}

// sweepOldBackups deletes whole backup directories older than retainDays,
// judged by the timestamp encoded in their own name rather than filesystem
// mtime — mtime can be nudged by something unrelated (a restore test
// copying the directory, a backup tool touching it), but the name this
// command minted can't drift. Entries whose name doesn't parse as one this
// command would have produced are left alone: a file an operator dropped
// into backup.dir by hand is not this command's to delete.
func sweepOldBackups(backupDir string, retainDays int, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", backupDir, err)
	}

	cutoff := now.Add(-time.Duration(retainDays) * 24 * time.Hour)
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		at, ok := parseBackupName(entry.Name())
		if !ok || !at.Before(cutoff) {
			continue
		}
		path := filepath.Join(backupDir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("removing %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	sort.Strings(removed)
	return removed, nil
}

// parseBackupName recovers the timestamp uniqueBackupPath encoded into name,
// tolerating the "-N" collision suffix it sometimes appends.
func parseBackupName(name string) (time.Time, bool) {
	if t, err := time.Parse(backupTimeFormat, name); err == nil {
		return t, true
	}
	if i := strings.LastIndexByte(name, '-'); i > 0 {
		if t, err := time.Parse(backupTimeFormat, name[:i]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
