package config

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validMinimalTOML(name string) string {
	return `
[owner]
name = "` + name + `"
[mail]
imap = "imap.example.com:993"
smtp = "smtp.example.com:465"
address = "maya@example.com"
[device]
token_hash = "` + strings.Repeat("a", 64) + `"
listen = "0.0.0.0:8443"
[guardian]
listen = "0.0.0.0:8444"
`
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWatcher_NewWatcherFailsOnBadInitialConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wasi.toml")
	if err := os.WriteFile(path, []byte("not valid toml [["), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := NewWatcher(path, time.Millisecond, silentLogger()); err == nil {
		t.Fatal("expected NewWatcher to fail on a broken initial file: there is no last-good config to fall back to")
	}
}

func TestWatcher_PicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wasi.toml")
	if err := os.WriteFile(path, []byte(validMinimalTOML("Maya")), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	w, err := NewWatcher(path, 10*time.Millisecond, silentLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if got := w.Current().Owner.Name; got != "Maya" {
		t.Fatalf("initial Owner.Name = %q, want Maya", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Watch(ctx)

	// mtime-based polling needs the second write to actually change mtime;
	// sleeping past the filesystem's mtime granularity avoids a flaky no-op.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte(validMinimalTOML("Rosa")), 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.Current().Owner.Name == "Rosa" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Current().Owner.Name = %q after 2s, want Rosa", w.Current().Owner.Name)
}

// TestWatcher_BadReloadKeepsLastGood is the hot-reload half of §13's
// requirement: a parse failure on reload must never take the server down or
// half-apply a bad file.
func TestWatcher_BadReloadKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wasi.toml")
	if err := os.WriteFile(path, []byte(validMinimalTOML("Maya")), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	w, err := NewWatcher(path, 10*time.Millisecond, silentLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Watch(ctx)

	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte("this is not valid toml [["), 0o600); err != nil {
		t.Fatalf("write broken fixture: %v", err)
	}

	// Give the watcher several poll intervals to (fail to) reload.
	time.Sleep(100 * time.Millisecond)

	if got := w.Current().Owner.Name; got != "Maya" {
		t.Fatalf("Current().Owner.Name = %q after a broken reload, want last-good %q", got, "Maya")
	}
}

func TestWatcher_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wasi.toml")
	if err := os.WriteFile(path, []byte(validMinimalTOML("Maya")), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	w, err := NewWatcher(path, 5*time.Millisecond, silentLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Watch(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}
