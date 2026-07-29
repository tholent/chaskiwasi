//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

// messageIDShape is the minimum a Message-ID has to look like to be one:
// angle-bracketed, with an at-sign and a domain. §6.1 makes it the only header
// outbound carries beyond the envelope, so it being well-formed is the whole
// of the letter's identity in the relative's archive.
var messageIDShape = regexp.MustCompile(`^<[^<>@\s]+@[^<>@\s]+\.[^<>@\s]+>$`)

// TestV1_ComposeLandsInTheMailbox is §15's V-1: a letter composed on the
// device reaches the right mailbox with a valid Message-ID, once with a
// child-typed subject used verbatim and once with a subject the server
// generated (§6.2).
//
// Why it matters: this is the entire outbound product. Every other clause
// about outbound — sanitisation, idempotency, threading — assumes the letter
// arrives at all, and it is the one thing no unit test can establish, because
// "arrives" means a real SMTP submission accepted by a real server and filed
// into a real mailbox that is not the sender's own.
func TestV1_ComposeLandsInTheMailbox(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	dev.sync(t) // pick up the ayllu block so the compose picker has an id

	typedMark, generatedMark := nonce(t), nonce(t)
	typedSubject := "camping " + typedMark
	generatedBody := "sunset over the " + generatedMark

	dev.Compose(theo, typedSubject, "we walked to the lake "+typedMark, "o-typed")
	dev.Compose(theo, "", generatedBody, "o-generated")

	resp := dev.sync(t)
	for _, localID := range []string{"o-typed", "o-generated"} {
		if got := ackFor(t, resp, localID).Status; got != protocol.AckSent {
			t.Fatalf("%s: ack %q, want %q", localID, got, protocol.AckSent)
		}
	}

	delivered := h.mail.waitForCount(t, relativeAddress, inboxFolder, 2, 30*time.Second)

	byMark := map[string]stored{}
	for _, msg := range delivered {
		if msg.To() != relativeAddress {
			t.Errorf("letter %q addressed to %q, want %q", msg.Subject, msg.To(), relativeAddress)
		}
		if msg.From != childAddress {
			t.Errorf("letter %q sent from %q, want the child's address %q", msg.Subject, msg.From, childAddress)
		}
		if id := msg.Header.Get("Message-ID"); !messageIDShape.MatchString(id) {
			t.Errorf("letter %q carries Message-ID %q, which is not a valid one (§6.1)", msg.Subject, id)
		}
		switch {
		case strings.Contains(msg.Subject, typedMark):
			byMark["typed"] = msg
		case strings.Contains(msg.Subject, generatedMark):
			byMark["generated"] = msg
		}
	}

	// §6.2: a non-empty subject from the device is used verbatim after
	// sanitisation. "Verbatim" is load-bearing — it is the difference between
	// a subject the child wrote and one the server rewrote on their behalf.
	typed, ok := byMark["typed"]
	if !ok {
		t.Fatalf("no delivered letter carries the typed subject %q", typedSubject)
	}
	if typed.Subject != typedSubject {
		t.Errorf("typed subject delivered as %q, want %q verbatim (§6.2)", typed.Subject, typedSubject)
	}

	// §6.2: an absent subject is generated from the first words of the body,
	// capped at ~40 graphemes — not left empty, because an archive of
	// subjectless mail is the outcome that clause exists to prevent.
	generated, ok := byMark["generated"]
	if !ok {
		t.Fatalf("no delivered letter carries a subject generated from %q", generatedBody)
	}
	if generated.Subject == "" {
		t.Error("letter composed with no subject was delivered with an empty one (§6.2)")
	}
	if !strings.HasPrefix(generatedBody, generated.Subject) {
		t.Errorf("generated subject %q is not drawn from the body %q (§6.2)", generated.Subject, generatedBody)
	}

	if typed.Header.Get("Message-ID") == generated.Header.Get("Message-ID") {
		t.Errorf("two letters share one Message-ID %q", typed.Header.Get("Message-ID"))
	}
}

