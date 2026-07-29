package chaskisim

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

// pututuPrefix is the fixed token version tag (§10.2).
const pututuPrefix = "CH1"

// macBytes is how many bytes of the HMAC-SHA256 digest the server includes,
// base64-encoded, in a token (§10.2: "the first 12 bytes ... base64 of").
const macBytes = 12

// computeMAC reproduces the server's token MAC (§10.2): base64 of the first
// 12 bytes of HMAC-SHA256 over the ASCII decimal counter, keyed by the
// pututu key. Exported so tests (here and in test/e2e) can mint tokens
// exactly the way a real server would, without a running server.
func computeMAC(counter uint64, key []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(strconv.FormatUint(counter, 10)))
	sum := h.Sum(nil)
	return base64.StdEncoding.EncodeToString(sum[:macBytes])
}

// MintPututuToken builds a CH1.<counter>.<mac> token for the given counter
// and key. It exists for tests: to check that AcceptPututu correctly rejects
// a bad token, something first has to be able to construct a good one
// without standing up internal/pututu (a later wave).
func MintPututuToken(counter uint64, key []byte) string {
	return pututuPrefix + "." + strconv.FormatUint(counter, 10) + "." + computeMAC(counter, key)
}

// verifyPututuToken checks a CH1.<counter>.<mac> token's shape and MAC
// against key, returning the counter it carries. It does not consult device
// state — that is AcceptPututu's job — so this function alone answers only
// "is this token authentic," not "should this device act on it."
func verifyPututuToken(token string, key []byte) (counter uint64, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != pututuPrefix {
		return 0, false
	}
	counter, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}

	got, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, false
	}
	want, err := base64.StdEncoding.DecodeString(computeMAC(counter, key))
	if err != nil {
		return 0, false
	}
	if !hmac.Equal(got, want) {
		return 0, false
	}
	return counter, true
}

// PututuResult reports what AcceptPututu decided, for a caller (the CLI, or
// an e2e assertion) that wants to observe it. A real device makes none of
// this externally visible (§10.2: "ignore failures silently — no response,
// no wake"); this struct exists only because a test tool that can't be
// inspected can't be used to test anything.
type PututuResult struct {
	// Valid reports whether the token's shape and MAC checked out.
	Valid bool
	// Accepted reports whether Valid was true AND the counter was strictly
	// greater than the previously stored value (§10.2) — the two conditions
	// that together mean PututuCounterSeen actually advanced.
	Accepted bool
	// WouldWake reports whether this event would actually wake the device
	// and trigger a sync, after the independent 5-minute rate limit (§10.2)
	// that applies regardless of token validity. It can be true only if
	// Accepted is true.
	WouldWake bool
	Counter   uint64
}

// AcceptPututu implements the device-side rules of §10.2 for one incoming
// SMS doorbell token: verify the MAC, accept only if counter is strictly
// greater than the highest previously accepted value, persist that value
// (the caller is expected to Save the Device afterward — AcceptPututu only
// mutates in-memory state, matching every other Device method), and
// independently rate-limit actual wakes to at most one per 5 minutes
// regardless of validity, so a validation bug alone can never become a
// battery- or balance-drain attack.
//
// A failure — bad shape, bad MAC, or a counter that is not strictly greater
// — changes nothing and is reported only through the returned PututuResult;
// a real device does not even do that much (§10.2: "ignore failures
// silently — no response, no wake").
func (d *Device) AcceptPututu(token string, key []byte) PututuResult {
	d.mu.Lock()
	defer d.mu.Unlock()

	counter, valid := verifyPututuToken(token, key)
	if !valid || counter <= d.state.PututuCounterSeen {
		return PututuResult{Valid: valid, Counter: counter}
	}

	d.state.PututuCounterSeen = counter

	now := d.now()
	wouldWake := d.state.LastPututuWakeAt.IsZero() || now.Sub(d.state.LastPututuWakeAt) >= PututuWakeRateLimit
	if wouldWake {
		d.state.LastPututuWakeAt = now
	}

	return PututuResult{Valid: true, Accepted: true, WouldWake: wouldWake, Counter: counter}
}
