//go:build e2e

package e2e

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

// TestV11_StorageInvariant is §15's V-11 and the sharpest edge of I-1: after a
// full flow — inbound, outbound, quarantine, release, a contact change and a
// kipu block — no file under /data or /backups, and no line of Wasi's or
// strip's container logs, contains any part of any letter.
//
// Why it matters: I-1 says the mailbox is the sole store of letters, and every
// other promise this system makes is downstream of it. A lost or stolen device,
// a backup on somebody's laptop, a log aggregator — none of them should be able
// to yield a letter. The invariant is stated once and can be broken anywhere,
// which is why the check walks every file rather than a list of the ones a test
// author thought of.
//
// maddy's logs are out of scope, deliberately. It is the mail store here,
// standing in for Fastmail; I-1 constrains Wasi and the shared services, and
// has never claimed the provider does not hold the letters — it does, that is
// its job (§1, §3).
func TestV11_StorageInvariant(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	var probes probeSet

	theo := h.addContact(t, "Theo", relativeAddress)
	dev.sync(t)

	// Inbound, including one with a quoted tail so the body actually reaches
	// the strip service over HTTP — the shared service's logs are half of what
	// V-11 greps, and a body that never got there would not test them.
	plainMark, quotedMark := nonce(t), nonce(t)
	plainBody := "the campfire smelled of woodsmoke " + plainMark
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "marshmallows " + plainMark,
		MessageID: "storage-plain@chaski.test",
		Body:      plainBody,
	}.bytes())
	h.mail.add(t, childAddress, inboxFolder, quotedReply(quotedMark, "storage-quoted@chaski.test"))
	h.mail.waitForMark(t, childAddress, inboxFolder, quotedMark, 30*time.Second)

	// A stranger's letter, quarantined and then released through the UI: the
	// Held path reads message bodies into the guardian UI's own code, which is
	// a place a body could plausibly be logged.
	strangerMark := nonce(t)
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Marta <" + strangerAddress + ">",
		Subject:   "thunderstorm " + strangerMark,
		MessageID: "storage-stranger@chaski.test",
		Body:      "the thunderstorm knocked the power out " + strangerMark,
	}.bytes())
	h.waitForHeld(t, strangerMark, 30*time.Second)
	h.ui.release(t, h.ui.heldRowFrom(t, strangerAddress).UID, "Marta")

	// Outbound, plus a kipu block, in one request — every write path this
	// deployment has, exercised before anything is inspected.
	outboundMark := nonce(t)
	outboundBody := "we paddled the canoe until sundown " + outboundMark
	resp := dev.raw(t, protocol.Request{
		Cursor: "",
		Kipu:   map[string]any{"battery_pct": 84, "rat": "ltem", "rssi": -97, "queue_depth": 1, "fw": "0.3.1"},
		Outbound: []protocol.Outbound{
			{LocalID: "o-storage", ContactID: theo, Subject: "canoeing " + outboundMark, Body: outboundBody},
		},
	})
	if got := ackFor(t, resp, "o-storage").Status; got != protocol.AckSent {
		t.Fatalf("the outbound letter was acked %q, want %q; nothing was written to grep for", got, protocol.AckSent)
	}
	h.mail.waitForCount(t, relativeAddress, inboxFolder, 1, 30*time.Second)

	// A contact change, so pending_notices and the change log have been
	// written too.
	h.ui.deactivate(t, theo)

	// Everything the device was actually shown, taken from the wire rather
	// than from what the test believes it sent — if derivation changed a body,
	// the changed body is the one that must not have been persisted.
	final := dev.windowResync(t)
	for _, l := range final.Letters {
		probes.add(l.Subject, l.Body)
	}
	probes.add("canoeing "+outboundMark, outboundBody)
	probes.add("marshmallows "+plainMark, plainBody)

	if len(probes.strings()) == 0 {
		t.Fatal("no letter text was collected; this run would assert nothing")
	}

	// Positive control. A grep that finds nothing is only evidence if it is
	// capable of finding something, and every failure mode of this test —
	// probes that never got built, letters that never arrived, a search that
	// silently matches nothing — looks exactly like a pass. The mailbox is
	// where the letters are *supposed* to be, so the probes must hit there.
	var mailbox strings.Builder
	for _, msg := range h.mail.messages(t, childAddress, inboxFolder) {
		mailbox.Write(msg.Raw)
	}
	for _, msg := range h.mail.messages(t, relativeAddress, inboxFolder) {
		mailbox.Write(msg.Raw)
	}
	if probes.firstIn(mailbox.String()) == "" {
		t.Fatalf("none of the %d probes appears in the mailbox either, so this test cannot detect a leak",
			len(probes.strings()))
	}

	// §3: "the storage-invariant test greps /data **and** /backups". Run a real
	// backup so that half is not asserting over an empty directory — a backup
	// is a copy of /data on somebody's laptop, and it is the copy most likely
	// to outlive every other control this system has.
	h.stack.exec(t, "wasi", nil, "/usr/local/bin/wasi", "backup")

	// The files. Every regular file in both volumes, read through a helper
	// container — the question is what is on disk, not what the process would
	// admit to.
	for volume, files := range map[string]map[string][]byte{
		"/data":    h.stack.volumeFiles(t, h.dataVolume),
		"/backups": h.stack.volumeFiles(t, h.backupVolume),
	} {
		for name, data := range files {
			if found := probes.firstIn(string(data)); found != "" {
				t.Errorf("%s/%s contains letter text %q (I-1, §3)", volume, name, found)
			}
		}
	}

	// A backup that produced no files would make the grep above vacuous, and a
	// vacuous grep is indistinguishable from a clean one.
	if len(h.backupFiles(t)) == 0 {
		t.Error("`wasi backup` wrote nothing to /backups, so V-11's backup half asserted over an empty directory (§3)")
	}

	// The logs. §14 is explicit that this holds "at any log level"; letter ids
	// are what may be logged for correlation, and nothing else.
	for service, out := range h.logsSinceReset(t) {
		if strings.TrimSpace(out) == "" {
			t.Errorf("captured no %s logs at all, so the log half of V-11 asserted nothing", service)
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			if found := probes.firstIn(line); found != "" {
				t.Errorf("a %s log line contains letter text %q (I-1, §14):\n%s", service, found, line)
			}
		}
	}
}

