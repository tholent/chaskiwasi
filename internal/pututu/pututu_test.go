package pututu

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/carrier"
	"github.com/tholent/chaskiwasi/internal/state"
)

var testKey = []byte("test-pututu-key-do-not-use-in-prod")

func testState(t *testing.T) state.Store {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return st
}

// clock is a controllable time source for tests: Now() reads a value that
// advance moves forward, so a test can express "30 seconds later" without a
// real sleep.
type clock struct{ t time.Time }

func newClock(start time.Time) *clock { return &clock{t: start} }
func (c *clock) Now() time.Time       { return c.t }
func (c *clock) advance(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}

func newTestDoorbell(t *testing.T, c carrier.Carrier, st state.Store, clk *clock) *Doorbell {
	t.Helper()
	d, err := NewDoorbell(Config{
		Carrier:     c,
		State:       st,
		Key:         testKey,
		CoalesceMin: 15 * time.Minute,
		RetryBase:   time.Millisecond,
		RetryMax:    2 * time.Millisecond,
		now:         func() time.Time { return clk.Now() },
	})
	if err != nil {
		t.Fatalf("NewDoorbell: %v", err)
	}
	return d
}

// --- Construction --------------------------------------------------------

func TestNewDoorbell_RequiresDependencies(t *testing.T) {
	fake := carrier.NewFake()
	st := testState(t)

	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing carrier", Config{State: st, Key: testKey}},
		{"missing state", Config{Carrier: fake, Key: testKey}},
		{"missing key", Config{Carrier: fake, State: st}},
		{"empty key", Config{Carrier: fake, State: st, Key: []byte{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewDoorbell(tt.cfg); err == nil {
				t.Fatal("NewDoorbell: got no error, want one")
			}
		})
	}
}

// --- Token shape -----------------------------------------------------------

var tokenShape = regexp.MustCompile(`^CH1\.[0-9]+\.[A-Za-z0-9+/]+=*$`)

// TestToken_OpaqueShape asserts the wire shape (§10.2) and, by construction,
// that a token can never carry a sender name or letter content: Token's only
// parameters are a counter and a key, so there is nothing else it could
// encode.
func TestToken_OpaqueShape(t *testing.T) {
	tok := Token(41, testKey)
	if !tokenShape.MatchString(tok) {
		t.Fatalf("Token(41, key) = %q, does not match CH1.<counter>.<mac>", tok)
	}
	// A GSM-7 SMS is 160 septets; the token must fit comfortably (§10.2).
	if len(tok) > 40 {
		t.Errorf("token length = %d, want well under one SMS (160 chars)", len(tok))
	}
}

func TestToken_DifferentCountersDifferentTokens(t *testing.T) {
	if Token(1, testKey) == Token(2, testKey) {
		t.Fatal("Token(1, key) == Token(2, key), want distinct tokens per counter")
	}
}

// --- V-13: coalescing, skip-if-synced, and the 10-round drain -------------

// TestV13_CoalescesRapidArrivals: two letters 30 s apart -> exactly one
// pututu.
func TestV13_CoalescesRapidArrivals(t *testing.T) {
	fake := carrier.NewFake()
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	d.Ring(context.Background())
	clk.advance(30 * time.Second)
	d.Ring(context.Background())

	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := len(fake.Sent()); got != 1 {
		t.Fatalf("Sent() has %d entries after two arrivals 30s apart, want exactly 1", got)
	}
}

// TestV13_CoalescingWindowReopensAfterward checks the coalescing gate is a
// window, not a permanent lock: an arrival after coalesce_min has elapsed
// gets its own SMS.
func TestV13_CoalescingWindowReopensAfterward(t *testing.T) {
	fake := carrier.NewFake()
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	d.Ring(context.Background())
	clk.advance(15*time.Minute + time.Second)
	d.Ring(context.Background())

	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(fake.Sent()); got != 2 {
		t.Fatalf("Sent() has %d entries after two arrivals a coalesce window apart, want 2", got)
	}
}

// TestV13_SkipsIfDeviceSyncedSinceArrival: none if the device synced after
// arrival.
func TestV13_SkipsIfDeviceSyncedSinceArrival(t *testing.T) {
	fake := carrier.NewFake()
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	// The device already synced one second after "now" — modelling a sync
	// that raced ahead of this arrival's doorbell processing and delivered
	// the letter on its own (§10.1).
	arrival := clk.Now()
	if err := st.Update(func(s *state.State) error {
		s.LastSyncAt = arrival.Add(time.Second)
		return nil
	}); err != nil {
		t.Fatalf("state.Update: %v", err)
	}

	d.Ring(context.Background())
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := len(fake.Sent()); got != 0 {
		t.Fatalf("Sent() has %d entries, want 0 (device already synced since arrival)", got)
	}
}