// TestV8_ServerAddsNoThreading is §15's V-8, written the way finding F-4
// requires.
//
// V-8 as literally worded — "outbound carries no `Re:`" — is untestable by
// substring, because §6.2 says a child-typed subject is used *verbatim*, and a
// child who types "Re: camping" has broken no rule. F-4's binding reading is
// what this asserts instead: **the server never adds** threading headers or a
// `Re:` prefix. A literal grep would fail on the legitimate letter below and
// would have to be "fixed" by weakening §6.2, which is the reversal A.1 is
// guarded against in the first place.
//
// Why it matters: In-Reply-To and References are trivial to add and unfixable
// later — once a relative's client has threaded a conversation, the archive
// has a shape the device never had. A.1 chose passing notes over conducting
// email, and this is the test that fails loudly if anyone "fixes" it.
func TestV8_ServerAddsNoThreading(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	mark := nonce(t)
	// The legitimate "Re:" — typed by the child, not added by the server. A
	// naive V-8 dies here, which is the point of writing it this way.
	childTyped := "Re: camping " + mark
	dev.Compose(theo, childTyped, "yes lets go "+mark, "o-child-re")
	dev.Compose(theo, "swimming "+mark, "the lake was warm "+mark, "o-plain")
	dev.Compose(theo, "", "no subject at all "+mark, "o-none")
	dev.sync(t)

	delivered := h.mail.waitForCount(t, relativeAddress, inboxFolder, 3, 30*time.Second)

	for _, msg := range delivered {
		// §1.1, §6.1, A.1: threading headers are absent from outbound, full
		// stop. There is no case in which the server emits one.
		for _, header := range []string{"In-Reply-To", "References", "Thread-Index", "Thread-Topic"} {
			if v := msg.Header.Get(header); v != "" {
				t.Errorf("letter %q carries %s: %q — A.1 says outbound carries Message-ID and nothing else",
					msg.Subject, header, v)
			}
		}
		if msg.Header.Get("Message-ID") == "" {
			t.Errorf("letter %q carries no Message-ID at all (§6.1)", msg.Subject)
		}
	}

	// The three subjects, checked for what the *server* did to each.
	for _, msg := range delivered {
		switch {
		case msg.Subject == childTyped:
			// Correct: passed through untouched, "Re:" and all.
		case strings.HasPrefix(msg.Subject, "swimming "):
			// Correct: no prefix invented for a plain subject.
		case strings.HasPrefix(msg.Subject, "no subject at all"):
			// Correct: generated from the body, with no prefix invented.
		default:
			t.Errorf("subject %q is not any of the three composed, so the server rewrote one", msg.Subject)
		}
		if msg.Subject != childTyped && replyPrefix.MatchString(msg.Subject) {
			t.Errorf("server added a reply prefix to %q (A.1, §6.2)", msg.Subject)
		}
	}
}

// replyPrefix matches the prefixes §5.4 strips on the way in and A.1 forbids
// adding on the way out, in the localised variants internal/subject knows.
var replyPrefix = regexp.MustCompile(`(?i)^\s*(re|fwd|fw|aw|sv|vs|res|antw|tr|wg)\s*:`)

