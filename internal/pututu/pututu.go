// Package pututu implements the SMS doorbell's provider-independent policy
// (§10.1–§10.3): coalescing, the skip-if-already-synced check, the signed
// counter token, and retry with backoff — everything that sits between
// internal/filing's raw "something arrived" signal and internal/carrier's
// bare Pututu(ctx, payload string) call.
//
// Glossary (design-spec §0.1): pututu = the conch shell blown on approach =
// the SMS doorbell that tells the device to sync now. It carries no
// information beyond that (§10.2) and this package enforces that in code:
// Doorbell.Ring takes no letter data, and Token's only inputs are an
// integer and a key, so there is nothing here that could leak a name or a
// letter's content even by accident.
package pututu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tholent/chaskiwasi/internal/carrier"
	"github.com/tholent/chaskiwasi/internal/filing"
	"github.com/tholent/chaskiwasi/internal/state"
)

// DefaultCoalesceMin mirrors config.DefaultCoalesceMin (§13's
// pututu.coalesce_min default). Duplicated as a time.Duration here rather
// than imported so this package has no dependency on internal/config; the
// coordinator converts the configured minutes to a Duration when building
// Config.
const DefaultCoalesceMin = 15 * time.Minute

const (
	defaultRetryBase = 2 * time.Second
	defaultRetryMax  = 2 * time.Minute

	// maxAttempts bounds retry: a doorbell ring is a hint that saves the
	// device a wait until its next periodic sync_interval_s wake, not the
	// only path a letter can travel by. Retrying forever would let one
	// stuck arrival hold a background goroutine hostage indefinitely, for no
	// benefit — nothing else in the system relies on the doorbell actually
	// landing.
	maxAttempts = 5
)

// Config configures a Doorbell.
type Config struct {
	// Carrier sends the actual SMS. Required.
	Carrier carrier.Carrier

	// State is the single writer of state.json (internal/state): Doorbell
	// reads LastSyncAt for the skip check (§10.1) and advances
	// PututuCounter for every token it mints (§10.2). Required.
	State state.Store

	// Key is the dedicated pututu HMAC key (§10.2) — a secret separate from
	// the device bearer token (Wasi stores only that token's hash and so
	// cannot MAC with it), sourced from internal/secrets and never TOML.
	// Required, non-empty.
	Key []byte

	// CoalesceMin is "at most one SMS per this window" (§10.1, §13's
	// pututu.coalesce_min). Defaults to DefaultCoalesceMin if <= 0.
	CoalesceMin time.Duration

	// RetryBase / RetryMax bound the backoff between transport-error
	// retries. Defaulted if <= 0; tests shrink them to keep the suite fast.
	RetryBase, RetryMax time.Duration

	// Logger defaults to slog.Default(). Never receives letter content or
	// contact names — Ring's signature makes that structurally impossible,
	// but nothing here should be given the chance to change that (I-1, I-2).
	Logger *slog.Logger

	// now overrides the clock. Test-only; nil means time.Now.
	now func() time.Time
}

// Doorbell implements filing.Doorbell (§10.1). Ring is the arrival/release
// signal — filing rings it holding its own mutex, so Ring must return
// quickly; see its doc comment for exactly how much of the send path stays
// synchronous versus backgrounded.
type Doorbell struct {
	carrier             carrier.Carrier
	state               state.Store
	key                 []byte
	coalesceMin         time.Duration
	retryBase, retryMax time.Duration
	log                 *slog.Logger
	now                 func() time.Time

	mu          sync.Mutex
	nextAllowed time.Time // coalescing gate: no new send attempt starts before this

	wg sync.WaitGroup // outstanding background retries

	// afterAttempt, if set, runs after each failed retry attempt, before the
	// next skip-check and backoff wait. Test-only hook (nil in production):
	// it is what lets a test deterministically interleave a multi-round
	// "more" drain (state.Update calls) between two retry attempts without
	// racing real goroutine scheduling.
	afterAttempt func()
}

var _ filing.Doorbell = (*Doorbell)(nil)

