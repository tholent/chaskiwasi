//go:build e2e

package e2e

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"
)

// maxLetterChars mirrors sync.max_letter_chars in deploy/wasi.example.toml.
// §4.6 counts it in graphemes, never bytes.
const maxLetterChars = 500

// TestV4_InboundDerivation is §15's V-4 against the real strip service and a
// real mailbox: a reply with a quoted tail and a prefix-encrusted, RFC 2047
// encoded subject comes out stripped, flagged, decoded and collapsed; an
// over-long letter comes out truncated with the mailbox copy untouched.
//
// Why it matters end to end: derivation's unit tests use a strip fake, so they
// prove Wasi handles a stripped body, not that talon actually strips this
// body. And "the mailbox copy is intact" (§5.2, A.4) is a claim about a
// different system entirely — the archive the child graduates with — which
// only a real IMAP store can answer.
func TestV4_InboundDerivation(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	h.ui.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	quotedMark, longMark := nonce(t), nonce(t)

	// §5.4: RFC 2047 decode, strip repeated Re:/Fwd:/Fw: prefixes, collapse
	// whitespace. The encoded word below decodes to "café ☕ camping <mark>".
	decodedSubject := "café ☕ camping " + quotedMark
	encoded := "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(decodedSubject)) + "?="
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "Re: Re: Fwd: " + encoded,
		MessageID: "quoted-" + quotedMark + "@chaski.test",
		Date:      time.Now().Add(-time.Hour),
		Body: "That sounds like so much fun " + quotedMark + "\n" +
			"\n" +
			"On Mon, Jan 5, 2026 at 2:00 PM, Maya <" + childAddress + "> wrote:\n" +
			"> We are going camping this weekend near the lake.\n" +
			">\n" +
			"> Cannot wait!\n",
	}.bytes())

	// §5.2 step 5: truncate at max_letter_chars graphemes, set `truncated`.
	// The full text stays in the mailbox and graduates intact (A.4).
	longBody := longMark + " " + strings.Repeat("a", 5000)
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "a very long letter " + longMark,
		MessageID: "long-" + longMark + "@chaski.test",
		Date:      time.Now().Add(-30 * time.Minute),
		Body:      longBody,
	}.bytes())

	resp := dev.windowResync(t)

	quoted := letterWithBody(t, resp, quotedMark)
	if !quoted.Trimmed {
		t.Errorf("quoted reply delivered with trimmed=false; strip did not remove the tail (§5.2)")
	}
	if strings.Contains(quoted.Body, "Cannot wait") || strings.Contains(quoted.Body, ">") {
		t.Errorf("the quoted tail survived into the device body (§5.2):\n%q", quoted.Body)
	}
	if quoted.Degraded {
		t.Errorf("strip is up, so the letter should not be flagged degraded (§5.3)")
	}
	// §5.4: prefixes stripped case-insensitively and repeatedly, encoded word
	// decoded, whitespace collapsed. Inbound subjects are real and are never
	// generated, so the decoded original is exactly what should arrive.
	if quoted.Subject != decodedSubject {
		t.Errorf("subject delivered as %q, want %q (§5.4)", quoted.Subject, decodedSubject)
	}

	long := letterWithBody(t, resp, longMark)
	if !long.Truncated {
		t.Errorf("5000-grapheme letter delivered with truncated=false (§5.2)")
	}
	if n := uniseg.GraphemeClusterCount(long.Body); n > maxLetterChars {
		t.Errorf("delivered body is %d graphemes, over the %d cap (§4.6)", n, maxLetterChars)
	}
	if long.Trimmed {
		t.Errorf("length-capping set trimmed; §4.3 keeps the two flags distinct")
	}

	// A.4: truncation shapes the device's view only. The archive keeps the
	// whole letter, which is the entire reason a truncation flag is acceptable
	// instead of a reply to the sender.
	for _, msg := range h.mail.messages(t, childAddress, inboxFolder) {
		if !strings.Contains(msg.Body, longMark) {
			continue
		}
		if !strings.Contains(msg.Body, longBody) {
			t.Errorf("the mailbox copy of the long letter was modified; §5.2 says the full text stays untouched")
		}
	}
}

