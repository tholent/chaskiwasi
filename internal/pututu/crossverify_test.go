package pututu

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/carrier"
	"github.com/tholent/chaskiwasi/internal/state"
	"github.com/tholent/chaskiwasi/tools/chaskisim"
)

// This file is the cross-verification the task brief calls out by name: mint
// tokens with this package's code and check them against tools/chaskisim's
// AcceptPututu/MintPututuToken, the device-side implementation of §10.2. If
// the two ever disagree about the MAC — a byte-order slip, a different key
// derivation, a different macBytes constant — the doorbell silently never
// rings in production and nothing else would catch it (A.8, §10.3). A unit
// test that only checks this package's own verifier would never see that
// class of bug at all.

// newDeviceKeyedTo returns a fresh chaskisim.Device primed with key, ready
// to accept doorbell tokens exactly as a real Chaski would.
func newDeviceKeyedTo(t *testing.T) *chaskisim.Device {
	t.Helper()
	d, err := chaskisim.Open(filepath.Join(t.TempDir(), "device-state.json"))
	if err != nil {
		t.Fatalf("chaskisim.Open: %v", err)
	}
	return d
}

// TestCrossVerify_MatchesChaskisimMint asserts pututu.Token and
// chaskisim.MintPututuToken produce byte-identical output for the same
// (counter, key) — the most direct check that both sides implement §10.2's
// MAC the same way.
func TestCrossVerify_MatchesChaskisimMint(t *testing.T) {
	keys := [][]byte{
		testKey,
		[]byte("another-key"),
		{}, // deliberately degenerate; HMAC accepts an empty key
	}
	counters := []uint64{0, 1, 41, 42, 1_000_000}

	for _, key := range keys {
		for _, counter := range counters {
			got := Token(counter, key)
			want := chaskisim.MintPututuToken(counter, key)
			if got != want {
				t.Errorf("Token(%d, %q) = %q, chaskisim.MintPututuToken = %q — MAC disagreement, the doorbell would silently never ring",
					counter, key, got, want)
			}
		}
	}
}

// TestCrossVerify_ServerMintedTokenIsAcceptedByRealDevice mints a token with
// this package's stateful mintToken path (via a live Doorbell, not the bare
// Token helper) and feeds it through chaskisim's real AcceptPututu. This is
// the same round trip production makes: server state -> wire token -> device
// verification.
func TestCrossVerify_ServerMintedTokenIsAcceptedByRealDevice(t *testing.T) {
	fake := carrier.NewFake()
	st := testState(t)
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)

	d.Ring(context.Background())
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("Sent() = %v, want exactly one token", sent)
	}

	device := newDeviceKeyedTo(t)
	result := device.AcceptPututu(sent[0], testKey)
	if !result.Valid || !result.Accepted {
		t.Fatalf("real device AcceptPututu(%q) = %+v, want Valid and Accepted", sent[0], result)
	}
}

// TestV20_ServerHalf_CounterHealsAfterReconciliation is the server-side half
// of V-20's full round trip: "restore state.json from an older copy -> next
// SMS ignored by simulated device; after one sync, counter jumps forward and
// the following SMS is accepted."
//
// It reproduces the rollback with a real state.Store (a state.json rolled
// back to counter=2, as a restore from an old backup would leave it), heals
// it the way internal/syncsvc's commitSync does on receiving a device's
// pututu_counter_seen (calling the same state.State.ReconcilePututuCounter
// this package's mintToken implicitly builds on), and then proves — against
// tools/chaskisim's real AcceptPututu, not a reimplementation of its rule —
// that the very next doorbell ring is accepted by a device that had already
// seen counter 41, and that it would NOT have been accepted without the
// reconciliation step.
func TestV20_ServerHalf_CounterHealsAfterReconciliation(t *testing.T) {
	st := testState(t)

	// The device previously accepted counter 41 (modelling its own history,
	// independent of the server).
	device := newDeviceKeyedTo(t)
	if r := device.AcceptPututu(chaskisim.MintPututuToken(41, testKey), testKey); !r.Accepted {
		t.Fatalf("priming device accept(41) = %+v, want Accepted", r)
	}

	// The server's state.json was restored from a backup taken before that:
	// its counter is now behind what the device already saw.
	if err := st.Update(func(s *state.State) error {
		s.PututuCounter = 2
		return nil
	}); err != nil {
		t.Fatalf("simulating rolled-back state: %v", err)
	}

	// Sanity check: without reconciliation, the very next mint (counter=3)
	// would be silently ignored by the device — this is the bug §10.3 exists
	// to heal, demonstrated against the real device code before we fix it.
	unhealedToken := Token(3, testKey)
	if r := device.AcceptPututu(unhealedToken, testKey); r.Accepted {
		t.Fatalf("device accepted counter 3 after already seeing 41 — the rollback scenario isn't set up correctly")
	}

	// A device sync happens and reports pututu_counter_seen=41; syncsvc's
	// commitSync heals the server counter forward by calling exactly this
	// state method (§10.3, wave 2's internal/syncsvc.commitSync).
	if err := st.Update(func(s *state.State) error {
		s.ReconcilePututuCounter(41)
		return nil
	}); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	// Now a new arrival rings the doorbell. It must mint from the healed
	// counter (42), not from the rolled-back one (3).
	fake := carrier.NewFake()
	clk := newClock(time.Now())
	d := newTestDoorbell(t, fake, st, clk)
	d.Ring(context.Background())
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("Sent() = %v, want exactly one token after healing", sent)
	}

	result := device.AcceptPututu(sent[0], testKey)
	if !result.Valid || !result.Accepted {
		t.Fatalf("device rejected the post-heal token %q: %+v, want Accepted — the doorbell would stay permanently silent (V-20)", sent[0], result)
	}
	if result.Counter != 42 {
		t.Fatalf("healed token carried counter %d, want 42 (41 reconciled, then NextPututuCounter)", result.Counter)
	}
}