// NewDoorbell builds a Doorbell. It validates its required dependencies
// eagerly so a misconfiguration fails at startup, not on the first arrival.
func NewDoorbell(cfg Config) (*Doorbell, error) {
	if cfg.Carrier == nil {
		return nil, errors.New("pututu: Carrier is required")
	}
	if cfg.State == nil {
		return nil, errors.New("pututu: State is required")
	}
	if len(cfg.Key) == 0 {
		return nil, errors.New("pututu: Key is required (the dedicated pututu HMAC key, §10.2 — never the device bearer token)")
	}

	coalesceMin := cfg.CoalesceMin
	if coalesceMin <= 0 {
		coalesceMin = DefaultCoalesceMin
	}
	retryBase := cfg.RetryBase
	if retryBase <= 0 {
		retryBase = defaultRetryBase
	}
	retryMax := cfg.RetryMax
	if retryMax <= 0 {
		retryMax = defaultRetryMax
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	return &Doorbell{
		carrier:     cfg.Carrier,
		state:       cfg.State,
		key:         append([]byte(nil), cfg.Key...),
		coalesceMin: coalesceMin,
		retryBase:   retryBase,
		retryMax:    retryMax,
		log:         logger,
		now:         now,
	}, nil
}

// Ring implements filing.Doorbell: it is called on arrival to an active
// contact and on release from Held (§5.1, §8) — "a released letter is an
// arriving letter from the child's point of view," so both paths land here
// identically, with no separate release-flavoured logic.
//
// Ring does three things synchronously, on the caller's goroutine — safe to
// call while filing holds its own lock, because none of the three is slow
// at this system's scale (~10 syncs a day, one doorbell ring at most every
// coalesce_min): check and update the coalescing gate; check whether the
// device has already synced since this arrival (§10.1); if neither
// suppresses it, mint a token (one state.Update, the same synchronous
// fsync-and-rename cost internal/state's other callers already pay) and
// make exactly one send attempt.
//
// Only what happens after that first attempt fails is backgrounded: retrying
// with backoff (§10.1) can span minutes, and filing's mutex also guards
// Reconcile, CheckSpam, and the release flows, so blocking it for that long
// would be a real cost for no benefit — the device's own periodic sync is
// the backstop a delayed or ultimately abandoned doorbell falls back to.
func (d *Doorbell) Ring(ctx context.Context) {
	now := d.now()

	d.mu.Lock()
	if now.Before(d.nextAllowed) {
		d.mu.Unlock()
		d.log.Debug("pututu: ring coalesced", "window", d.coalesceMin.String())
		return
	}
	d.nextAllowed = now.Add(d.coalesceMin)
	d.mu.Unlock()

	if d.syncedSince(now) {
		d.log.Info("pututu: skipped, device already synced since the triggering arrival")
		return
	}

	token, err := d.mintToken()
	if err != nil {
		d.log.Error("pututu: minting token", "error", err)
		return
	}

	err = d.carrier.Pututu(ctx, token)
	if err == nil {
		d.log.Info("pututu: sent")
		return
	}
	d.log.Warn("pututu: send failed, will retry with backoff", "error", err)

	// Detach from ctx's cancellation before backgrounding: Ring's ctx is
	// scoped to the caller (one IDLE notification, one guardian release
	// request) and may be cancelled the moment that caller returns, but a
	// backgrounded retry must outlive it — only a real send, a skip, or
	// process shutdown (Close) should end it.
	bg := context.WithoutCancel(ctx)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.retry(bg, token, now)
	}()
}

// retry re-attempts a send that already failed once. It re-checks the skip
// condition before every attempt, which is exactly where it earns its keep:
// a transport-error retry is precisely the case where enough time passes for
// the device to sync on its own in the meantime, at which point continuing
// to retry would ring a doorbell nobody needs. The check reads a single
// LastSyncAt timestamp, so it is correct whether the intervening sync was
// one round or a ten-round "more" drain (§4.6) — there is nothing here that
// counts sync events, so there is nothing for a drain loop to multiply.
func (d *Doorbell) retry(ctx context.Context, token string, triggerAt time.Time) {
	bo := newBackoff(d.retryBase, d.retryMax)
	for attempt := 2; attempt <= maxAttempts; attempt++ {
		if !bo.wait(ctx) {
			d.log.Info("pututu: retry abandoned, context done")
			return
		}
		if d.afterAttempt != nil {
			d.afterAttempt()
		}
		if d.syncedSince(triggerAt) {
			d.log.Info("pututu: retry abandoned, device already synced since the triggering arrival")
			return
		}

		err := d.carrier.Pututu(ctx, token)
		if err == nil {
			d.log.Info("pututu: sent", "attempt", attempt)
			return
		}
		d.log.Warn("pututu: send failed, retrying", "attempt", attempt, "error", err)
	}
	d.log.Error("pututu: giving up after repeated transport errors", "attempts", maxAttempts)
}

// syncedSince reports whether the device has synced at or after triggerAt —
// the §10.1 skip condition, read straight off state.json's LastSyncAt mirror
// rather than any locally counted event, per retry's doc comment.
func (d *Doorbell) syncedSince(triggerAt time.Time) bool {
	return !d.state.Snapshot().LastSyncAt.Before(triggerAt)
}

// mintToken advances the persistent doorbell counter and builds the token
// for it (§10.2). Because NextPututuCounter reads and increments whatever
// value state.Store currently holds, a counter healed by
// ReconcilePututuCounter (§10.3, called from internal/syncsvc on the receipt
// of a device's pututu_counter_seen) is automatically what the very next
// mintToken call builds on — there is no separate "resume sending" step for
// a restored state.json to wire up.
func (d *Doorbell) mintToken() (string, error) {
	var counter uint64
	err := d.state.Update(func(s *state.State) error {
		counter = s.NextPututuCounter()
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("pututu: recording counter: %w", err)
	}
	return Token(counter, d.key), nil
}

// Close waits for any in-flight retry to finish, or ctx to expire first.
// Call it during graceful shutdown so a process restart doesn't orphan a
// retry mid-backoff; it is also the synchronization point tests use to
// observe Ring's effects deterministically instead of racing the background
// goroutine it may have started.
func (d *Doorbell) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
