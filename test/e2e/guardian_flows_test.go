//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

// pututuCounter reads the doorbell counter off /data/state.json.
//
// Read from the file rather than from a sync response, because syncing is not
// a neutral observation here: §10.1 skips the SMS entirely if the device has
// synced since the triggering arrival, so a test that syncs to look at the
// counter can suppress the very ring it is about to check for.
func (h *harness) pututuCounter(t *testing.T) float64 {
	t.Helper()
	st := h.serverState(t)
	if st == nil {
		// No state.json yet: nothing has been written, so nothing has rung.
		return 0
	}
	v, ok := st["pututu_counter"].(float64)
	if !ok {
		t.Fatalf("state.json has no numeric pututu_counter: %+v", st)
	}
	return v
}

// TestV18_ReleaseAStranger is the first half of §15's V-18: a stranger's Held
// letter, released through the real UI as "add as contact, then deliver",
// arrives on the device, and the add is announced and rings the doorbell.
//
// Why it matters: §8 says nothing ever silently vanishes on release, and names
// the pre-fix behaviour it is specified against — stranger-release evaporating
// at derivation, because the message moved back to INBOX with a sender that
// still resolved to nobody. That failure is invisible from inside the release
// handler: the MOVE succeeds, the guardian sees a success message, and the
// letter is simply never delivered. Only following it all the way to the
// device catches it.
func TestV18_ReleaseAStranger(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	mark := nonce(t)
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Marta <" + strangerAddress + ">",
		Subject:   "a letter from someone new " + mark,
		MessageID: "stranger-release-" + mark + "@chaski.test",
		Body:      "I am your aunt and I found this address " + mark,
	}.bytes())
	h.waitForHeld(t, mark, 30*time.Second)

	before := h.pututuCounter(t)

	// The UI decides the flow from a fresh server-side resolution, not from
	// anything posted, so reading the offered control back is reading the
	// server's own classification (§8).
	row := h.ui.heldRowFrom(t, strangerAddress)
	if row.Kind != "stranger" {
		t.Fatalf("a sender on nobody's list was offered the %q flow, want %q (§8)", row.Kind, "stranger")
	}
	h.ui.release(t, row.UID, "Marta")

	// One action: the contact exists, the change is announced (I-4), and the
	// message left Held.
	marta := h.contactID(t, "Marta")
	for _, c := range h.contacts(t) {
		if c.ID == marta && !c.Active {
			t.Errorf("the contact created by a release is not active, so the release cannot have delivered (§8)")
		}
	}
	waitFor(t, 30*time.Second, "the released letter to leave Held", func() error {
		if n := h.mail.count(t, childAddress, heldFolder); n != 0 {
			return fmt.Errorf("Held still holds %d", n)
		}
		return nil
	})

	// §8: "a released message receives a new UID above the cursor and flows
	// through derivation like any arrival."
	resp := dev.windowResync(t)
	released := letterWithBody(t, resp, mark)
	if released.ContactID != marta {
		t.Errorf("the released letter renders as contact %q, want the newly added %q", released.ContactID, marta)
	}
	if !h.mail.holds(t, childAddress, inboxFolder, "Marta was added") {
		t.Errorf("the add was not announced (I-4, §7.4)")
	}

	// §8: "Release fires pututu — a released letter is an arriving letter from
	// the child's point of view."
	waitFor(t, 20*time.Second, "the doorbell to ring for the release", func() error {
		if after := h.pututuCounter(t); after <= before {
			return fmt.Errorf("counter is still %v", after)
		}
		return nil
	})
}

// TestV18_ReleaseADeactivatedContact is the second half of §15's V-18:
// reactivate, release, deactivate again — three deliberate, announced actions —
// delivers the one old letter and leaves the channel closed behind it.
//
// Why it matters: the other pre-fix behaviour §8 names is inactive-release
// delivering "by accident of the table split". Getting it right by accident is
// indistinguishable from getting it right, until the split changes. And the
// deactivate-again step is the documented way to deliver one letter without
// reopening the channel; if mail from that sender did not go back to Held
// afterwards, the guardian would have silently reopened it.
func TestV18_ReleaseADeactivatedContact(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	h.ui.deactivate(t, theo)

	mark, laterMark := nonce(t), nonce(t)
	// Seeded straight into Held: this is a release test (§8), and release
	// assumes the message is already there. How a deactivated contact's arrival
	// reaches Held is the IDLE path's job and is exercised in filing's unit
	// tests — the maddy fixture cannot drive it deterministically (see V-6 and
	// finding F-9). Held is read live over IMAP (§8), so a seeded message is a
	// faithful stand-in for one filing moved.
	h.mail.add(t, childAddress, heldFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "one last letter " + mark,
		MessageID: "deactivated-release-" + mark + "@chaski.test",
		Body:      "I heard I was taken off the list " + mark,
	}.bytes())

	row := h.ui.heldRowFrom(t, relativeAddress)
	if row.Kind != "deactivated" {
		t.Fatalf("a tombstone's letter was offered the %q flow, want %q (§8)", row.Kind, "deactivated")
	}
	h.ui.release(t, row.UID, "")

	if !h.mail.holds(t, childAddress, inboxFolder, "Theo was added back") {
		t.Errorf("the reactivation was not announced (I-4, §7.4)")
	}

	// The third deliberate action. §8 documents this sequence rather than
	// offering a silent one-off override.
	h.ui.deactivate(t, theo)

	resp := dev.windowResync(t)
	released := letterWithBody(t, resp, mark)
	if released.ContactID != theo {
		t.Errorf("the released letter renders as contact %q, want %q", released.ContactID, theo)
	}

	// The channel is closed again — releasing one letter did not reopen it. The
	// child-facing meaning of "closed" is that the child can no longer write to
	// Theo, and that is asserted directly and reliably: an outbound letter to
	// the re-deactivated contact is rejected at send (§4.7, §7.2). The mirror
	// property — that Theo's *new* inbound mail is re-quarantined — is the
	// deactivated-arrival path again (IDLE, unit-tested; see V-6 and F-9), so it
	// is not re-driven through the fixture here.
	dev.Compose(theo, "", "are you there "+laterMark, "o-after-redeactivate")
	ack := ackFor(t, dev.sync(t), "o-after-redeactivate")
	if ack.Status != protocol.AckRejectedInactive {
		t.Errorf("outbound to the re-deactivated contact acked %q, want %q — the channel reopened (§7.2)",
			ack.Status, protocol.AckRejectedInactive)
	}
}

