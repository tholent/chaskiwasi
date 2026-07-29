package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/config"
	"github.com/tholent/chaskiwasi/internal/guardians"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/web"
)

// stubMailbox answers only what the ops endpoints ask of a mailbox (§14).
type stubMailbox struct{ err error }

func (s stubMailbox) UIDValidity(ctx context.Context) (uint32, error) { return 7, s.err }
func (s stubMailbox) FetchAbove(ctx context.Context, uid uint32, max int) ([]mailbox.Raw, error) {
	return nil, s.err
}
func (s stubMailbox) Recent(ctx context.Context, n int) ([]mailbox.Raw, error) { return nil, s.err }
func (s stubMailbox) List(ctx context.Context, folder string) ([]mailbox.Raw, error) {
	return nil, s.err
}
func (s stubMailbox) Move(ctx context.Context, folder string, uid uint32, dest string) error {
	return s.err
}
func (s stubMailbox) Append(ctx context.Context, folder string, msg []byte, at time.Time) error {
	return s.err
}
func (s stubMailbox) Idle(ctx context.Context, notify chan<- struct{}) error { return s.err }
func (s stubMailbox) Close() error                                           { return nil }

const testWasiTOML = `
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
`

func testWatcher(t *testing.T) *config.Watcher {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wasi.toml")
	if err := os.WriteFile(path, []byte(testWasiTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	w, err := config.NewWatcher(path, time.Hour, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	return w
}

// testUI builds the real guardian UI over throwaway state, so the mux tests
// exercise the actual mount rather than a stand-in that happens to 404.
func testUI(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()

	guardianStore, err := guardians.Open(dir)
	if err != nil {
		t.Fatalf("guardians.Open: %v", err)
	}
	aylluStore, err := ayllu.Open(dir, config.DefaultMaxContacts)
	if err != nil {
		t.Fatalf("ayllu.Open: %v", err)
	}
	ui, err := web.New(web.Config{
		Guardians: guardianStore,
		Ayllu:     aylluStore,
		Watcher:   testWatcher(t),
		DataDir:   dir,
		CookieKey: []byte("a key that only this test uses"),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return ui.Handler()
}

func TestGuardianMux_HealthzAndReadyz(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		mailboxErr error
		wantStatus int
	}{
		{"healthz is process-up only", "/healthz", nil, http.StatusOK},
		{"healthz ignores the mailbox", "/healthz", errors.New("imap down"), http.StatusOK},
		{"readyz with a reachable mailbox", "/readyz", nil, http.StatusOK},
		{"readyz with an unreachable mailbox", "/readyz", mailbox.ErrUnreachable, http.StatusServiceUnavailable},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := guardianMux(testWatcher(t), stubMailbox{err: tc.mailboxErr}, testUI(t), logger)

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.wantStatus {
				t.Fatalf("%s = %d, want %d", tc.path, w.Code, tc.wantStatus)
			}
		})
	}
}

func TestGuardianMux_ServesNoDeviceEndpoint(t *testing.T) {
	// The two listeners share nothing but the process (§12.1): /sync exists on
	// the device listener alone, behind the private-CA leaf. The guardian UI
	// is mounted at "/" and must not answer for it either.
	mux := guardianMux(testWatcher(t), stubMailbox{}, testUI(t), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sync", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("/sync on the guardian listener = %d, want 404", w.Code)
	}
}

func TestGuardianMux_MountsTheUIWithoutShadowingOps(t *testing.T) {
	// The UI is registered at "/", which must not capture the two ops
	// endpoints (§14) and must still authenticate everything of its own.
	mux := guardianMux(testWatcher(t), stubMailbox{}, testUI(t), slog.New(slog.NewTextHandler(os.Stderr, nil)))

	tests := []struct {
		path       string
		wantStatus int
		wantHeader string
	}{
		{"/healthz", http.StatusOK, ""},
		{"/", http.StatusSeeOther, "/login?m=signed-out"},
		{"/contacts", http.StatusSeeOther, "/login?m=signed-out"},
		{"/login", http.StatusOK, ""},
	}
	for _, tc := range tests {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != tc.wantStatus {
			t.Errorf("GET %s = %d, want %d", tc.path, w.Code, tc.wantStatus)
		}
		if tc.wantHeader != "" && w.Header().Get("Location") != tc.wantHeader {
			t.Errorf("GET %s redirected to %q, want %q", tc.path, w.Header().Get("Location"), tc.wantHeader)
		}
	}
}

func TestDeviceMux_ServesOnlySync(t *testing.T) {
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux := deviceMux(sentinel)

	tests := []struct {
		path       string
		wantStatus int
	}{
		{"/sync", http.StatusTeapot},
		{"/healthz", http.StatusNotFound},
		{"/", http.StatusNotFound},
	}
	for _, tc := range tests {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, nil))
		if w.Code != tc.wantStatus {
			t.Fatalf("%s = %d, want %d", tc.path, w.Code, tc.wantStatus)
		}
	}
}

func TestServeTLS_RequiresCertAndKey(t *testing.T) {
	err := serveTLS(&http.Server{}, "", "", "device")
	if err == nil {
		t.Fatal("serveTLS accepted an empty certificate path")
	}
}
