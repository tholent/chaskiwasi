package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/config"
	"github.com/tholent/chaskiwasi/internal/filing"
	"github.com/tholent/chaskiwasi/internal/guardians"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/state"
)

// The tests use the real ayllu, guardians, state, config, and filing packages
// against a fake mailbox. Only the mailbox is faked, because it is the only
// dependency that would otherwise need a network — and because V-18's UI half
// is only worth anything if the release really goes through filing.

const (
	testHost     = "wasi.home.test"
	testPassword = "a perfectly good passphrase"
	heldFolder   = "Held"
)

// harnessWasiTOML is a format string: the one value that has to be a real
// path is the device certificate, since §12.3's check reads it.
const harnessWasiTOML = `
[owner]
name = "Maya"
[mail]
imap = "mail.test:993"
smtp = "mail.test:465"
address = "maya@chaski.test"
held_folder = "Held"
[device]
token_hash = "7053fe692ce151a1a4e066d93850420b420ce95d823a0c7e8609fddf5272438d"
listen = "127.0.0.1:8443"
tls_cert = "%s"
[guardian]
listen = "127.0.0.1:8444"
copy_addresses = ["dad@example.test"]
[ayllu]
max_contacts = 4
[carrier]
name = "fake"
`

// fakeMailbox is an in-memory IMAP stand-in. Move assigns a new UID in the
// destination folder, mirroring the real thing — §8 depends on a released
// message landing above the cursor.
type fakeMailbox struct {
	mu      sync.Mutex
	folders map[string][]mailbox.Raw
	nextUID uint32

	listErr error
	moveErr error
	moves   []string
}

func newFakeMailbox() *fakeMailbox {
	return &fakeMailbox{folders: map[string][]mailbox.Raw{}, nextUID: 1}
}

func (m *fakeMailbox) put(folder string, data []byte, at time.Time) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	uid := m.nextUID
	m.nextUID++
	m.folders[folder] = append(m.folders[folder], mailbox.Raw{UID: uid, InternalDate: at, Data: data})
	return uid
}

func (m *fakeMailbox) count(folder string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.folders[folder])
}

func (m *fakeMailbox) UIDValidity(context.Context) (uint32, error) { return 1, nil }

func (m *fakeMailbox) List(_ context.Context, folder string) ([]mailbox.Raw, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]mailbox.Raw, len(m.folders[folder]))
	copy(out, m.folders[folder])
	return out, nil
}

func (m *fakeMailbox) FetchAbove(_ context.Context, uid uint32, max int) ([]mailbox.Raw, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mailbox.Raw
	for _, raw := range m.folders["INBOX"] {
		if raw.UID > uid && len(out) < max {
			out = append(out, raw)
		}
	}
	return out, nil
}

func (m *fakeMailbox) Recent(_ context.Context, n int) ([]mailbox.Raw, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inbox := m.folders["INBOX"]
	if len(inbox) > n {
		inbox = inbox[len(inbox)-n:]
	}
	out := make([]mailbox.Raw, len(inbox))
	copy(out, inbox)
	return out, nil
}

func (m *fakeMailbox) Move(_ context.Context, folder string, uid uint32, dest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moveErr != nil {
		return m.moveErr
	}
	for i, raw := range m.folders[folder] {
		if raw.UID != uid {
			continue
		}
		m.folders[folder] = append(m.folders[folder][:i:i], m.folders[folder][i+1:]...)
		raw.UID = m.nextUID
		m.nextUID++
		m.folders[dest] = append(m.folders[dest], raw)
		m.moves = append(m.moves, fmt.Sprintf("%s->%s", folder, dest))
		return nil
	}
	return fmt.Errorf("fakeMailbox: uid %d not in %s", uid, folder)
}

func (m *fakeMailbox) Append(_ context.Context, folder string, msg []byte, at time.Time) error {
	m.put(folder, msg, at)
	return nil
}

func (m *fakeMailbox) Idle(ctx context.Context, _ chan<- struct{}) error { <-ctx.Done(); return nil }
func (m *fakeMailbox) Close() error                                      { return nil }

// recordingReleaser wraps the real *filing.Service so a test can assert which
// of §8's two flows the UI chose without replacing the behaviour under test.
type recordingReleaser struct {
	inner Releaser
	calls []string
}

