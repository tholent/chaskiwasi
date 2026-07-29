//go:build e2e

package e2e

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// anyAddress matches anything shaped like an email address. I-2 and §7.4 do
// not say "no *contact* address" — they say notice letters name the person and
// never the address — so the assertion has to be that no address at all
// appears, not that one particular string is missing.
var anyAddress = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]+`)

// internalVocabulary is design-spec §0.1's greppable identifiers. They are
// legal in Go, in internal logs and in this file; they are illegal in anything
// a person reads (§0, V-14).
var internalVocabulary = []string{"pututu", "ayllu", "kipu"}

// TestV7_EveryContactChangeIsAnnounced is §15's V-7: add, remove and re-point
// a contact, and find three notice letters in INBOX from c_sys — which cannot
// be written to, and whose text carries neither an address nor a word from the
// internal vocabulary.
//
// Why it matters: I-4 is the promise that neither party can change the contact
// list behind the other's back, and a notice letter is the entire mechanism.
// Re-pointing is announced with the same weight as adding and removing because
// quietly aiming an existing name at a new address is exactly how this system
// would be turned into a redirection attack (§7.4) — the change with the most
// reason to be silent is the one that must not be.
func TestV7_EveryContactChangeIsAnnounced(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	h.ui.deactivate(t, theo)
	h.ui.readdress(t, theo, "theo.new@chaski.test")

	notices := h.mail.waitForCount(t, childAddress, inboxFolder, 3, 30*time.Second)

	for _, msg := range notices {
		if msg.From != ayllu.SystemAddress {
			t.Errorf("notice %q came from %q, want the reserved system contact %q (§7.4)",
				msg.Subject, msg.From, ayllu.SystemAddress)
		}
		// F-6: the sender a child reads is "Wasi", pinned rather than left to
		// judgement because notice letters graduate — an archive whose sender
		// name changes halfway through reads as two correspondents.
		if !strings.Contains(msg.Header.Get("From"), ayllu.SystemName) {
			t.Errorf("notice %q has From %q, want the display name %q",
				msg.Subject, msg.Header.Get("From"), ayllu.SystemName)
		}

		text := msg.Subject + "\n" + msg.Body
		if found := anyAddress.FindString(text); found != "" {
			t.Errorf("notice letter contains an email address %q (I-2, §7.4):\n%s", found, text)
		}
		for _, word := range internalVocabulary {
			if strings.Contains(strings.ToLower(text), word) {
				t.Errorf("notice letter contains the internal word %q (§0.1, V-14):\n%s", word, text)
			}
		}
	}

	// The same three letters, seen from the device. §7.4 is about what the
	// child reads, and the mailbox copy and the wire copy are produced by
	// different code paths — the wire one goes through derivation.
	resp := dev.windowResync(t)
	if len(resp.Letters) != 3 {
		t.Fatalf("device received %d letters, want the 3 notices (§7.4)", len(resp.Letters))
	}
	for _, l := range resp.Letters {
		if l.ContactID != protocol.SysContactID {
			t.Errorf("notice delivered with contact_id %q, want %q (§7.4)", l.ContactID, protocol.SysContactID)
		}
		text := l.Subject + "\n" + l.Body
		if found := anyAddress.FindString(text); found != "" {
			t.Errorf("notice on the wire contains an email address %q (I-2):\n%s", found, text)
		}
	}

	// §7.4: c_sys "always resolves, cannot be deactivated, and cannot be
	// written to". The third clause is the one with a wire consequence.
	dev.sync(t)
	dev.Compose(protocol.SysContactID, "hello", "can I write to you", "o-to-sys")
	ack := ackFor(t, dev.sync(t), "o-to-sys")
	if ack.Status == protocol.AckSent {
		t.Errorf("a letter addressed to %s was sent; §7.4 says it cannot be written to", protocol.SysContactID)
	}
	if ack.Status != protocol.AckRejectedUnknownContact {
		t.Errorf("letter to %s acked %q, want %q", protocol.SysContactID, ack.Status, protocol.AckRejectedUnknownContact)
	}
	if n := h.mail.count(t, relativeAddress, inboxFolder); n != 0 {
		t.Errorf("%d message(s) reached the relative from a c_sys-addressed letter", n)
	}
}

// TestV6_DeactivationSplitsFilingFromRendering is §15's V-6: two letters from
// a contact, the contact removed, a third letter, then a full window resync —
// the first two still render with the person's name, the third is in Held,
// outbound to them is rejected, the tombstone arrives with active: false, and
// re-adding the same address reuses the same id.
//
// Why it matters: §7.1 calls this out as the latent bug the whole section
// exists to prevent. Default-deny resolution plus read-time derivation means
// deleting a contact row would make their history stop resolving on the next
// resync — silently, exactly like spam. The resolution split (active-only at
// arrival, full table at read time) is what buys "you can still read Rosa's
// old letters, you just can't write to her", and only a resync after a real
// deactivation can show that it holds.
func TestV6_DeactivationSplitsFilingFromRendering(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	firstMark, secondMark, thirdMark := nonce(t), nonce(t), nonce(t)
	for _, mark := range []string{firstMark, secondMark} {
		h.mail.add(t, childAddress, inboxFolder, letter{
			From:      "Theo <" + relativeAddress + ">",
			Subject:   "before " + mark,
			MessageID: "before-" + mark + "@chaski.test",
			Body:      "written while still on the list " + mark,
		}.bytes())
	}
	h.mail.waitForMark(t, childAddress, inboxFolder, secondMark, 30*time.Second)

	h.ui.deactivate(t, theo)

	// The post-deactivation letter is seeded straight into Held rather than
	// injected into INBOX and waited on. That a deactivated contact's *arrival*
	// is quarantined to Held is the IDLE path's job (§5.1, §7.2), and the maddy
	// fixture cannot drive it reliably: `imap-msgs add` writes to the store
	// without the notification a real delivery fires, so IDLE sees it only on an
	// unpredictable reconnect. That decision is covered where it can be exercised
	// deterministically — filing's unit tests, against a fake mailbox. What V-6
	// is about, and what this asserts reliably, is the *device-observable* split:
	// history in INBOX renders with the tombstone's name, and a letter in Held is
	// never delivered to the device. (Reconciliation cannot substitute for IDLE
	// here — it would sweep the pre-deactivation history too; finding F-2/F-9.)
	h.mail.add(t, childAddress, heldFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "after " + thirdMark,
		MessageID: "after-" + thirdMark + "@chaski.test",
		Body:      "written after the removal " + thirdMark,
	}.bytes())

	resp := dev.windowResync(t)

	// Read time resolves against the full table, tombstones included (§7.2).
	for _, mark := range []string{firstMark, secondMark} {
		l := letterWithBody(t, resp, mark)
		if l.ContactID != theo {
			t.Errorf("letter %s renders as contact %q, want %q — history stopped resolving (§7.1, §7.2)",
				mark, l.ContactID, theo)
		}
	}
	for _, l := range resp.Letters {
		if strings.Contains(l.Body, thirdMark) {
			t.Errorf("a letter in Held reached the device; only INBOX is derived (§5.2)")
		}
	}

	// The tombstone ships to the device so it can name the sender of an old
	// letter while hiding the person from the compose picker (§7.2).
	if resp.Ayllu == nil {
		t.Fatal("the ayllu version changed but no contact block was sent (§4.3)")
	}
	var tombstone *protocol.AylluContact
	for i := range resp.Ayllu.Contacts {
		if resp.Ayllu.Contacts[i].ID == theo {
			tombstone = &resp.Ayllu.Contacts[i]
		}
	}
	if tombstone == nil {
		t.Fatal("the removed contact vanished from the device's list; I-5 says removal never deletes")
	}
	if tombstone.Active {
		t.Errorf("removed contact arrived with active: true")
	}
	if tombstone.Name != "Theo" {
		t.Errorf("tombstone name is %q, want %q — an old letter would render with no sender", tombstone.Name, "Theo")
	}

	// Sending resolves active-only (§7.2).
	dev.sync(t)
	dev.Compose(theo, "still there?", "hello", "o-to-tombstone")
	if got := ackFor(t, dev.sync(t), "o-to-tombstone").Status; got != protocol.AckRejectedInactive {
		t.Errorf("outbound to a tombstone acked %q, want %q (§7.2)", got, protocol.AckRejectedInactive)
	}

	// §7.2: re-adding an address reuses the original contact id, so a person
	// who leaves and returns stays one person in the archive.
	h.ui.addContact(t, "Theo", relativeAddress)
	if got := h.contactID(t, "Theo"); got != theo {
		t.Errorf("re-adding %s minted a new contact id %q, want the original %q (§7.2)",
			relativeAddress, got, theo)
	}
	if n := len(h.contacts(t)); n != 1 {
		t.Errorf("the contact list holds %d rows after a remove and re-add, want 1 person", n)
	}
}

// TestV17_NoticeSurvivesACrash is §15's V-17: a contact change whose notice
// was interrupted by a crash is announced exactly once on restart — not zero
// times, and not again on every subsequent restart.
//
// Why it matters: §7.6's ordering buys "the notice arrives a little late"
// instead of "the change happened silently", but only if recovery actually
// runs, and F-5 showed that `pending_notices` alone cannot close the window —
// a crash between the ayllu write and the pending_notices write loses the
// notice entirely. The durable record is the change log, and startup
// reconciliation against it is what makes I-4 true. The "exactly once" half is
// equally load-bearing: without it, every restart would re-announce old news
// into the child's inbox.
//
// The crash is real and untimed. There is no hook to stop the process between
// two writes from outside it, so the test kills the container at a series of
// short delays after the mutation is requested — some land in the window, some
// do not — and asserts the invariant that must hold either way.
func TestV17_NoticeSurvivesACrash(t *testing.T) {
	h := newHarness(t)

	// A spread of delays rather than one, because the window §7.6 describes is
	// a few milliseconds wide and nothing outside the process can aim at it.
	// Some of these land before the mutation is durable, some after the notice
	// is appended, some in between; the assertion below holds for all three,
	// which is the property worth having.
	delays := []time.Duration{5, 15, 30, 50, 80, 120}
	names := make([]string, 0, len(delays))

	for _, delay := range delays {
		name := "Crash" + nonce(t)[:8]
		names = append(names, name)

		token := h.ui.csrfToken(t, "/contacts")
		posted := make(chan error, 1)
		go func() {
			posted <- h.ui.tryPost("/contacts/add", url.Values{
				"csrf_token": {token},
				"name":       {name},
				"address":    {strings.ToLower(name) + "@chaski.test"},
			})
		}()

		time.Sleep(delay * time.Millisecond)
		h.stack.kill(t, "wasi")
		<-posted // whatever it returned, the process is gone now

		h.stack.start(t, "wasi")
		h.waitReady(t)
		// The old session survives a restart only if the cookie key does, and
		// it does (it comes from secrets, not from process memory) — but the
		// client is rebuilt anyway so a torn connection cannot leak into the
		// next iteration.
		h.ui = newGuardianUI(t, h.stack)
		h.ui.login(t, guardianName, h.guardianPass)
	}

	// A second restart with nothing new happening: whatever recovery does, it
	// must be idempotent (F-5's companion requirement).
	h.restartWasi(t)

	contacts := h.contacts(t)
	onList := make(map[string]bool, len(contacts))
	for _, c := range contacts {
		onList[c.Name] = true
	}

	notices := h.mail.messages(t, childAddress, inboxFolder)
	announced := make(map[string]int)
	for _, msg := range notices {
		for _, name := range names {
			if strings.Contains(msg.Body, name) {
				announced[name]++
			}
		}
	}

	if len(onList) == 0 {
		t.Fatal("every crash landed before any mutation was durable; this run exercised nothing")
	}

	for _, name := range names {
		// I-4: if the change stuck, it was announced. If it did not stick,
		// nothing may claim it did — a notice for a change that did not happen
		// is the same failure seen from the other side (§7.6).
		switch {
		case onList[name] && announced[name] == 0:
			t.Errorf("%s is on the contact list but was never announced (I-4, §7.6, F-5)", name)
		case onList[name] && announced[name] > 1:
			t.Errorf("%s was announced %d times; recovery is not idempotent (F-5)", name, announced[name])
		case !onList[name] && announced[name] > 0:
			t.Errorf("%s was announced but is not on the list — a notice for a change that did not stick (§7.6)", name)
		}
	}
}
