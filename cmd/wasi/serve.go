package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/carrier"
	"github.com/tholent/chaskiwasi/internal/config"
	"github.com/tholent/chaskiwasi/internal/derive"
	"github.com/tholent/chaskiwasi/internal/filing"
	"github.com/tholent/chaskiwasi/internal/guardians"
	"github.com/tholent/chaskiwasi/internal/kipu"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/notice"
	"github.com/tholent/chaskiwasi/internal/pututu"
	"github.com/tholent/chaskiwasi/internal/secrets"
	"github.com/tholent/chaskiwasi/internal/state"
	"github.com/tholent/chaskiwasi/internal/strip"
	"github.com/tholent/chaskiwasi/internal/syncsvc"
	"github.com/tholent/chaskiwasi/internal/web"
)

// Default container paths (§3): /config is bind-mounted read-only, /data is
// the only writable location, and both are singular because the container is
// the device's identity (§2).
const (
	defaultConfigPath = "/config/wasi.toml"
	defaultDataDir    = "/data"
)

// shutdownGrace bounds how long an in-flight sync may finish after SIGTERM.
// A sync is a handful of IMAP fetches and at most a dozen SMTP submissions;
// past this it is better to drop the connection than to delay the restart,
// since the device retries the identical request safely (§4.1).
const shutdownGrace = 20 * time.Second

// readyzTimeout bounds the IMAP probe behind /readyz (§14). Short on purpose:
// the question is "is the mailbox reachable right now", and a probe that hangs
// is itself the answer.
const readyzTimeout = 5 * time.Second

// filingStartRetry is how long to wait before retrying filing's startup pass
// (§5.1) when the mailbox is unreachable at boot. The listeners are already up
// by then, so this only delays quarantine, which the per-sync reconciliation
// pass covers in the meantime.
const filingStartRetry = 30 * time.Second

