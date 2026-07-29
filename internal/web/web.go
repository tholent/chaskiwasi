// Package web is the guardian UI (wasi-server-plan §9): html/template plus a
// vendored copy of htmx served from an embed.FS, no build step, no CDN.
//
// # What guardians can see here, and what they cannot
//
// Two invariants shape every page in this package, and both are easy to
// violate by adding an obviously helpful field:
//
//   - I-2 — addresses live in ayllu.toml and the change log, and the device
//     never sees one. In this UI they therefore appear in exactly the places
//     where the guardian is *managing* addresses: contact management, the
//     Held review that feeds the "add as contact" decision, and the change
//     log (§7.4 explicitly points guardians here for the old/new address).
//     Nothing this package renders is device-bound, so nothing here can leak
//     one onto the wire — but nothing here may be copied into something that
//     is.
//   - I-1 — no letter content is persisted, and none of it goes to a log at
//     any level. The deliveries panel shows ids and timestamps, never
//     content. Held review does show a sender and a subject line, because a
//     review that shows neither is not a review and the guardian already owns
//     the canonical mailbox; it never fetches or renders a body, and nothing
//     it reads reaches the logger.
//
// # Vocabulary boundary (§9.1, V-14)
//
// `pututu`, `ayllu`, and `kipu` are internal identifiers. They appear freely
// in this package's Go code and never in templates/ or in rendered output:
// guardians read "Contacts", "Held Messages", and "Device health". The
// enforcing test is TestV14_VocabularyBoundary, which greps both the template
// sources and the actual rendered HTML of every page.
//
// # Sessions
//
// Stateless signed cookies (§9.2). There is no session store, so a restart
// logs everyone out — a non-event at two or three accounts, and the thing
// that makes a password change able to invalidate sessions without one. See
// session.go and internal/guardians.
//
// # Seams left open on purpose
//
// This package does not import internal/notice or internal/carrier. Contact
// mutations return an ayllu.Event which is handed to an Announcer; the SMS
// balance panel reads through a BalanceReporter. Both default to a no-op so
// the UI runs correctly before those packages are wired in, and both are the
// single obvious place the wiring goes.
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/config"
	"github.com/tholent/chaskiwasi/internal/guardians"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/state"
)

// Releaser is the release half of §8, implemented by *filing.Service. It is
// an interface here only so handler tests can observe the calls: this package
// must never reimplement release, because filing owns the guarantee that
// nothing vanishes when a release half-fails (V-18).
type Releaser interface {
	// ReleaseStranger is "add as contact, then release".
	ReleaseStranger(ctx context.Context, uid uint32, actor, name string) (ayllu.Event, error)
	// ReleaseDeactivated is "reactivate, then release".
	ReleaseDeactivated(ctx context.Context, uid uint32, actor, contactID string) (ayllu.Event, error)
	// ReleaseActive is "just release": the sender already resolves to an
	// active contact, so nothing is mutated and nothing is announced. It
	// returns no event because there is no change to announce — announcing a
	// reactivation that did not happen would put a false statement into the
	// family record (§8, and see filing.ReleaseActive).
	ReleaseActive(ctx context.Context, uid uint32) error
}

// Announcer receives the ayllu.Event that every contact mutation produces
// (§7.4, I-4: nothing about the contact list changes silently). internal/notice
// implements it; this package deliberately does not import that package, so
// the wiring is a single field on Config.
//
// Announce is called after the mutation is already durable, and its error
// never fails the request: §7.6 fixes the ordering so that the purchasable
// failure is "notice arrives a little late", never "change happened
// silently". A failure is logged and shown to the guardian; the change stands
// and the pending-notice flush is what eventually delivers the letter.
type Announcer interface {
	Announce(ctx context.Context, ev ayllu.Event) error
}

// AnnouncerFunc adapts a plain function to Announcer.
type AnnouncerFunc func(ctx context.Context, ev ayllu.Event) error

// Announce implements Announcer.
func (f AnnouncerFunc) Announce(ctx context.Context, ev ayllu.Event) error { return f(ctx, ev) }