// TestV5_StrangerIsHeldSilently is §15's V-5: mail from a sender who is on
// nobody's list is quarantined, nothing is sent back, and the device never
// learns it existed.
//
// Why it matters: this is the allowlist, and all three halves have to hold at
// once. A bounce would tell a stranger the address is live and would come from
// the child's own address (I-3, A.4); a Held count or hint on the wire would
// turn the device into a surveillance indicator (§1.1); and a letter that
// slipped through would defeat design-spec §3.1 outright. The "no bounce" half
// is only checkable against a real MTA, which is why the fixture gives the
// stranger a real mailbox for one to land in.
func TestV5_StrangerIsHeldSilently(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	// One real contact, so the test can tell "nothing was delivered" apart
	// from "delivery is broken".
	h.ui.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	strangerMark, friendMark := nonce(t), nonce(t)
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "A Stranger <" + strangerAddress + ">",
		Subject:   "hello there " + strangerMark,
		MessageID: "stranger-" + strangerMark + "@chaski.test",
		Body:      "I found this address somewhere " + strangerMark,
	}.bytes())
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "from your uncle " + friendMark,
		MessageID: "friend-" + friendMark + "@chaski.test",
		Body:      "the garden is doing well " + friendMark,
	}.bytes())

	// §5.1: the stranger goes to Held, and only the stranger.
	h.waitForHeld(t, strangerMark, 30*time.Second)
	if n := h.mail.count(t, childAddress, heldFolder); n != 1 {
		t.Fatalf("%d messages were quarantined, want only the stranger's", n)
	}

	resp := dev.windowResync(t)

	// The friend's letter arrives; the stranger's does not, and nothing on the
	// wire hints that it exists (§1.1: no Held count, flag, or hint).
	letterWithBody(t, resp, friendMark)
	for _, l := range resp.Letters {
		if strings.Contains(l.Body, strangerMark) || strings.Contains(l.Subject, strangerMark) {
			t.Errorf("the stranger's letter reached the device (§5.1, design-spec §3.1)")
		}
	}
	if strings.Contains(string(mustJSON(t, resp)), strangerAddress) {
		t.Errorf("the sync response mentions the stranger's address (I-2)")
	}

	// I-3, design-spec §3.3: no bounce, ever. Nothing may have been sent back.
	if n := h.mail.count(t, strangerAddress, inboxFolder); n != 0 {
		t.Errorf("%d message(s) were sent back to the stranger; I-3 permits none", n)
	}
}

// TestV15_ReconciliationDoesNotDependOnUptime is §15's V-15: a stranger's mail
// that arrives while Wasi is down is in Held before the device can be told
// about it, and the mail that arrived alongside it is not skipped.
//
// Why it matters: derivation walks UIDs above the cursor and never re-examines
// what it has passed. If a stranger's message could sit in INBOX until the
// first sync after a restart, the cursor would move past it and it would be
// invisible to both the guardian review and the child — vanished, not
// quarantined. §5.1 puts a reconciliation pass at startup precisely so that
// filing does not depend on the process having been alive.
func TestV15_ReconciliationDoesNotDependOnUptime(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	h.ui.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	// Down it goes, ungracefully. Everything below arrives with nobody home.
	h.stack.kill(t, "wasi")

	strangerMark, friendMark := nonce(t), nonce(t)
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "A Stranger <" + strangerAddress + ">",
		Subject:   "while you were out " + strangerMark,
		MessageID: "offline-stranger-" + strangerMark + "@chaski.test",
		Body:      "nobody was watching " + strangerMark,
	}.bytes())
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "also while you were out " + friendMark,
		MessageID: "offline-friend-" + friendMark + "@chaski.test",
		Body:      "the tomatoes came in " + friendMark,
	}.bytes())

	h.stack.start(t, "wasi")
	h.waitReady(t)

	// The assertion that matters: quarantine happens on the startup pass,
	// before any sync. waitReady deliberately does not sync (see its comment),
	// so nothing in this test has moved the cursor yet.
	h.mail.waitForCount(t, childAddress, heldFolder, 1, 60*time.Second)

	resp := dev.windowResync(t)
	letterWithBody(t, resp, friendMark) // nothing skipped
	for _, l := range resp.Letters {
		if strings.Contains(l.Body, strangerMark) {
			t.Errorf("the stranger's letter was delivered after all (§5.1)")
		}
	}
}

// TestV16_SpamBackstop is §15's V-16.
//
// The condition is injected directly, as §15 and deploy/README.md both say it
// must be: maddy never classifies anything as spam, so there is nothing for
// the backstop to catch unless a test puts it there. The real-world control is
// §5.1's setup step — disabling the provider's filter — which no fixture can
// stand in for.
//
// Why it matters: provider-side spam filtering is the one path by which family
// mail can disappear with nobody, device or guardian, ever learning it
// existed. The mailbox is allowlist-only by construction, so a spam filter can
// only ever cost it good mail.
func TestV16_SpamBackstop(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	h.ui.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	spamLetter := func(mark string) []byte {
		return letter{
			From:      "Theo <" + relativeAddress + ">",
			Subject:   "filed as spam by the provider " + mark,
			MessageID: "spam-" + mark + "@chaski.test",
			Body:      "a real letter the filter took " + mark,
		}.bytes()
	}

	// §5.1: "Wasi checks the provider's Spam/Junk folder at startup, at each
	// sync, and at least every 15 minutes." The first two are asserted
	// separately because they are the two a person can cause: a child syncing,
	// or an operator restarting. The third — the 15-minute ticker — is left to
	// internal/filing's clock-injecting tests, since an honest e2e version
	// would have to sit through the interval and a shortened one would be
	// testing a number this deployment does not use.
	t.Run("AtEachSync", func(t *testing.T) {
		mark := nonce(t)
		h.mail.add(t, childAddress, spamFolder, spamLetter(mark))
		dev.sync(t)
		h.mail.waitForMark(t, childAddress, heldFolder, mark, 20*time.Second)
	})

	t.Run("AtStartup", func(t *testing.T) {
		mark := nonce(t)
		h.mail.add(t, childAddress, spamFolder, spamLetter(mark))
		h.restartWasi(t)
		h.mail.waitForMark(t, childAddress, heldFolder, mark, 60*time.Second)
		if n := h.mail.count(t, childAddress, spamFolder); n != 0 {
			t.Errorf("%d message(s) still sit in %s after the backstop ran", n, spamFolder)
		}
	})
}
