package web

import (
	"sync"
	"time"
)

// §9.2's login throttling. argon2id already prices the offline attack; this
// prices online guessing, which argon2id cannot touch because the attacker is
// paying our CPU rather than their own.
//
// Three pieces, each answering a different attack:
//
//   - failureDelay is a flat pause on every failed login regardless of count.
//     It caps a single attacker's guess rate from the first attempt, before
//     any counter has had a chance to trip.
//   - After throttleThreshold consecutive failures the account enters a
//     doubling lockout, so a sustained attack costs exponentially more wall
//     clock while a guardian who mistyped twice notices nothing.
//   - The lockout is capped, because an uncapped one is a denial-of-service
//     against the guardian: anyone who can reach the login page could
//     permanently lock a real account out by guessing at it. A 60 s ceiling
//     still reduces an attacker to about a thousand guesses a day.
//
// State is in memory only. That matches the stateless-session design (§9.2):
// a restart clears the counters, which an attacker cannot trigger and which
// costs a guardian nothing.
const (
	failureDelay      = time.Second
	throttleThreshold = 5
	throttleBase      = time.Second
	throttleCap       = 60 * time.Second
)

// throttle tracks consecutive login failures per account name.
//
// It is keyed by the *submitted* name, not by a resolved guardian, so that
// hammering a name that does not exist is throttled identically to hammering
// one that does — otherwise the lockout itself becomes the account-enumeration
// oracle that the constant-time verification in internal/guardians exists to
// close.
type throttle struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	// lockedUntil is when the next attempt on this account may be made. Zero
	// means no lockout in force.
	lockedUntil time.Time
}

func newThrottle(now func() time.Time) *throttle {
	return &throttle{now: now, entries: make(map[string]*throttleEntry)}
}

// check reports whether an attempt on name may proceed. When it may not, the
// second return is how long the caller should tell the guardian to wait.
//
// A locked-out attempt deliberately does not extend the lockout: an attacker
// who keeps hammering during the window must not be able to keep a guardian
// locked out indefinitely by doing so.
func (t *throttle) check(name string) (allowed bool, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[name]
	if !ok || e.lockedUntil.IsZero() {
		return true, 0
	}
	if remaining := e.lockedUntil.Sub(t.now()); remaining > 0 {
		return false, remaining
	}
	return true, 0
}

// fail records a failed attempt and returns the lockout now in force, or zero
// if the account is still below the threshold.
func (t *throttle) fail(name string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[name]
	if !ok {
		e = &throttleEntry{}
		t.entries[name] = e
	}
	e.failures++
	if e.failures < throttleThreshold {
		e.lockedUntil = time.Time{}
		return 0
	}

	backoff := backoffFor(e.failures)
	e.lockedUntil = t.now().Add(backoff)
	return backoff
}

// succeed clears the counter. Only a correct password does this — not the
// mere passage of time — so an attacker cannot wait out the doubling and start
// again from one delay.
func (t *throttle) succeed(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, name)
}

// backoffFor is the doubling schedule: 1 s at the fifth consecutive failure,
// then 2, 4, 8 … capped at throttleCap.
func backoffFor(failures int) time.Duration {
	if failures < throttleThreshold {
		return 0
	}
	backoff := throttleBase
	for i := throttleThreshold; i < failures; i++ {
		backoff *= 2
		if backoff >= throttleCap {
			return throttleCap
		}
	}
	return backoff
}
