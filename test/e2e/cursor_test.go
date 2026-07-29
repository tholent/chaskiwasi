//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/tools/chaskisim"
)

// TestV21_UIDValidityResetIsInvisibleOnTheWire is §15's V-21: after the
// mailbox's UIDVALIDITY changes underneath a live device, the next sync behaves
// as a window resync and the device's own dedup leaves no duplicates.
//
// The condition is injected directly. maddy has no way to be asked for a
// UIDVALIDITY reset, so the fixture produces one the only way it can — by
// recreating the storage account and re-adding the identical messages. The
// Message-IDs are unchanged, so the letter ids are unchanged (§4.5), which is
// exactly the situation a real reset produces: the same letters, new UIDs.
//
// Why it matters: §4.4 says resets are invisible on the wire, and that "no
// special firmware path exists" for them. That promise has two halves in two
// different codebases — the server must treat a stale-uidvalidity cursor
// exactly as an empty one, and the device must dedup by letter id. Neither
// half's unit tests can observe the other. If the server got it wrong the
// device would silently stop receiving mail; if the device got it wrong the
// child would see every letter they own arrive again.
func TestV21_UIDValidityResetIsInvisibleOnTheWire(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	h.ui.addContact(t, "Theo", relativeAddress)

	marks := []string{nonce(t), nonce(t), nonce(t)}
	for i, mark := range marks {
		h.mail.add(t, childAddress, inboxFolder, letter{
			From:      "Theo <" + relativeAddress + ">",
			Subject:   "letter " + mark,
			MessageID: fmt.Sprintf("uidvalidity-%d@chaski.test", i),
			Date:      time.Now().Add(-time.Duration(len(marks)-i) * time.Hour),
			Body:      "one of several " + mark,
		}.bytes())
	}
	h.mail.waitForMark(t, childAddress, inboxFolder, marks[len(marks)-1], 30*time.Second)

	// The device catches up normally and persists what it has seen, because
	// the dedup ring only helps if it survives — this is flash, not RAM (§4.5).
	dev.wake(t)
	if err := dev.Save(); err != nil {
		t.Fatalf("persisting device state: %v", err)
	}
	beforeCursor := dev.State().Cursor
	beforeLetters := len(dev.State().Letters)
	if beforeLetters == 0 {
		t.Fatal("the device received nothing before the reset; there is no duplication to look for")
	}

	// The reset. Everything currently in INBOX is captured, the account is
	// recreated (which rolls UIDVALIDITY), and the same messages go back.
	preserved := h.mail.messages(t, childAddress, inboxFolder)
	h.mail.resetAccounts(t)
	for _, msg := range preserved {
		h.mail.add(t, childAddress, inboxFolder, msg.Raw)
	}

	// Fixture artifact, not part of what V-21 asserts: recreating the account
	// is the only lever maddy offers for rolling UIDVALIDITY, and it also
	// invalidates Wasi's live IMAP session in a way a real provider's
	// UIDVALIDITY reset does not — the account row is gone, so SELECT INBOX
	// answers NO rather than answering with a new UIDVALIDITY. Restarting Wasi
	// puts it back in the state a real reset would have left it in: connected,
	// with a mailbox whose UIDVALIDITY has moved.
	//
	// (Wasi does not recover from that on its own — a persistent protocol-level
	// NO leaves the cached connection open and every later sync reuses it. That
	// is a real robustness gap, reported separately; it is not this test's
	// subject and must not be smuggled into it.)
	h.restartWasi(t)

	// From here the device does the only thing it knows how to do: sync with
	// the cursor it stored. It has no idea anything happened, and there is no
	// firmware path for it to take.

	before := dev.State()
	resp := dev.sync(t)

	// §4.4: a cursor whose uidvalidity no longer matches is treated exactly as
	// "", so the recent window is re-delivered.
	if len(resp.Letters) == 0 {
		t.Fatalf("the server delivered nothing after a UIDVALIDITY reset; §4.4 says it re-delivers")
	}
	// §4.5: the device drops the repeats silently. Nothing new was rendered.
	if fresh := chaskisim.NewLetters(before, resp); len(fresh) != 0 {
		t.Errorf("the device rendered %d letter(s) again after the reset (§4.5): %+v", len(fresh), fresh)
	}
	if got := len(dev.State().Letters); got != beforeLetters {
		t.Errorf("the device holds %d letters after the reset, want the %d it had (§4.5)", got, beforeLetters)
	}
	if dev.State().Cursor == beforeCursor {
		t.Errorf("the cursor did not change across a UIDVALIDITY reset; it encodes uidvalidity (§4.4)")
	}

	// A second sync settles: nothing new, and nothing repeated.
	settled := dev.State()
	if fresh := chaskisim.NewLetters(settled, dev.sync(t)); len(fresh) != 0 {
		t.Errorf("a follow-up sync rendered %d letter(s) again (§4.5)", len(fresh))
	}
}

