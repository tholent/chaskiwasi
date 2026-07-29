package config

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// DefaultWatchInterval is how often Watcher polls wasi.toml for changes when
// no interval is given. mtime+size polling is deliberately preferred over an
// fsnotify dependency (§13): at this poll rate a config edit is picked up
// well inside the time it takes a human to notice and refresh a browser tab,
// and it costs one stat(2) call.
const DefaultWatchInterval = 5 * time.Second

// Watcher hot-reloads wasi.toml (§13) and exposes the current config to
// concurrent readers via Current. A parse failure on reload keeps the last
// good config and logs an error — a bad hand-edit must never take the server
// down or half-apply, since two writers to one file (§9.1) means the human
// editing it may well save a syntactically broken intermediate state.
type Watcher struct {
	path     string
	interval time.Duration
	logger   *slog.Logger

	current atomic.Pointer[Config]

	// modTime and size are read and written only from the Watch goroutine, so
	// they need no lock of their own; current is the only field readers touch
	// concurrently, and atomic.Pointer covers that without one.
	modTime time.Time
	size    int64
}

// NewWatcher loads path once — a failure here is a startup failure, not a
// reload failure, because there is no "last good" config yet to fall back to
// — and returns a Watcher ready to serve Current() immediately. Call Watch in
// a goroutine to start polling for changes.
func NewWatcher(path string, interval time.Duration, logger *slog.Logger) (*Watcher, error) {
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	if logger == nil {
		logger = slog.Default()
	}

	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		path:     path,
		interval: interval,
		logger:   logger,
		modTime:  fi.ModTime(),
		size:     fi.Size(),
	}
	w.current.Store(cfg)
	return w, nil
}

// Current returns the most recently loaded good config. Safe for concurrent
// use; the returned *Config must be treated as read-only by callers, since it
// may be shared with a reload that replaces Watcher's pointer a moment later.
func (w *Watcher) Current() *Config {
	return w.current.Load()
}

// Watch polls path for changes every interval until ctx is done. Intended to
// be run in its own goroutine by the caller (mirrors internal/kipu's
// StartRetentionSweeper), so Watcher itself owns no goroutine lifecycle.
func (w *Watcher) Watch(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reloadIfChanged()
		}
	}
}

// reloadIfChanged is called only from Watch's goroutine, so modTime/size need
// no synchronization here.
func (w *Watcher) reloadIfChanged() {
	fi, err := os.Stat(w.path)
	if err != nil {
		w.logger.Error("config: stat failed during reload poll, keeping last good config",
			"path", w.path, "error", err)
		return
	}
	if fi.ModTime().Equal(w.modTime) && fi.Size() == w.size {
		return
	}

	cfg, err := Load(w.path)
	if err != nil {
		w.logger.Error("config: reload failed, keeping last good config",
			"path", w.path, "error", err)
		return
	}

	w.modTime = fi.ModTime()
	w.size = fi.Size()
	w.current.Store(cfg)
	w.logger.Info("config: reloaded", "path", w.path)
}
