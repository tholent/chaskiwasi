package web

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestBackoffFor(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{1, 0},
		{4, 0},
		{5, time.Second},
		{6, 2 * time.Second},
		{7, 4 * time.Second},
		{10, 32 * time.Second},
		{11, throttleCap},
		{50, throttleCap}, // capped: an uncapped lockout is a way to lock a real guardian out
	}
	for _, tc := range tests {
		if got := backoffFor(tc.failures); got != tc.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

func TestThrottle_SuccessClearsTheCounter(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	th := newThrottle(func() time.Time { return now })

	for range throttleThreshold {
		th.fail("dad")
	}
	if allowed, _ := th.check("dad"); allowed {
		t.Fatal("the account is not locked after five failures")
	}

	now = now.Add(2 * time.Second)
	if allowed, _ := th.check("dad"); !allowed {
		t.Fatal("the lockout did not expire")
	}
	th.succeed("dad")

	// After a success the schedule restarts: four more failures must not lock.
	for range throttleThreshold - 1 {
		th.fail("dad")
	}
	if allowed, _ := th.check("dad"); !allowed {
		t.Fatal("the counter was not cleared by a successful sign-in")
	}
}

func TestThrottle_AttemptsDuringLockoutDoNotExtendIt(t *testing.T) {
	// An attacker hammering during the window must not be able to keep a real
	// guardian locked out indefinitely.
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	th := newThrottle(func() time.Time { return now })

	for range throttleThreshold {
		th.fail("dad")
	}
	_, first := th.check("dad")

	now = now.Add(500 * time.Millisecond)
	if _, remaining := th.check("dad"); remaining >= first {
		t.Fatalf("remaining lockout grew from %v to %v", first, remaining)
	}
}

func TestThrottle_IsPerAccount(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	th := newThrottle(func() time.Time { return now })

	for range throttleThreshold {
		th.fail("dad")
	}
	if allowed, _ := th.check("mum"); !allowed {
		t.Fatal("locking one account locked another")
	}
}

// TestV19_FiveFailedLoginsProduceBackoff is the second half of V-19.
func TestV19_FiveFailedLoginsProduceBackoff(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")

	badLogin := func() *http.Request {
		return h.request(http.MethodPost, "/login", url.Values{
			"guardian": {"dad"}, "password": {"not the password"},
		}, nil)
	}

	for i := 1; i <= throttleThreshold; i++ {
		if rec := h.do(badLogin()); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i, rec.Code)
		}
	}

	rec := h.do(badLogin())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the attempt after five failures = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header on a throttled attempt")
	}

	// The correct password is refused too while the lockout stands — otherwise
	// the throttle is a hint about which guess was right.
	if rec := h.do(h.request(http.MethodPost, "/login", url.Values{
		"guardian": {"dad"}, "password": {testPassword},
	}, nil)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a correct password during lockout = %d, want 429", rec.Code)
	}

	// Once the window passes, the guardian gets back in.
	h.now = h.now.Add(throttleCap)
	if cookie := h.login("dad", testPassword); cookie == nil {
		t.Fatal("the guardian could not sign in after the lockout expired")
	}
}

func TestLogin_FailurePaysTheFixedDelay(t *testing.T) {
	// §9.2's ~1 s fixed delay caps the guess rate from the first attempt,
	// before any counter has tripped. The harness stubs the sleep out; this
	// test puts a recorder in its place.
	h := newHarness(t)
	h.addGuardian("dad")

	var slept []time.Duration
	h.server.sleep = func(d time.Duration) { slept = append(slept, d) }

	h.do(h.request(http.MethodPost, "/login", url.Values{
		"guardian": {"dad"}, "password": {"wrong"},
	}, nil))
	if len(slept) != 1 || slept[0] != failureDelay {
		t.Fatalf("delays on one failed login = %v, want [%v]", slept, failureDelay)
	}

	slept = nil
	h.login("dad", testPassword)
	if len(slept) != 0 {
		t.Fatalf("a successful login paid a delay: %v", slept)
	}
}