// TestV13_TenRoundDrainCountsAsOneSync exercises the skip check across a
// retry: the first send attempt fails (transport error), and while the
// backgrounded retry is waiting, a simulated 10-round "more" drain updates
// LastSyncAt ten times in a row — exactly what syncsvc's commitSync does
// once per round (§4.6). The retry's skip check reads only the final
// LastSyncAt value, so this must abandon the retry precisely as cleanly as
// a single-round sync would: nothing here counts sync events, so a drain
// loop has nothing to multiply (§4.6, §10.1).
func TestV13_TenRoundDrainCountsAsOneSync(t *testing.T) {
	fake := carrier.NewFake()
	fake.FailNext(1, errors.New("simulated transport error"))
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	arrival := clk.Now()
	drained := false
	d.afterAttempt = func() {
		if drained {
			return
		}
		drained = true
		for i := 0; i < 10; i++ {
			if err := st.Update(func(s *state.State) error {
				s.LastSyncAt = arrival.Add(time.Duration(i+1) * time.Millisecond)
				return nil
			}); err != nil {
				t.Errorf("simulated drain round %d: state.Update: %v", i, err)
			}
		}
	}

	d.Ring(context.Background())
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !drained {
		t.Fatal("afterAttempt never ran; the retry path was not exercised")
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("carrier saw %d attempts, want exactly 1 (the initial failure; the retry must be abandoned by the drain)", len(calls))
	}
	if len(fake.Sent()) != 0 {
		t.Fatalf("Sent() = %v, want empty: the drain should have abandoned the retry before any successful send", fake.Sent())
	}
}

// --- Retry with backoff ----------------------------------------------------

func TestRetry_SucceedsAfterTransientTransportError(t *testing.T) {
	fake := carrier.NewFake()
	fake.FailNext(2, errors.New("simulated transport error"))
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	d.Ring(context.Background())
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("Sent() = %v, want exactly 1 successful send after two transient failures", sent)
	}
	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("carrier saw %d attempts, want 3 (2 failures + 1 success)", len(calls))
	}
}

func TestRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	fake := carrier.NewFake()
	fake.FailNext(maxAttempts, errors.New("permanently down"))
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	d.Ring(context.Background())
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := len(fake.Sent()); got != 0 {
		t.Fatalf("Sent() has %d entries, want 0 after exhausting retries", got)
	}
	if got := len(fake.Calls()); got != maxAttempts {
		t.Fatalf("carrier saw %d attempts, want exactly %d (bounded retry)", got, maxAttempts)
	}
}

// TestRing_RetryOutlivesItsCallerContext confirms the detach documented on
// Ring: the ctx passed to one Ring call is scoped to that caller (filing
// holds it only for the duration of one HandleNotify pass) and may be
// cancelled the instant Ring returns, but a retry already under way must not
// be cut short by that — only Close, or the retry's own terminal outcome,
// ends it.
func TestRing_RetryOutlivesItsCallerContext(t *testing.T) {
	fake := carrier.NewFake()
	fake.FailNext(1, errors.New("simulated transport error"))
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	ctx, cancel := context.WithCancel(context.Background())
	d.Ring(ctx)
	cancel() // the caller's context is gone before the retry has run

	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := len(fake.Sent()); got != 1 {
		t.Fatalf("Sent() has %d entries, want 1: a retry must outlive its caller's context", got)
	}
}

// TestClose_ReturnsContextErrorIfRetriesOutlastIt exercises Close's own
// timeout: it is a bounded wait, not an unconditional block, so a caller
// doing graceful shutdown can still give up.
func TestClose_ReturnsContextErrorIfRetriesOutlastIt(t *testing.T) {
	fake := carrier.NewFake()
	fake.FailNext(maxAttempts, errors.New("permanently down"))
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)
	d.retryBase = 50 * time.Millisecond
	d.retryMax = 50 * time.Millisecond

	d.Ring(context.Background())

	shortCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := d.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close with a short deadline while a retry is in flight: err = %v, want DeadlineExceeded", err)
	}

	// Drain the still-running retry so it doesn't outlive the test.
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("final Close: %v", err)
	}
}