// TestV19_SessionsAndThrottling is §15's V-19 against the real listener: a
// password change invalidates the session cookie a browser is already holding,
// and repeated failures buy an attacker a wait.
//
// Why it matters: §9.2 is explicit that this is the hostile-household case —
// "a lock change that leaves old keys working is not a lock change". The unit
// tests prove the epoch check; what they cannot prove is that the cookie a
// real browser holds, over a real TLS connection, stops working. Sessions are
// stateless signed cookies, so the only thing standing between an old cookie
// and the contact list is that one comparison.
func TestV19_SessionsAndThrottling(t *testing.T) {
	h := newHarness(t)

	// A second browser, signed in with the same account, standing in for the
	// session somebody else is still holding.
	other := newGuardianUI(t, h.stack)
	other.login(t, guardianName, h.guardianPass)
	other.page(t, "/contacts") // it works right now

	const newPassword = "a-different-guardian-password"
	h.ui.changePassword(t, h.guardianPass, newPassword)
	h.guardianPass = newPassword

	// §9.2: the per-guardian session_epoch is bumped, so cookies carrying the
	// old one are rejected. The UI answers by redirecting to the sign-in page
	// rather than by serving the contact list.
	body := other.page(t, "/contacts")
	if !strings.Contains(body, `name="password"`) {
		t.Errorf("a session issued before the password change still reaches the contact list (§9.2)")
	}

	// §9.2: five consecutive failures start an exponential backoff, and the
	// refusal is answered before the password is looked at.
	const attempts = 5
	for i := range attempts {
		if _, status := h.ui.loginAttempt(t, guardianName, "definitely-not-the-password"); status != http.StatusUnauthorized {
			t.Fatalf("failed sign-in %d answered %d, want 401", i+1, status)
		}
	}
	body, status := h.ui.loginAttempt(t, guardianName, h.guardianPass)
	if status != http.StatusTooManyRequests {
		t.Errorf("after %d failures the correct password was answered %d, want 429 (§9.2)", attempts, status)
	}
	if !strings.Contains(body, "Too many failed sign-ins") {
		t.Errorf("the backoff response does not say what happened:\n%s", truncate(body, 400))
	}
}

// TestV14_VocabularyBoundaryOnRenderedSurfaces is §15's V-14, over the
// surfaces a unit test cannot reach: the HTML the live server actually serves,
// and the notice letters actually sitting in the mailbox.
//
// The unit test greps template source, which is the right place to catch a
// word typed into a template. This catches the other half: a word that reaches
// a person through a value rather than through a template — a flash message, a
// contact name, an error string, a generated subject.
//
// Why it matters: §0.1 makes these three words internal on purpose. A guardian
// who meets "ayllu" in an error message has been handed a word that explains
// nothing and belongs to the implementation.
func TestV14_VocabularyBoundaryOnRenderedSurfaces(t *testing.T) {
	h := newHarness(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	h.ui.deactivate(t, theo)
	mark := nonce(t)
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "A Stranger <" + strangerAddress + ">",
		Subject:   "hello " + mark,
		MessageID: "vocab-" + mark + "@chaski.test",
		Body:      "so the Held page has a row to render",
	}.bytes())
	h.waitForHeld(t, mark, 30*time.Second)

	for _, path := range pageURLs {
		html := h.ui.page(t, path)
		for _, word := range internalVocabulary {
			if strings.Contains(strings.ToLower(html), word) {
				t.Errorf("%s renders the internal word %q (§0.1, §9.1)", path, word)
			}
		}
	}

	// A rejected write is a rendered surface too, and error strings are where
	// an internal identifier is most likely to escape.
	body, _ := h.ui.postRaw(t, "/contacts/add", url.Values{
		"csrf_token": {"not-a-valid-token"},
		"name":       {"Nobody"},
		"address":    {"nobody@chaski.test"},
	})
	for _, word := range internalVocabulary {
		if strings.Contains(strings.ToLower(body), word) {
			t.Errorf("a rejected write answers with the internal word %q (§0.1)", word)
		}
	}

	// Outgoing mail: the other rendering path §9.1 names.
	for _, msg := range h.mail.messages(t, childAddress, inboxFolder) {
		text := strings.ToLower(string(msg.Raw))
		for _, word := range internalVocabulary {
			if strings.Contains(text, word) {
				t.Errorf("a generated letter contains the internal word %q (§0.1, §7.4)", word)
			}
		}
	}
}