// Balance mirrors carrier.Balance. It is redeclared rather than imported so
// this package has no dependency on the carrier registry; the wiring in
// cmd/wasi adapts one to the other in four lines.
type Balance struct {
	Amount   float64
	Currency string
}

// BalanceReporter reports remaining prepaid SMS credit (§10.4). Returning
// ErrBalanceUnsupported hides the panel; it is not an error condition, it is
// a provider without the concept, and the UI must degrade rather than shout.
type BalanceReporter interface {
	Balance(ctx context.Context) (Balance, error)
}

// ErrBalanceUnsupported is the web-side spelling of carrier.ErrUnsupported.
// The adapter in cmd/wasi maps one onto the other.
var ErrBalanceUnsupported = errors.New("web: balance is not supported by the configured carrier")

// Config wires the guardian UI to the rest of the server.
type Config struct {
	// Guardians is the account table behind login and password change (§9.2).
	Guardians guardians.Store
	// Ayllu is the contact list. Every mutation goes through Mutate — this
	// package never writes ayllu.toml itself (§9.1).
	Ayllu ayllu.Store
	// Releaser performs the two §8 release flows.
	Releaser Releaser
	// Mailbox is read live for the Held review — no mirror to fall out of
	// sync (§8).
	Mailbox mailbox.Mailbox
	// State supplies the recent-deliveries panel and last-sync time.
	State state.Store
	// Watcher is the hot-reloading wasi.toml. The UI reads it for the
	// read-only settings page and for the Held folder name.
	Watcher *config.Watcher
	// ConfigPath is where wasi.toml lives, shown verbatim on the settings
	// page: §9.1 requires the ownership boundary be visible, not merely
	// respected.
	ConfigPath string
	// DataDir is the server-owned directory (/data). The change log and the
	// device health day-files are read from it.
	DataDir string

	// CookieKey is the HMAC key for session cookies, from internal/secrets
	// (§3, §9.2). Required.
	CookieKey []byte

	// Announcer receives contact-change events; see Announcer. Optional.
	Announcer Announcer
	// Balance reports SMS credit; see BalanceReporter. Optional — a nil
	// reporter hides the panel exactly as ErrBalanceUnsupported does.
	Balance BalanceReporter

	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default. No letter content, subject, or
	// password reaches it (I-1).
	Logger *slog.Logger
}

// Server is the guardian UI. Build it with New and mount Handler on the
// guardian listener (§12.1).
type Server struct {
	guardians guardians.Store
	ayllu     ayllu.Store
	releaser  Releaser
	mailbox   mailbox.Mailbox
	state     state.Store
	watcher   *config.Watcher

	configPath string
	dataDir    string
	cookieKey  []byte
	announcer  Announcer
	balance    BalanceReporter

	now  func() time.Time
	log  *slog.Logger
	tmpl *renderer

	// cert caches the §12.3 expiry check so that rendering a page does not
	// mean parsing a PEM file.
	cert certState

	throttle *throttle
	// sleep is time.Sleep, replaced in tests so the deliberate ~1 s failure
	// delay (§9.2) does not make the handler suite take a minute to run. It is
	// unexported so no deployment can configure the delay away.
	sleep func(time.Duration)
}

// New validates the wiring and builds a Server. It parses every template up
// front so a broken template is a startup failure rather than a 500 on the
// one page nobody visited during testing.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Guardians == nil:
		return nil, errors.New("web: Guardians is required")
	case cfg.Ayllu == nil:
		return nil, errors.New("web: Ayllu is required")
	case cfg.Watcher == nil:
		return nil, errors.New("web: Watcher is required")
	case len(cfg.CookieKey) == 0:
		// Without a key there are no sessions, and a server that silently
		// generated its own would log everyone out on every restart with no
		// explanation (§9.2 names secrets as the source).
		return nil, errors.New("web: CookieKey is required (see internal/secrets)")
	}

	tmpl, err := newRenderer()
	if err != nil {
		return nil, err
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	announcer := cfg.Announcer
	if announcer == nil {
		announcer = unwiredAnnouncer{log: logger}
	}

	return &Server{
		guardians:  cfg.Guardians,
		ayllu:      cfg.Ayllu,
		releaser:   cfg.Releaser,
		mailbox:    cfg.Mailbox,
		state:      cfg.State,
		watcher:    cfg.Watcher,
		configPath: cfg.ConfigPath,
		dataDir:    cfg.DataDir,
		cookieKey:  cfg.CookieKey,
		announcer:  announcer,
		balance:    cfg.Balance,
		now:        now,
		log:        logger,
		tmpl:       tmpl,
		throttle:   newThrottle(now),
		sleep:      time.Sleep,
	}, nil
}