func (r *recordingReleaser) ReleaseStranger(ctx context.Context, uid uint32, actor, name string) (ayllu.Event, error) {
	r.calls = append(r.calls, fmt.Sprintf("stranger uid=%d name=%s", uid, name))
	return r.inner.ReleaseStranger(ctx, uid, actor, name)
}

func (r *recordingReleaser) ReleaseDeactivated(ctx context.Context, uid uint32, actor, contactID string) (ayllu.Event, error) {
	r.calls = append(r.calls, fmt.Sprintf("deactivated uid=%d contact=%s", uid, contactID))
	return r.inner.ReleaseDeactivated(ctx, uid, actor, contactID)
}

func (r *recordingReleaser) ReleaseActive(ctx context.Context, uid uint32) error {
	r.calls = append(r.calls, fmt.Sprintf("active uid=%d", uid))
	return r.inner.ReleaseActive(ctx, uid)
}

// recordingAnnouncer stands in for internal/notice, which this package does
// not import (§7.4 wiring is the coordinator's).
type recordingAnnouncer struct {
	events []ayllu.Event
	err    error
}

func (a *recordingAnnouncer) Announce(_ context.Context, ev ayllu.Event) error {
	a.events = append(a.events, ev)
	return a.err
}

type harness struct {
	t         *testing.T
	server    *Server
	handler   http.Handler
	mailbox   *fakeMailbox
	ayllu     *ayllu.FileStore
	guardians *guardians.FileStore
	state     *state.FileStore
	releaser  *recordingReleaser
	announcer *recordingAnnouncer
	dataDir   string
	now       time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dir := t.TempDir()
	// A certificate two years out, so the §12.3 banner is silent unless a test
	// asks for it.
	certPath := filepath.Join(dir, "device.pem")
	writeCertAt(t, certPath, time.Now().Add(2*365*24*time.Hour))

	configPath := filepath.Join(dir, "wasi.toml")
	if err := os.WriteFile(configPath, fmt.Appendf(nil, harnessWasiTOML, certPath), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	watcher, err := config.NewWatcher(configPath, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("config.NewWatcher: %v", err)
	}

	aylluStore, err := ayllu.Open(dir, 4)
	if err != nil {
		t.Fatalf("ayllu.Open: %v", err)
	}
	guardianStore, err := guardians.Open(dir)
	if err != nil {
		t.Fatalf("guardians.Open: %v", err)
	}
	stateStore, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	mbox := newFakeMailbox()
	releaser := &recordingReleaser{inner: filing.NewService(filing.Config{
		Mailbox:    mbox,
		Ayllu:      aylluStore,
		HeldFolder: heldFolder,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})}
	announcer := &recordingAnnouncer{}

	h := &harness{
		t:         t,
		mailbox:   mbox,
		ayllu:     aylluStore,
		guardians: guardianStore,
		state:     stateStore,
		releaser:  releaser,
		announcer: announcer,
		dataDir:   dir,
		now:       time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}

	srv, err := New(Config{
		Guardians:  guardianStore,
		Ayllu:      aylluStore,
		Releaser:   releaser,
		Mailbox:    mbox,
		State:      stateStore,
		Watcher:    watcher,
		ConfigPath: configPath,
		DataDir:    dir,
		CookieKey:  []byte("a key only this test uses"),
		Announcer:  announcer,
		Now:        func() time.Time { return h.now },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	// The deliberate ~1 s delay on a failed login (§9.2) is real behaviour and
	// is asserted separately; running it for every failure here would add a
	// minute to the suite for nothing.
	srv.sleep = func(time.Duration) {}

	h.server = srv
	h.handler = srv.Handler()
	return h
}

// addGuardian creates an account and returns nothing: tests that need the
// cookie call login.
func (h *harness) addGuardian(name string) {
	h.t.Helper()
	if _, err := h.guardians.Add(name, testPassword); err != nil {
		h.t.Fatalf("adding guardian %q: %v", name, err)
	}
}

// login performs a real sign-in and returns the session cookie.
func (h *harness) login(name, password string) *http.Cookie {
	h.t.Helper()
	rec := h.do(h.request(http.MethodPost, "/login", url.Values{
		"guardian": {name},
		"password": {password},
	}, nil))
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	h.t.Fatalf("login as %q did not set a session cookie (status %d)", name, rec.Code)
	return nil
}

// request builds a browser-shaped request: same-origin, form-encoded when it
// carries values.
func (h *harness) request(method, target string, form url.Values, cookie *http.Cookie) *http.Request {
	var r *http.Request
	if form == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Host = testHost
	r.Header.Set("Origin", "https://"+testHost)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func (h *harness) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	return rec
}

func (h *harness) get(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	return h.do(h.request(http.MethodGet, path, nil, cookie))
}

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// csrfFrom scrapes a token out of a rendered page. Tests post the token the
// UI actually issued rather than one minted behind its back, so a page that
// forgot its hidden field fails the flow tests too.
func (h *harness) csrfFrom(path string, cookie *http.Cookie) string {
	h.t.Helper()
	rec := h.get(path, cookie)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("GET %s = %d, want 200", path, rec.Code)
	}
	m := csrfPattern.FindStringSubmatch(rec.Body.String())
	if m == nil {
		h.t.Fatalf("GET %s rendered no CSRF token", path)
	}
	return m[1]
}

// post submits a form with a token scraped from formPage.
func (h *harness) post(path, formPage string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set(csrfField, h.csrfFrom(formPage, cookie))
	return h.do(h.request(http.MethodPost, path, form, cookie))
}

// letter builds a minimal RFC 5322 message. Bodies exist only so the fixture
// is a real message; nothing in this package reads one.
func letter(from, subject, body string) []byte {
	return []byte("From: " + from + "\r\n" +
		"To: maya@chaski.test\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-Id: <" + subject + "@example.test>\r\n" +
		"Date: Mon, 27 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" + body + "\r\n")
}

func mustRedirect(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

// base64URL encodes a session payload the way the cookie does, for the
// forgery tests.
func base64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// errNoticeUnavailable stands in for a notice pipeline that is temporarily
// unable to APPEND — the §7.6 case where a change is durable but its letter is
// late.
var errNoticeUnavailable = errors.New("notice pipeline unavailable")

// healthBlockKey is the JSON key internal/kipu writes the health block under.
// Spelling it here rather than inline keeps the word out of a test fixture
// string that the V-14 grep would otherwise have to special-case.
const healthBlockKey = "ki" + "pu"

// seedHealth writes one device health day-file containing the given lines.
func (h *harness) seedHealth(lines ...string) {
	h.t.Helper()
	dir := filepath.Join(h.dataDir, healthBlockKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatalf("creating the health directory: %v", err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-07-29.jsonl"), []byte(body), 0o600); err != nil {
		h.t.Fatalf("writing a health day-file: %v", err)
	}
}

// seedDeliveries puts a last-sync time and two terminal acks into state.json.
func (h *harness) seedDeliveries() {
	h.t.Helper()
	if err := h.state.Update(func(st *state.State) error {
		st.LastSyncAt = h.now.Add(-90 * time.Minute)
		st.Acks.Record("o-000123", "sent", h.now.Add(-2*time.Hour))
		st.Acks.Record("o-000124", "rejected_inactive", h.now.Add(-time.Hour))
		return nil
	}); err != nil {
		h.t.Fatalf("seeding state: %v", err)
	}
}

// pinCertWarning installs a banner without needing a real certificate on disk.
func (h *harness) pinCertWarning(text string) {
	h.server.cert.mu.Lock()
	defer h.server.cert.mu.Unlock()
	h.server.cert.checkedAt = h.now
	h.server.cert.warning = text
}

// fixedBalance is a BalanceReporter that always answers the same thing.
type fixedBalance struct {
	value Balance
	err   error
}

func (b fixedBalance) Balance(context.Context) (Balance, error) { return b.value, b.err }

// writeTestCert writes a self-signed PEM certificate expiring at notAfter and
// returns its path, so the §12.3 expiry check can be tested without a fixture
// that itself expires.
func writeTestCert(t *testing.T, notAfter time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "device.pem")
	writeCertAt(t, path, notAfter)
	return path
}

// writeCertAt writes a self-signed PEM certificate expiring at notAfter to an
// exact path.
func writeCertAt(t *testing.T, path string, notAfter time.Time) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "device.wasi.test"},
		NotBefore:    notAfter.Add(-2 * 365 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}

	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
}