// probeSet is the collection of strings V-11 searches for.
//
// Choosing them is the whole difficulty of this test. Real prose is a bad
// probe in both directions: a short common word that *is* leaked hides in
// plain sight, and a naive search for a fragment like "tent" matches
// "contents" and "non-existent" in files that hold nothing of the kind, so the
// test reports a leak that is not there and gets weakened until it reports
// nothing at all.
//
// So every letter in this test is built from a per-run 18-character random
// nonce plus a few deliberately chosen long words — campfire, woodsmoke,
// marshmallows, thunderstorm, canoe, sundown — none of which appears anywhere
// in Wasi's own vocabulary, config, templates or log messages. The probes are
// the whole subject, the whole body, and every token of eight characters or
// more, which for these letters means the nonces and those words. A body
// fragment that reached disk or a log line has to contain one.
type probeSet struct {
	seen map[string]bool
}

// minProbeLen is the shortest token worth searching for. Below this, a match
// is as likely to be a coincidence in a TOML comment as a leak.
const minProbeLen = 8

func (p *probeSet) add(texts ...string) {
	if p.seen == nil {
		p.seen = make(map[string]bool)
	}
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		p.seen[text] = true
		for _, field := range strings.Fields(text) {
			field = strings.Trim(field, ".,!?;:\"'()<>")
			if len(field) >= minProbeLen {
				p.seen[field] = true
			}
		}
	}
}

// strings returns the probes, sorted so a failure reads the same way twice.
func (p *probeSet) strings() []string {
	out := make([]string, 0, len(p.seen))
	for s := range p.seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// firstIn returns the first probe found in haystack, or "".
func (p *probeSet) firstIn(haystack string) string {
	for _, probe := range p.strings() {
		if strings.Contains(haystack, probe) {
			return probe
		}
	}
	return ""
}