// Handler returns the mux for the guardian listener (§12.1). Every
// state-changing route is registered METHOD-qualified, so a GET to a mutating
// path is a 405 from the router rather than a check a future handler could
// forget (§9.2's "every state-changing action must be POST").
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.requireSession(s.handleLogout))

	mux.HandleFunc("GET /{$}", s.requireSession(s.handleDashboard))
	mux.HandleFunc("GET /status/device", s.requireSession(s.handleDeviceStatusFragment))

	mux.HandleFunc("GET /contacts", s.requireSession(s.handleContacts))
	mux.HandleFunc("POST /contacts/add", s.requireSession(s.handleContactAdd))
	mux.HandleFunc("POST /contacts/deactivate", s.requireSession(s.handleContactDeactivate))
	mux.HandleFunc("POST /contacts/reactivate", s.requireSession(s.handleContactReactivate))
	mux.HandleFunc("POST /contacts/readdress", s.requireSession(s.handleContactReaddress))

	mux.HandleFunc("GET /held", s.requireSession(s.handleHeld))
	mux.HandleFunc("GET /held/list", s.requireSession(s.handleHeldFragment))
	mux.HandleFunc("POST /held/release", s.requireSession(s.handleHeldRelease))

	mux.HandleFunc("GET /changes", s.requireSession(s.handleChangeLog))
	mux.HandleFunc("GET /settings", s.requireSession(s.handleSettings))

	mux.HandleFunc("GET /account", s.requireSession(s.handleAccount))
	mux.HandleFunc("POST /account/password", s.requireSession(s.handlePasswordChange))
	mux.HandleFunc("POST /account/add", s.requireSession(s.handleGuardianAdd))

	return mux
}

// unwiredAnnouncer is the default when Config.Announcer is nil. It logs a
// warning rather than doing nothing quietly: running with no notice pipeline
// means contact changes are happening without the letter I-4 requires, which
// is a deployment defect and should read like one.
type unwiredAnnouncer struct{ log *slog.Logger }

func (a unwiredAnnouncer) Announce(_ context.Context, ev ayllu.Event) error {
	a.log.Warn("web: contact change made with no notice pipeline wired",
		"action", ev.Action, "contact_id", ev.ContactID)
	return nil
}

// announce hands ev to the notice pipeline and reports whether the guardian
// should be told the letter is late. The mutation is already durable by the
// time this runs (§7.6), so a failure here is never fatal to the request.
func (s *Server) announce(ctx context.Context, ev ayllu.Event) bool {
	if err := s.announcer.Announce(ctx, ev); err != nil {
		s.log.Error("web: announcing a contact change failed",
			"action", ev.Action, "contact_id", ev.ContactID, "error", err)
		return false
	}
	return true
}

// heldFolder reads the Held folder name from the live config, so a hot reload
// of wasi.toml is picked up without a restart (§13).
func (s *Server) heldFolder() string {
	if cfg := s.watcher.Current(); cfg != nil && cfg.Mail.HeldFolder != "" {
		return cfg.Mail.HeldFolder
	}
	return config.DefaultHeldFolder
}

// maxContacts reads the cap from the live config (§13, A.3).
func (s *Server) maxContacts() int {
	if cfg := s.watcher.Current(); cfg != nil && cfg.Ayllu.MaxContacts > 0 {
		return cfg.Ayllu.MaxContacts
	}
	return config.DefaultMaxContacts
}

// redirect performs the POST/redirect/GET half of every mutation. The outcome
// travels as a short opaque code, never as free text: it keeps guardian input
// — and above all addresses — out of URLs, browser history, and any proxy log
// in between (I-2).
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path string, code flashCode) {
	target := path
	if code != "" {
		target = fmt.Sprintf("%s?m=%s", path, code)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
