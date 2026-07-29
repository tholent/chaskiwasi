// Package carrier is the SMS provider boundary for the pututu doorbell.
//
// Glossary (design-spec §0.1): pututu = the conch blown on approach = the SMS
// notification that tells the device to sync now. It carries no information
// beyond that.
//
// The interface exists because providers disagree on device identity — Hologram
// keys on a device id, Soracom on an IMSI or SIM id — and that difference must
// not leak into core config or the pututu code (§10.4). A new provider is one
// package plus a passing carriertest conformance run.
package carrier

import (
	"context"
	"errors"
)

// Carrier is the provider contract (§10.4).
type Carrier interface {
	Name() string

	// Pututu rings the device. The payload is an opaque token by contract: no
	// sender name, no content. SMS is plaintext and carrier-buffered, and a name
	// in it would leak exactly what the allowlist protects. This constraint
	// lives in the contract so no future provider can quietly reintroduce it
	// (§10.2).
	Pututu(ctx context.Context, payload string) error

	// Balance reports remaining prepaid credit — the thing that keeps
	// design-spec Principle 6 true. Providers with no such concept return
	// ErrUnsupported, which hides the UI balance panel and silences the
	// low-credit alert rather than panicking (§10.4).
	Balance(ctx context.Context) (Balance, error)
}

// Balance is remaining prepaid credit.
type Balance struct {
	Amount   float64
	Currency string
}

// ErrUnsupported reports an optional capability the provider does not have.
// Optional capabilities degrade; they do not fail (§10.4).
var ErrUnsupported = errors.New("carrier: capability not supported by this provider")