// TestV3_SubjectHeaderInjection is §15's V-3 at the SMTP boundary.
//
// The sanitiser has unit tests; this is the half they cannot reach. §6.2 calls
// the subject "the one place child-supplied text enters an email header", and
// what makes that dangerous is not the string — it is what a real MTA does
// with a real message built from it. A newline that survives into the wire
// format becomes a header, and only a real submission to a real server can
// show whether it did.
func TestV3_SubjectHeaderInjection(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	mark := nonce(t)
	dev.Compose(theo, "hello "+mark+"\r\nBcc: attacker@evil.test", "injection attempt "+mark, "o-inject")
	dev.Compose(theo, "café ☕ "+mark, "non-ascii subject "+mark, "o-unicode")
	dev.Compose(theo, strings.Repeat("é", 500)+mark, "long subject "+mark, "o-long")
	dev.sync(t)

	delivered := h.mail.waitForCount(t, relativeAddress, inboxFolder, 3, 30*time.Second)

	for _, msg := range delivered {
		// The attack is a *header*, not a string. §6.2 strips CR and LF and
		// leaves the rest of the text alone, so "Bcc: attacker@evil.test"
		// legitimately survives as part of a flattened subject line — asserting
		// the substring is absent would be asserting the wrong thing and would
		// only pass if the sanitiser also silently ate the child's words.
		//
		// What must be absent is a field: no header named Bcc or Cc, and no
		// header line in the delivered message that the server did not write.
		if v := msg.Header.Get("Bcc"); v != "" {
			t.Errorf("subject injection produced a Bcc header: %q (§6.2)", v)
		}
		for _, name := range headerNames(string(msg.Raw)) {
			if strings.EqualFold(name, "bcc") || strings.EqualFold(name, "cc") {
				t.Errorf("subject injection produced a %s header field (§6.2)", name)
			}
		}

		// A subject is one header field. Sanitisation strips CR, LF and every
		// other control character unconditionally, so no delivered subject may
		// contain one after decoding.
		if strings.ContainsAny(msg.Subject, "\r\n") {
			t.Errorf("delivered subject %q still contains a line break (§6.2)", msg.Subject)
		}

		// §6.2: hard cap at 100 graphemes, enforced server-side. Counted in
		// graphemes because that is what the panel renders and what §0 says
		// every cap in this system means.
		if n := uniseg.GraphemeClusterCount(msg.Subject); n > protocol.MaxSubjectGraphemes {
			t.Errorf("delivered subject is %d graphemes, over the %d cap (§4.6, §6.2): %q",
				n, protocol.MaxSubjectGraphemes, msg.Subject)
		}
	}

	// §6.2: non-ASCII is RFC 2047 encoded so accented names and emoji survive.
	// Asserted on the raw header, since the decoded value cannot tell the
	// difference between an encoded word and a raw 8-bit header field — and
	// the raw 8-bit one is the one that breaks in somebody's mail client.
	var unicode *stored
	for i := range delivered {
		if strings.Contains(delivered[i].Subject, "café") {
			unicode = &delivered[i]
		}
	}
	if unicode == nil {
		t.Fatal("the non-ASCII subject was not delivered at all")
	}
	rawSubject := rawHeaderLine(string(unicode.Raw), "Subject")
	if !strings.Contains(rawSubject, "=?") {
		t.Errorf("non-ASCII subject was not RFC 2047 encoded on the wire (§6.2): %q", rawSubject)
	}
	if !strings.Contains(unicode.Subject, "☕") {
		t.Errorf("the emoji did not survive the round trip: %q", unicode.Subject)
	}
}

// headerNames lists the field names in a raw message's header block, in order.
// Continuation lines are skipped, which is precisely what makes this able to
// tell "a header called Bcc" apart from "the text 'Bcc:' inside a folded
// Subject" — the distinction V-3 turns on.
func headerNames(raw string) []string {
	var names []string
	for _, line := range strings.Split(raw, "\r\n") {
		if line == "" {
			break // end of the header block
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // folded continuation of the previous field
		}
		name, _, ok := strings.Cut(line, ":")
		if ok {
			names = append(names, strings.TrimSpace(name))
		}
	}
	return names
}

// rawHeaderLine returns a header field as it appears on the wire, unfolded,
// which is what an assertion about encoding has to look at.
func rawHeaderLine(raw, name string) string {
	lines := strings.Split(raw, "\r\n")
	prefix := strings.ToLower(name) + ":"
	for i, line := range lines {
		if !strings.HasPrefix(strings.ToLower(line), prefix) {
			continue
		}
		field := line
		for _, cont := range lines[i+1:] {
			if cont == "" || (!strings.HasPrefix(cont, " ") && !strings.HasPrefix(cont, "\t")) {
				break
			}
			field += cont
		}
		return field
	}
	return ""
}
