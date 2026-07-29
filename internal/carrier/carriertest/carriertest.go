// Package carriertest is the conformance suite every internal/carrier
// provider runs against (§10.4): "a new provider is one package plus a
// passing carriertest conformance run." It checks the four guarantees the
// Carrier interface promises regardless of who's on the other end of the
// HTTP call: one ring goes through, a transport error comes back as an
// error (not a panic or a silent success), context cancellation is honoured,
// and an unsupported optional capability reports ErrUnsupported rather than
// a zero value dressed up as a real answer.
package carriertest

import (
	"context"
	"errors"
	"testing"

	"github.com/tholent/chaskiwasi/internal/carrier"
)

// samplePayload is a syntactically valid doorbell token (§10.2's shape) used
// to exercise Pututu. Its exact bytes don't matter to this suite — no
// provider is expected to parse it — but using the real CH1.<counter>.<mac>
// shape keeps the conformance tests honest about what actually crosses the
// wire in production.
const samplePayload = "CH1.1.AAAAAAAAAAAAAAAAAAAA"

// Config supplies the factories Run needs to exercise one Carrier
// implementation. New is required; the others are optional, and Run skips
// (rather than silently passing) whatever it can't check without them, so a
// skipped subtest is visible in `go test -v` output instead of looking like
// a passing guarantee that was never actually exercised.
type Config struct {
	// New returns a healthy, freshly constructed Carrier ready to accept one
	// Pututu call successfully. Required.
	New func(t *testing.T) carrier.Carrier

	// Failing returns a Carrier whose next Pututu call fails with a
	// transport-shaped error — a dropped connection, a 5xx, a timeout —
	// distinct from ErrUnsupported.
	Failing func(t *testing.T) carrier.Carrier

	// BalanceUnsupported returns a Carrier configured such that Balance is a
	// capability it genuinely does not have right now (e.g. Hologram with no
	// org id configured, or a bare Fake).
	BalanceUnsupported func(t *testing.T) carrier.Carrier
}

// Run executes the conformance suite against cfg as subtests of t.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.New == nil {
		t.Fatal("carriertest: Config.New is required")
	}

	t.Run("Name", func(t *testing.T) {
		if name := cfg.New(t).Name(); name == "" {
			t.Error("Name() returned an empty string")
		}
	})

	t.Run("PututuRingsOnce", func(t *testing.T) {
		if err := cfg.New(t).Pututu(context.Background(), samplePayload); err != nil {
			t.Fatalf("Pututu on a healthy carrier: %v", err)
		}
	})

	t.Run("PututuSurfacesTransportErrors", func(t *testing.T) {
		if cfg.Failing == nil {
			t.Skip("no Failing factory supplied")
		}
		err := cfg.Failing(t).Pututu(context.Background(), samplePayload)
		if err == nil {
			t.Fatal("Pututu against a failing transport returned a nil error")
		}
		if errors.Is(err, carrier.ErrUnsupported) {
			t.Error("a transport error must not be reported as ErrUnsupported")
		}
	})

	t.Run("PututuHonoursContextCancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := cfg.New(t).Pututu(ctx, samplePayload); err == nil {
			t.Fatal("Pututu with an already-cancelled context returned a nil error")
		}
	})

	t.Run("BalanceUnsupportedDegrades", func(t *testing.T) {
		if cfg.BalanceUnsupported == nil {
			t.Skip("no BalanceUnsupported factory supplied")
		}
		bal, err := cfg.BalanceUnsupported(t).Balance(context.Background())
		if !errors.Is(err, carrier.ErrUnsupported) {
			t.Fatalf("Balance() on an unsupported carrier: err = %v, want ErrUnsupported", err)
		}
		if bal != (carrier.Balance{}) {
			t.Errorf("Balance() alongside ErrUnsupported = %+v, want the zero value (§10.4: degrade, don't return a zero value dressed as real data)", bal)
		}
	})

	t.Run("BalanceWhenSupportedIsWellFormed", func(t *testing.T) {
		bal, err := cfg.New(t).Balance(context.Background())
		if err != nil {
			if !errors.Is(err, carrier.ErrUnsupported) {
				t.Fatalf("Balance(): %v, want nil or ErrUnsupported", err)
			}
			return // this instance doesn't support it; a legitimate outcome (§10.4).
		}
		if bal.Currency == "" {
			t.Error("Balance() succeeded but Currency is empty")
		}
	})
}
