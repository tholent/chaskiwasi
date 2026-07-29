// Package strip is derivation's quote-stripping boundary: an HTTP client for
// the shared strip service (wasi-server-plan §11.1), plus the in-process
// fallback rules used when that service is unreachable (§5.3).
//
// The defining property of this package is that Strip never fails the
// caller. A Python container being down must not delay a letter from
// family: when the live service can't be reached, Client.Strip silently
// falls back to a much simpler local rule set and says so via Result.
// Degraded, rather than returning an error derivation would have to decide
// what to do with. Nothing here is cached (mirroring internal/mailbox, and
// for the same reason, V-10): the next sync after the service comes back
// re-derives the letter cleanly, because there is no stale copy anywhere to
// invalidate.
package strip

// Result is the outcome of stripping one letter body, from either the live
// service or the fallback rules.
type Result struct {
	// Body is the text with quoted tails removed (or as much of that as the
	// path taken could manage).
	Body string

	// Trimmed reports that a quoted tail was actually removed — distinct
	// from Degraded, which reports which code path produced Body. The wire
	// carries both as separate flags for a reason (§4.3): a device can tell
	// "we cut something" apart from "we cut it with the good tool".
	Trimmed bool

	// Degraded reports that the live service was unreachable and the
	// in-process fallback rules were used instead of talon.quotations
	// (§5.3). The fallback is deliberately narrower than the real service —
	// see fallback.go — so Degraded is what tells derivation (and
	// eventually the device) that this body may be under-trimmed.
	Degraded bool
}
