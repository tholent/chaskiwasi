// Package kipu implements /data/kipu/YYYY-MM-DD.jsonl (wasi-server-plan §3,
// §4.8, A.6): one line per sync holding that sync's kipu block — device
// health telemetry — plus a server timestamp.
//
// Glossary (design-spec §0.1): kipu = the knotted-cord record = device health
// telemetry. The word is greppable on purpose and never appears in
// guardian-facing UI text (§9.1, test V-14).
//
// Kipu is designed to be forgotten, unlike everything else this server keeps:
// retention deletes whole day-files, never rewrites part of one, so erasure
// means exactly what it says — no freelist pages, no WAL, nothing to VACUUM
// (§3.7, A.6). Backups exclude this directory entirely so that retention
// isn't silently extended by the backup window (§3).
//
// Each day-file is itself still written through internal/atomicfile: a
// day-file is a server-owned file like any other, and at the sync volumes
// this server sees (~10/day) rewriting the whole (tiny) file on every append
// is cheap insurance against a torn line.
package kipu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/tholent/chaskiwasi/internal/atomicfile"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// dayFileLayout formats and parses the UTC calendar date in a day-file's
// name. Using UTC unconditionally, regardless of the server's local
// timezone, is what makes day-file naming stable (a sync just before
// midnight local time must not land in a different file than the sweep
// later expects).
const dayFileLayout = "2006-01-02"

// dayFileRE matches exactly the day-file names this package writes, so Sweep
// ignores anything else that might land in the directory.
var dayFileRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

// Log appends to and sweeps /data/kipu.
type Log struct {
	dir string
}

// line is one row of a day-file. Kipu is nil when a sync carried no kipu
// block, or when the block was dropped for exceeding protocol.MaxKipuBytes.
type line struct {
	At   time.Time      `json:"at"`
	Kipu map[string]any `json:"kipu,omitempty"`
}

// Open ensures dir exists and returns a Log over it. dir is normally
// /data/kipu.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("kipu: create %s: %w", dir, err)
	}
	return &Log{dir: dir}, nil
}

// Append records one sync's kipu block, plus the server timestamp at, into
// today's (UTC) day-file. block may be nil — a sync with no kipu field still
// gets a line, which is what makes "how long has it been since we heard from
// the device" a query over this log rather than something state.json has to
// track separately.
//
// Unknown fields inside block are written back exactly as received (§4.8):
// this package never interprets kipu content, so a firmware field it doesn't
// know about round-trips untouched, ready for a future server version. A
// block whose encoded size exceeds protocol.MaxKipuBytes is dropped — not
// truncated, since there is no truncation rule for an arbitrary JSON object
// that both fits a byte budget and stays valid JSON — and the sync itself
// must not fail because of it: a malformed or oversized kipu block from a
// buggy device is a telemetry problem, never a reason to lose a letter.
func (l *Log) Append(block map[string]any, at time.Time) error {
	if block != nil {
		raw, err := json.Marshal(block)
		if err != nil {
			return fmt.Errorf("kipu: marshal block: %w", err)
		}
		if len(raw) > protocol.MaxKipuBytes {
			slog.Warn("kipu: block exceeds size cap, dropping",
				"bytes", len(raw), "cap", protocol.MaxKipuBytes)
			block = nil
		}
	}

	encoded, err := json.Marshal(line{At: at.UTC(), Kipu: block})
	if err != nil {
		return fmt.Errorf("kipu: marshal line: %w", err)
	}

	path := l.pathFor(at)
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("kipu: read %s: %w", path, err)
	}

	updated := make([]byte, 0, len(existing)+len(encoded)+1)
	updated = append(updated, existing...)
	updated = append(updated, encoded...)
	updated = append(updated, '\n')

	if err := atomicfile.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("kipu: write %s: %w", path, err)
	}
	return nil
}

// pathFor returns the day-file path for at, always keyed by at's UTC
// calendar date regardless of at's own location.
func (l *Log) pathFor(at time.Time) string {
	return filepath.Join(l.dir, at.UTC().Format(dayFileLayout)+".jsonl")
}

// Sweep deletes whole day-files whose date is older than retentionDays
// before now (evaluated in UTC), at startup and once daily (§3). "Older
// than" is strict: a file dated exactly retentionDays ago is still inside
// the window and survives.
func (l *Log) Sweep(now time.Time, retentionDays int) error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("kipu: read %s: %w", l.dir, err)
	}

	cutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -retentionDays)

	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dayFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		day, err := time.Parse(dayFileLayout, m[1])
		if err != nil {
			continue // not a date we recognise; leave it alone
		}
		if day.Before(cutoff) {
			path := filepath.Join(l.dir, e.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("kipu: remove %s: %w", path, err))
			}
		}
	}
	return errors.Join(errs...)
}

// RunRetentionSweeper runs Sweep once immediately — "at startup" — and then
// once every 24 hours until ctx is cancelled — "daily" (§3). It blocks; the
// caller runs it in its own goroutine, mirroring internal/config.Watcher.Watch.
func (l *Log) RunRetentionSweeper(ctx context.Context, retentionDays int, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	if err := l.Sweep(time.Now(), retentionDays); err != nil {
		logger.Error("kipu: startup retention sweep failed", "error", err)
	}

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.Sweep(time.Now(), retentionDays); err != nil {
				logger.Error("kipu: retention sweep failed", "error", err)
			}
		}
	}
}
