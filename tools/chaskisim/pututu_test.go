package chaskisim

import (
	"path/filepath"
	"testing"
	"time"
)

var testPututuKey = []byte("test-pututu-key-do-not-use-in-prod")

func TestAcceptPututu_ValidStrictlyGreaterCounter_Accepted(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	token := MintPututuToken(5, testPututuKey)
	result := d.AcceptPututu(token, testPututuKey)

	if !result.Valid || !result.Accepted {
		t.Fatalf("AcceptPututu(counter=5) = %+v, want Valid and Accepted", result)
	}
	if !result.WouldWake {
		t.Errorf("first accepted token WouldWake = false, want true (no prior wake to rate-limit against)")
	}
	if got := d.State().PututuCounterSeen; got != 5 {
		t.Errorf("PututuCounterSeen = %d, want 5", got)
	}
}

func TestAcceptPututu_BadMAC_IgnoredNoStateChange(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	forged := MintPututuToken(5, []byte("wrong-key"))
	result := d.AcceptPututu(forged, testPututuKey)

	if result.Valid || result.Accepted {
		t.Fatalf("AcceptPututu with wrong key = %+v, want neither Valid nor Accepted", result)
	}
	if got := d.State().PututuCounterSeen; got != 0 {
		t.Errorf("PututuCounterSeen after forged token = %d, want unchanged (0)", got)
	}
}

func TestAcceptPututu_NonGreaterCounter_Rejected(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	first := d.AcceptPututu(MintPututuToken(10, testPututuKey), testPututuKey)
	if !first.Accepted {
		t.Fatalf("first accept = %+v, want Accepted", first)
	}

	// A replayed token, and a strictly-lower counter — §10.2's "accept only
	// if strictly greater" rules out both equal and lesser.
	replay := d.AcceptPututu(MintPututuToken(10, testPututuKey), testPututuKey)
	if replay.Accepted {
		t.Errorf("replayed equal-counter token = %+v, want not Accepted", replay)
	}
	lower := d.AcceptPututu(MintPututuToken(9, testPututuKey), testPututuKey)
	if lower.Accepted {
		t.Errorf("lower-counter token = %+v, want not Accepted", lower)
	}
	if got := d.State().PututuCounterSeen; got != 10 {
		t.Errorf("PututuCounterSeen after rejected tokens = %d, want unchanged (10)", got)
	}
}

func TestV20_CounterHeal_RestoreThenSyncThenAccept(t *testing.T) {
	// Mirrors V-20's device-side half: "restore state.json from an older
	// copy -> next SMS ignored by simulated device; after one sync, counter
	// jumps forward and the following SMS is accepted." The server-side
	// counter-jump machinery is wave 3's internal/pututu; what belongs here
	// is that this simulator's own accept/reject rule is exactly what makes
	// "ignored, then accepted after a sync" observable at all.
	d, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The device previously accepted counter 41.
	if r := d.AcceptPututu(MintPututuToken(41, testPututuKey), testPututuKey); !r.Accepted {
		t.Fatalf("priming accept(41) = %+v, want Accepted", r)
	}

	// The server was restored from an older backup and now (wrongly) sends
	// counter 21 — lower than what this device already saw.
	rolledBack := d.AcceptPututu(MintPututuToken(21, testPututuKey), testPututuKey)
	if rolledBack.Accepted {
		t.Fatalf("rolled-back counter 21 = %+v, want not Accepted (silently ignored)", rolledBack)
	}
	if got := d.State().PututuCounterSeen; got != 41 {
		t.Fatalf("PututuCounterSeen after rolled-back SMS = %d, want still 41", got)
	}

	// A sync happens (modelled here as directly reporting the device's
	// counter to the "server" and getting healed back a higher one — the
	// full wire round trip is exercised in device_test.go via SyncOnce). The
	// server, having healed past 41, now sends 42.
	healed := d.AcceptPututu(MintPututuToken(42, testPututuKey), testPututuKey)
	if !healed.Accepted {
		t.Fatalf("post-heal counter 42 = %+v, want Accepted", healed)
	}
	if got := d.State().PututuCounterSeen; got != 42 {
		t.Errorf("PututuCounterSeen after heal = %d, want 42", got)
	}
}

func TestAcceptPututu_WakeRateLimit(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	frozen := d.now() // capture the real clock once; substitute a fixed one below
	d.now = func() time.Time { return frozen }

	first := d.AcceptPututu(MintPututuToken(1, testPututuKey), testPututuKey)
	if !first.WouldWake {
		t.Fatalf("first accepted token WouldWake = false, want true")
	}

	// A second valid, strictly-greater, accepted token arriving in the same
	// instant must still not wake: §10.2's 5-minute wake rate limit applies
	// regardless of validity.
	second := d.AcceptPututu(MintPututuToken(2, testPututuKey), testPututuKey)
	if !second.Accepted {
		t.Fatalf("second token = %+v, want Accepted (counter still strictly greater)", second)
	}
	if second.WouldWake {
		t.Errorf("second token within the rate-limit window WouldWake = true, want false")
	}

	d.now = func() time.Time { return frozen.Add(PututuWakeRateLimit) }
	third := d.AcceptPututu(MintPututuToken(3, testPututuKey), testPututuKey)
	if !third.WouldWake {
		t.Errorf("token after the rate-limit window WouldWake = false, want true")
	}
}

func TestAcceptPututu_MalformedToken_Rejected(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, tok := range []string{"", "not-a-token", "CH1.notanumber.abc", "CH2.1.abc"} {
		if r := d.AcceptPututu(tok, testPututuKey); r.Valid {
			t.Errorf("AcceptPututu(%q) = %+v, want not Valid", tok, r)
		}
	}
}