// runServe wires the process together and runs both TLS listeners (§12.1).
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "path to wasi.toml (human-owned, read-only)")
	dataDir := fs.String("data", defaultDataDir, "path to the server-owned data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// JSON slog to stdout (§14). No letter body or subject reaches this logger
	// at any level (I-1); letter ids are logged where correlation is needed.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	sec, err := secrets.Load()
	if err != nil {
		return err
	}

	watcher, err := config.NewWatcher(*configPath, 0, logger)
	if err != nil {
		return err
	}
	cfg := watcher.Current()

	stateStore, err := state.Open(*dataDir)
	if err != nil {
		return err
	}
	aylluStore, err := ayllu.Open(*dataDir, cfg.Ayllu.MaxContacts)
	if err != nil {
		return err
	}
	kipuLog, err := kipu.Open(filepath.Join(*dataDir, "kipu"))
	if err != nil {
		return err
	}
	guardianStore, err := guardians.Open(*dataDir)
	if err != nil {
		return err
	}

	mbox := mailbox.NewIMAPMailbox(mailbox.Config{
		Addr:     cfg.Mail.IMAP,
		Username: cfg.Mail.Address,
		Password: sec.IMAPPassword,
		Logger:   logger,
	})
	defer mbox.Close()

	submitter := mailbox.NewSMTPSubmitter(mailbox.SMTPConfig{
		Addr:     cfg.Mail.SMTP,
		Username: cfg.Mail.Address,
		// One provider credential serves both endpoints: secrets holds a
		// mailbox password, not a per-protocol one (§3).
		Password: sec.IMAPPassword,
		Logger:   logger,
	})

	deriver, err := derive.New(derive.Config{
		Ayllu: aylluStore,
		Strip: strip.NewClient(strip.Config{
			BaseURL: cfg.Services.StripURL,
			Token:   sec.ServiceToken,
			Logger:  logger,
		}),
		MaxLetterChars: cfg.Sync.MaxLetterChars,
		Logger:         logger,
	})
	if err != nil {
		return err
	}

	// The SMS doorbell (§10). An unconfigured carrier is normal and leaves
	// filing's no-op default in place: arrivals are still filed correctly,
	// the device just waits for its next scheduled sync instead of being rung.
	doorbell, closeDoorbell, err := newDoorbell(cfg, sec, stateStore, logger)
	if err != nil {
		return err
	}
	defer closeDoorbell()

	// Filing quarantines strangers' mail before derivation can let the cursor
	// pass it by (§5.1).
	filingSvc := filing.NewService(filing.Config{
		Mailbox:    mbox,
		Ayllu:      aylluStore,
		HeldFolder: cfg.Mail.HeldFolder,
		SpamFolder: cfg.Mail.SpamFolder,
		Doorbell:   doorbell,
		Logger:     logger,
	})

	// Notices are how I-4 stays true: every contact change becomes a letter in
	// the canonical mailbox, so neither party can alter the list behind the
	// other's back (§7.4).
	notices, err := notice.New(notice.Config{
		State:          stateStore,
		Mailbox:        mbox,
		Submitter:      submitter,
		MailboxAddress: cfg.Mail.Address,
		CopyAddresses:  cfg.Guardian.CopyAddresses,
		Logger:         logger,
	})
	if err != nil {
		return err
	}

	syncHandler, err := syncsvc.New(syncsvc.Deps{
		Config:    watcher,
		Ayllu:     aylluStore,
		State:     stateStore,
		Mailbox:   mbox,
		Submitter: submitter,
		Deriver:   deriver,
		Kipu:      kipuLog,
		// §5.1's "at the top of every sync" half: filing must not depend on
		// uptime, so a stranger's mail that arrived while Wasi was down is
		// quarantined before this sync's derivation can deliver it (test V-15).
		Reconciler: filingSvc,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	// The guardian web UI (§9).
	ui, err := web.New(web.Config{
		Guardians:  guardianStore,
		Ayllu:      aylluStore,
		Releaser:   filingSvc,
		Mailbox:    mbox,
		State:      stateStore,
		Watcher:    watcher,
		ConfigPath: *configPath,
		DataDir:    *dataDir,
		CookieKey:  sec.CookieSigningKey,
		Announcer:  notices,
		Balance:    balanceReporter(carrierFor(cfg, sec, logger)),
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Notice recovery, before anything can announce a new change (§7.6).
	// Flush re-drives what state.json remembers was in flight; Reconcile then
	// covers the wider window where the process died after ayllu.toml was
	// written but before pending_notices recorded anything owed — the change
	// log is the durable record there (F-5 in the implementation plan).
	if err := notices.Flush(ctx); err != nil {
		logger.Error("serve: flushing pending notices failed", "error", err)
	}
	if events, err := ayllu.ReadLog(*dataDir, time.Now().Add(-noticeReconcileWindow)); err != nil {
		logger.Error("serve: reading the contact change log failed", "error", err)
	} else if err := notices.Reconcile(ctx, events); err != nil {
		logger.Error("serve: reconciling notices against the change log failed", "error", err)
	}

	var background sync.WaitGroup
	background.Go(func() { watcher.Watch(ctx) })
	background.Go(func() { kipuLog.RunRetentionSweeper(ctx, cfg.Kipu.RetentionDays, logger) })
	background.Go(func() { runFiling(ctx, mbox, filingSvc, logger) })

	// Two listeners, two trust models, nothing shared but the process (§12.1).
	device := &http.Server{
		Addr:      cfg.Device.Listen,
		Handler:   deviceMux(syncHandler),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		ErrorLog:  slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	guardian := &http.Server{
		Addr:      cfg.Guardian.Listen,
		Handler:   guardianMux(watcher, mbox, ui.Handler(), logger),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		ErrorLog:  slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	errs := make(chan error, 2)
	go func() {
		logger.Info("serve: device listener starting", "addr", cfg.Device.Listen)
		errs <- serveTLS(device, cfg.Device.TLSCert, cfg.Device.TLSKey, "device")
	}()
	go func() {
		logger.Info("serve: guardian listener starting", "addr", cfg.Guardian.Listen)
		errs <- serveTLS(guardian, cfg.Guardian.TLSCert, cfg.Guardian.TLSKey, "guardian")
	}()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("serve: shutting down")
	case runErr = <-errs:
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	device.Shutdown(shutdownCtx)
	guardian.Shutdown(shutdownCtx)
	background.Wait()

	return runErr
}

// runFiling runs the two uptime-dependent halves of §5.1: the IDLE
// notification path and the periodic spam-folder backstop. The third half —
// reconciliation at the top of every sync — is wired into the sync handler and
// is what makes correctness independent of anything here staying connected.
//
// The startup pass is retried until it succeeds rather than merely logged,
// because filing.Service.HandleNotify must not run before Start has
// established which INBOX messages this process has already decided about:
// starting the notification consumer early would apply the active-only arrival
// rule to the entire mailbox history, which is exactly the failure F-2 and
// §7.2 ("the decision is made once, at arrival") exist to prevent.
func runFiling(ctx context.Context, mbox mailbox.Mailbox, svc *filing.Service, logger *slog.Logger) {
	for ctx.Err() == nil {
		err := svc.Start(ctx)
		if err == nil {
			break
		}
		logger.Error("serve: startup filing pass failed, retrying", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(filingStartRetry):
		}
	}
	if ctx.Err() != nil {
		return
	}

	notify := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Go(func() { svc.RunSpamBackstop(ctx) })
	wg.Go(func() {
		// Idle reconnects on its own and returns only when ctx is cancelled.
		if err := mbox.Idle(ctx, notify); err != nil && ctx.Err() == nil {
			logger.Error("serve: IDLE stopped", "error", err)
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-notify:
				if err := svc.HandleNotify(ctx); err != nil {
					// A notification path, not an ingest path: the next sync's
					// reconciliation covers whatever this missed (§5.1).
					logger.Error("serve: filing an arrival failed", "error", err)
				}
			}
		}
	})
	wg.Wait()
}

// serveTLS runs one listener and normalises the expected shutdown error away.
func serveTLS(srv *http.Server, certFile, keyFile, name string) error {
	if certFile == "" || keyFile == "" {
		return fmt.Errorf("serve: %s listener needs tls_cert and tls_key in wasi.toml (§12.1)", name)
	}
	err := srv.ListenAndServeTLS(certFile, keyFile)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve: %s listener: %w", name, err)
}

// deviceMux serves POST /sync and nothing else (§12.1). The device listener's
// leaf is signed by the private CA (§12.2); every other path on it is a
// mistake or a probe, and answering 404 to both is right.
func deviceMux(syncHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/sync", syncHandler)
	return mux
}

// guardianMux serves the human listener (§12.1): the ops endpoints — /healthz
// reports the process is up, /readyz reports IMAP reachable and config parsed
// (§14) — plus the guardian web UI (§9) on everything else.
//
// Both ops endpoints are deliberately outside the UI's authentication: a
// readiness probe that needs a session is not a readiness probe. Neither
// reveals anything about the family — one prints "ok", the other prints "ok"
// or a mailbox-unreachable line — and neither is registered on the device
// listener.
//
// This listener gets the public Let's Encrypt leaf and shares nothing with the
// device listener but the process. No handler registered here may ever serve
// device traffic or read the device bearer token.
func guardianMux(watcher *config.Watcher, mbox mailbox.Mailbox, ui http.Handler, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// The UI owns everything the ops endpoints do not. Registering it at "/"
	// after the two explicit patterns is safe: Go's ServeMux prefers the more
	// specific pattern regardless of registration order.
	mux.Handle("/", ui)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if watcher.Current() == nil {
			http.Error(w, "config unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
		defer cancel()
		if _, err := mbox.UIDValidity(ctx); err != nil {
			logger.Warn("readyz: mailbox unreachable", "error", err)
			http.Error(w, "mailbox unreachable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	return mux
}

// noticeReconcileWindow bounds how far back startup reconciliation reads the
// contact change log (F-5). It is deliberately much wider than any plausible
// outage: re-examining an already-announced change costs one INBOX comparison
// and produces nothing, while missing one means a change the child was never
// told about — the asymmetry runs entirely one way.
const noticeReconcileWindow = 90 * 24 * time.Hour

// carrierFor builds the configured SMS provider, or nil when none is
// configured. A missing carrier is a normal deployment state (§10.4), not an
// error: it costs the doorbell and the credit panel, nothing else.
func carrierFor(cfg *config.Config, sec *secrets.Secrets, logger *slog.Logger) carrier.Carrier {
	car, err := carrier.New(cfg.Carrier, sec.CarrierAPIKey)
	if err != nil {
		logger.Error("serve: carrier is configured but could not be built; the doorbell is disabled",
			"carrier", cfg.Carrier.Name, "error", err)
		return nil
	}
	return car
}

// newDoorbell wires the pututu doorbell (§10) onto the configured carrier.
//
// A configured carrier with no pututu key fails startup rather than degrading:
// the key is a separate secret by necessity (Wasi stores only a *hash* of the
// device bearer token and so cannot MAC with it, §10.2), and starting without
// it would leave an operator with a carrier bill, a healthy-looking process,
// and a doorbell that never rings — the silent failure §10.3 is written to
// prevent.
func newDoorbell(cfg *config.Config, sec *secrets.Secrets, st state.Store, logger *slog.Logger) (filing.Doorbell, func(), error) {
	car := carrierFor(cfg, sec, logger)
	if car == nil {
		return filing.NopDoorbell, func() {}, nil
	}
	if len(sec.PututuKey) == 0 {
		return nil, nil, fmt.Errorf("serve: carrier %q is configured but the pututu key secret is not set", car.Name())
	}

	d, err := pututu.NewDoorbell(pututu.Config{
		Carrier:     car,
		State:       st,
		Key:         sec.PututuKey,
		CoalesceMin: time.Duration(cfg.Pututu.CoalesceMin) * time.Minute,
		Logger:      logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("serve: building the doorbell: %w", err)
	}
	logger.Info("serve: doorbell enabled", "carrier", car.Name())

	return d, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			logger.Warn("serve: doorbell shutdown incomplete", "error", err)
		}
	}, nil
}

// carrierBalance adapts a carrier to the web UI's reporter. internal/web must
// not import internal/carrier — the UI degrades on ErrUnsupported rather than
// knowing what a provider is (§9.1, §10.4).
type carrierBalance struct{ c carrier.Carrier }

func (b carrierBalance) Balance(ctx context.Context) (web.Balance, error) {
	bal, err := b.c.Balance(ctx)
	if errors.Is(err, carrier.ErrUnsupported) {
		return web.Balance{}, web.ErrBalanceUnsupported
	}
	if err != nil {
		return web.Balance{}, err
	}
	return web.Balance{Amount: bal.Amount, Currency: bal.Currency}, nil
}

// balanceReporter returns nil for an absent carrier, which hides the credit
// panel exactly as ErrUnsupported does.
func balanceReporter(car carrier.Carrier) web.BalanceReporter {
	if car == nil {
		return nil
	}
	return carrierBalance{c: car}
}
