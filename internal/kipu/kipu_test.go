package kipu

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

func TestAppend_WritesOneLinePerCall(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	day := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if err := l.Append(map[string]any{"battery_pct": float64(84)}, day); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if err := l.Append(map[string]any{"battery_pct": float64(83)}, day.Add(time.Hour)); err != nil {
		t.Fatalf("second Append: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026-07-29.jsonl"))
	if err != nil {
		t.Fatalf("read day-file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), data)
	}

	var first line
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}
	if first.Kipu["battery_pct"] != float64(84) {
		t.Errorf("first line battery_pct = %v, want 84", first.Kipu["battery_pct"])
	}
	if !first.At.Equal(day) {
		t.Errorf("first line At = %v, want %v", first.At, day)
	}
}

func TestAppend_PreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	block := map[string]any{
		"battery_pct":       float64(84),
		"rat":               "ltem",
		"a_future_v2_field": "opaque-to-v1-server",
	}
	at := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	if err := l.Append(block, at); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026-07-29.jsonl"))
	if err != nil {
		t.Fatalf("read day-file: %v", err)
	}
	var got line
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kipu["a_future_v2_field"] != "opaque-to-v1-server" {
		t.Errorf("unknown field not preserved: %+v", got.Kipu)
	}
}

func TestAppend_NilBlockStillWritesLine(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	at := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	if err := l.Append(nil, at); err != nil {
		t.Fatalf("Append(nil): %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026-07-29.jsonl"))
	if err != nil {
		t.Fatalf("read day-file: %v", err)
	}
	if strings.Count(string(data), "\n") != 1 {
		t.Fatalf("expected exactly one line, got: %q", data)
	}
}

// TestAppend_OversizedBlockIsDroppedNotFailed backs §4.8: an oversized kipu
// block must not fail the sync it arrived on.
func TestAppend_OversizedBlockIsDroppedNotFailed(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	oversized := map[string]any{"junk": strings.Repeat("x", protocol.MaxKipuBytes*2)}
	at := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	if err := l.Append(oversized, at); err != nil {
		t.Fatalf("Append with an oversized block must not error, got: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026-07-29.jsonl"))
	if err != nil {
		t.Fatalf("read day-file: %v", err)
	}
	var got line
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kipu != nil {
		t.Errorf("oversized block was written instead of dropped: %+v", got.Kipu)
	}
	if !got.At.Equal(at) {
		t.Errorf("At = %v, want %v (line should still be written)", got.At, at)
	}
}

// TestAppend_DayFileNamingIsUTCStable backs the "day-file naming is
// UTC-stable" requirement: a time.Time far from midnight in its own location
// must still land in the file named for its UTC calendar date, not its local
// one.
func TestAppend_DayFileNamingIsUTCStable(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// 00:30 in UTC+14 on July 30th is 10:30 UTC on July 29th.
	farEast := time.FixedZone("UTC+14", 14*60*60)
	local := time.Date(2026, 7, 30, 0, 30, 0, 0, farEast)

	if err := l.Append(map[string]any{"battery_pct": float64(50)}, local); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "2026-07-29.jsonl")); err != nil {
		t.Errorf("expected day-file keyed by UTC date 2026-07-29, not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-07-30.jsonl")); err == nil {
		t.Errorf("day-file keyed by local date 2026-07-30 should not exist")
	}
}

// --- Retention -------------------------------------------------------------

func TestSweep_RemovesOldKeepsWithinWindow(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	writeDayFile(t, dir, "2026-07-01") // 28 days old: outside a 14-day window
	writeDayFile(t, dir, "2026-07-20") // 9 days old: inside the window
	writeDayFile(t, dir, "2026-07-15") // exactly 14 days old: boundary, kept

	if err := l.Sweep(now, 14); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	assertExists(t, dir, "2026-07-20.jsonl", true)
	assertExists(t, dir, "2026-07-15.jsonl", true)
	assertExists(t, dir, "2026-07-01.jsonl", false)
}

func TestSweep_IgnoresNonDayFiles(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := l.Sweep(time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), 14); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	assertExists(t, dir, "README.txt", true)
}

func TestSweep_EmptyDirIsFine(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Sweep(time.Now(), 14); err != nil {
		t.Fatalf("Sweep on empty dir: %v", err)
	}
}

func writeDayFile(t *testing.T, dir, date string) {
	t.Helper()
	path := filepath.Join(dir, date+".jsonl")
	if err := os.WriteFile(path, []byte(`{"at":"2026-01-01T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write day-file %s: %v", date, err)
	}
}

func assertExists(t *testing.T, dir, name string, want bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	got := err == nil
	if got != want {
		t.Errorf("%s exists = %v, want %v (stat err: %v)", name, got, want, err)
	}
}