// TestV20_DoorbellCounterHeals is §15's V-20: after state.json is restored from
// an older copy, the doorbell is silent until one sync heals the counter, and a
// replayed or forged token is ignored either way.
//
// Why it matters: §10.3 exists because the failure it prevents is invisible.
// A restored state.json rolls the server's counter backward, every subsequent
// SMS then carries an already-seen counter, and the device correctly ignores
// all of them — a permanently silent doorbell with nothing anywhere looking
// broken. Healing it over the sync wire is what keeps "restore is `cp` back"
// (§3) a true statement instead of an operational trap.
func TestV20_DoorbellCounterHeals(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)
	key := []byte(pututuKey)

	// A first sync writes state.json. This copy is the "backup" the restore
	// below puts back — taken before any doorbell has rung.
	dev.sync(t)
	backup := h.stack.readVolumeFile(t, h.dataVolume, "state.json")

	// Ring the doorbell for real: adding a contact appends a notice letter,
	// which arrives in INBOX and is an arrival like any other (§7.4, §5.1).
	h.ui.addContact(t, "Theo", relativeAddress)
	waitFor(t, 30*time.Second, "the doorbell counter to advance", func() error {
		if h.pututuCounter(t) < 1 {
			return fmt.Errorf("counter is still 0")
		}
		return nil
	})
	rung := uint64(h.pututuCounter(t))

	// The device accepts that token, exactly as §10.2 says: verify the MAC,
	// accept only on a strictly greater counter, persist it across power loss.
	if got := dev.AcceptPututu(chaskisim.MintPututuToken(rung, key), key); !got.Accepted {
		t.Fatalf("the device rejected a valid doorbell token for counter %d: %+v", rung, got)
	}
	if err := dev.Save(); err != nil {
		t.Fatalf("persisting device state: %v", err)
	}

	// §10.2: failures are ignored silently, and there is nothing to observe
	// but the fact that nothing changed.
	if got := dev.AcceptPututu(chaskisim.MintPututuToken(rung, key), key); got.Accepted {
		t.Errorf("a replayed token was accepted; §10.2 requires strictly greater")
	}
	forged := chaskisim.MintPututuToken(rung+5, []byte("not-the-pututu-key"))
	if got := dev.AcceptPututu(forged, key); got.Valid || got.Accepted {
		t.Errorf("a forged token verified: %+v", got)
	}

	// The restore: `cp` the older state.json back, which is the whole of §3's
	// restore procedure.
	h.stack.kill(t, "wasi")
	h.stack.writeVolumeFile(t, h.dataVolume, "state.json", backup)
	h.stack.start(t, "wasi")
	h.waitReady(t)

	if got := h.pututuCounter(t); uint64(got) >= rung {
		t.Fatalf("the restore did not roll the counter back (got %v, want < %d); nothing below is being tested", got, rung)
	}

	// The silent-doorbell failure, demonstrated: the server would now mint a
	// token the device has already seen, and the device would correctly ignore
	// it forever.
	if got := dev.AcceptPututu(chaskisim.MintPututuToken(uint64(h.pututuCounter(t))+1, key), key); got.Accepted {
		t.Errorf("the device accepted a rolled-back counter; the ignore rule is what makes healing necessary")
	}

	// §10.3: one sync. The device reports the highest counter it has accepted,
	// and the server jumps past it.
	resp := dev.sync(t)
	if resp.PututuCounter < dev.State().PututuCounterSeen {
		t.Errorf("after one sync the server counter is %d, still at or below the device's %d (§10.3)",
			resp.PututuCounter, dev.State().PututuCounterSeen)
	}

	// And the doorbell works again: the next token the server would mint is
	// strictly greater than anything the device has seen.
	next := resp.PututuCounter + 1
	if got := dev.AcceptPututu(chaskisim.MintPututuToken(next, key), key); !got.Accepted {
		t.Errorf("the doorbell is still silent after healing: counter %d rejected (%+v)", next, got)
	}
}
