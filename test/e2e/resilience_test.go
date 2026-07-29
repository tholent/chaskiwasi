//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

// quotedReply is a reply with the tail a real mail client leaves behind: an
// attribution line and a `>`-quoted block. It is the shape strip exists for.
func quotedReply(mark, messageID string) []byte {
	return letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "about your week " + mark,
		MessageID: messageID,
		Date:      time.Now().Add(-time.Hour),
		Body: "That sounds like so much fun " + mark + "\n" +
			"\n" +
			"On Mon, Jan 5, 2026 at 2:00 PM, Maya <" + childAddress + "> wrote:\n" +
			"> We are going camping this weekend near the lake.\n" +
			">\n" +
			"> Cannot wait!\n",
	}.bytes()
}

// waitForStrip blocks until the strip service answers its health endpoint.
func waitForStrip(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	waitFor(t, 60*time.Second, "strip to come back", func() error {
		resp, err := client.Get(stripHealthURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("strip healthz: status %d", resp.StatusCode)
		}
		return nil
	})
}

// TestV10_StripOutageDegradesAndHeals is §15's V-10: with strip killed, letters
// still deliver, flagged `degraded`; with strip back, the next sync re-derives
// the same letter cleanly, with no stale copy anywhere.
//
// Why it matters, and why it has to be a real kill: §5.3's promise is that a
// Python container being down must not delay a letter from family. The second
// half is the subtler one — "since nothing is cached, there is no stale copy to
// invalidate". That is a claim about the *absence* of a cache, and the only way
// to test an absence is to produce two different derivations of one message and
// watch the second one win. A mocked strip error proves the flag is set; it
// cannot prove there is nowhere for the degraded body to have been kept.
func TestV10_StripOutageDegradesAndHeals(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	h.ui.addContact(t, "Theo", relativeAddress)
	mark := nonce(t)
	h.mail.add(t, childAddress, inboxFolder, quotedReply(mark, "strip-outage@chaski.test"))
	h.mail.waitForMark(t, childAddress, inboxFolder, mark, 30*time.Second)

	h.stack.kill(t, "strip")
	t.Cleanup(func() {
		h.stack.start(t, "strip")
		waitForStrip(t)
	})

	degraded := letterWithBody(t, dev.windowResync(t), mark)
	if !degraded.Degraded {
		t.Fatalf("strip is dead and the letter is not flagged degraded (§5.3)")
	}
	if degraded.Body == "" {
		t.Fatalf("strip is dead and the letter arrived empty; §5.3 says it must still deliver")
	}

	h.stack.start(t, "strip")
	waitForStrip(t)

	clean := letterWithBody(t, dev.windowResync(t), mark)
	if clean.Degraded {
		t.Errorf("strip is back and the letter is still flagged degraded (§5.3)")
	}
	// §4.5: the id is stable across re-derivation, which is what makes
	// re-delivery safe for the device.
	if clean.ID != degraded.ID {
		t.Errorf("re-derivation changed the letter id from %q to %q (§4.5)", degraded.ID, clean.ID)
	}
	if !clean.Trimmed {
		t.Errorf("strip is back but the quoted tail was not removed (§5.2)")
	}
	// The proof there was no stale copy: the second derivation produced a
	// different body from the first. A cache would have handed back the
	// degraded one.
	if clean.Body == degraded.Body {
		t.Errorf("the re-derived body is byte-identical to the degraded one, so it came from a cache (§5.3)")
	}
	if strings.Contains(clean.Body, "Cannot wait") {
		t.Errorf("the quoted tail survived the clean re-derivation:\n%q", clean.Body)
	}
}

// TestV9_ReplayIsIdempotent is §15's V-9: a byte-identical replayed sync sends
// nothing twice, returns the same acks — rejections included — and delivers
// byte-identical bodies under unchanged config.
//
// Why it matters: §4.1 says retrying the identical request is always safe, and
// the device is built on that — it retries on every transient failure, with no
// idea whether the previous attempt was processed. If a replay re-sent, every
// flaky link would multiply letters at the relative's end; if a replay
// re-*processed* a rejection, a letter the device was told was invalid would
// get a second, possibly different answer.
func TestV9_ReplayIsIdempotent(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice(t)

	theo := h.addContact(t, "Theo", relativeAddress)
	inboundMark := nonce(t)
	h.mail.add(t, childAddress, inboxFolder, letter{
		From:      "Theo <" + relativeAddress + ">",
		Subject:   "one letter " + inboundMark,
		MessageID: "replay-inbound@chaski.test",
		Body:      "the same words every time " + inboundMark,
	}.bytes())
	h.mail.waitForMark(t, childAddress, inboxFolder, inboundMark, 30*time.Second)

	// One letter that will send, one that will be rejected. §4.7 records
	// rejections in the ack ring too, precisely so a replay gets the same
	// rejection back rather than a fresh attempt.
	req := protocol.Request{
		Cursor:       "",
		AylluVersion: 0,
		Outbound: []protocol.Outbound{
			{LocalID: "o-replay-ok", ContactID: theo, Subject: "hello", Body: "first and only"},
			{LocalID: "o-replay-bad", ContactID: "c_nobody", Subject: "hello", Body: "nowhere to go"},
		},
	}

	first := dev.raw(t, req)
	h.mail.waitForCount(t, relativeAddress, inboxFolder, 1, 30*time.Second)

	second := dev.raw(t, req)

	if got, want := ackFor(t, second, "o-replay-ok").Status, ackFor(t, first, "o-replay-ok").Status; got != want {
		t.Errorf("replayed ack for the sent letter is %q, want %q (§4.7)", got, want)
	}
	if got, want := ackFor(t, second, "o-replay-bad").Status, ackFor(t, first, "o-replay-bad").Status; got != want {
		t.Errorf("replayed ack for the rejected letter is %q, want %q (§4.7)", got, want)
	}
	if got := ackFor(t, second, "o-replay-bad").Status; got != protocol.AckRejectedUnknownContact {
		t.Errorf("unknown contact acked %q, want %q", got, protocol.AckRejectedUnknownContact)
	}
	if n := h.mail.count(t, relativeAddress, inboxFolder); n != 1 {
		t.Errorf("the relative holds %d copies after one replay, want 1 (§4.7)", n)
	}

	// §5.2: derivation is deterministic under unchanged config, which is what
	// makes replays and window resyncs safe to serve at all.
	firstLetter := letterWithBody(t, first, inboundMark)
	secondLetter := letterWithBody(t, second, inboundMark)
	if firstLetter != secondLetter {
		t.Errorf("re-derivation changed the letter under unchanged config (§5.2):\n%+v\n%+v",
			firstLetter, secondLetter)
	}
}

// TestV12_CrashConsistency is §15's V-12, and it kills real processes because
// there is no other way to write it.
//
// Two claims, taken separately:
//
//   - **Mid-write.** SIGKILL during a contact change leaves ayllu.toml
//     parseable and loses no contact that the change log says was written.
//     §3's write discipline (temp file, fsync, rename, fsync the directory) is
//     what a database would otherwise have provided (A.9); a partial file here
//     would take the whole contact list, and with it every letter's rendering
//     key (§7.1).
//   - **Mid-sync.** A sync that was answered stays answered across a crash:
//     the ack ring is fsynced before the ack is handed over (§4.7 step 5), so a
//     replay after a restart returns the recorded outcome instead of sending
//     again.
//
// Note what is deliberately *not* asserted: that a crash between the SMTP send
// and the ack write never duplicates. §4.7 buys that duplicate on purpose — "a
// relative seeing a letter twice is the correct failure; a letter the kid
// watched leave that never arrives is not". A test forbidding it would be a
// test against the spec.
func TestV12_CrashConsistency(t *testing.T) {
	h := newHarness(t)

	t.Run("MidWrite", func(t *testing.T) {
		delays := []time.Duration{5, 15, 30, 50, 80, 120}
		for _, delay := range delays {
			name := "Kill" + nonce(t)[:8]

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
			<-posted

			// The file has to be readable by something other than the process
			// that wrote it, before that process gets a chance to repair it.
			assertAylluParses(t, h)

			h.stack.start(t, "wasi")
			h.waitReady(t)
			h.ui = newGuardianUI(t, h.stack)
			h.ui.login(t, guardianName, h.guardianPass)
		}

		// §7.6 appends the change-log line *after* ayllu.toml is durable, so
		// every add the log records must be present in the file. This is the
		// "no contact lost" half, checked against a durable record rather than
		// against the test's own idea of what it asked for.
		byName := map[string]bool{}
		for _, c := range h.contacts(t) {
			byName[c.Name] = true
		}
		for _, ev := range h.changeLog(t) {
			if ev.Action == "add" && !byName[ev.Name] {
				t.Errorf("the change log records adding %q but ayllu.toml has no such contact (§3, §7.6)", ev.Name)
			}
		}
	})

	t.Run("MidSyncReplayDoesNotDoubleSend", func(t *testing.T) {
		theo := h.contactIDByAddress(t, relativeAddress)
		if theo == "" {
			theo = h.addContact(t, "Theo", relativeAddress)
		}
		dev := h.newDevice(t)

		req := protocol.Request{
			Cursor: "",
			Outbound: []protocol.Outbound{
				{LocalID: "o-crash-replay", ContactID: theo, Subject: "hello", Body: "exactly once please"},
			},
		}

		before := h.mail.count(t, relativeAddress, inboxFolder)
		first := dev.raw(t, req)
		if got := ackFor(t, first, "o-crash-replay").Status; got != protocol.AckSent {
			t.Fatalf("outbound acked %q, want %q", got, protocol.AckSent)
		}
		h.mail.waitForCount(t, relativeAddress, inboxFolder, before+1, 30*time.Second)

		// The crash lands after the ack was fsynced, which is the case §4.7
		// promises is safe.
		h.restartWasi(t)

		second := dev.raw(t, req)
		if got := ackFor(t, second, "o-crash-replay").Status; got != protocol.AckSent {
			t.Errorf("replayed ack is %q, want the recorded %q (§4.7)", got, protocol.AckSent)
		}
		if n := h.mail.count(t, relativeAddress, inboxFolder); n != before+1 {
			t.Errorf("the relative holds %d copies after a crash and a replay, want %d — the ack ring did not survive (§4.7)",
				n, before+1)
		}
	})

	t.Run("KilledMidSyncLosesNothing", func(t *testing.T) {
		theo := h.contactIDByAddress(t, relativeAddress)
		dev := h.newDevice(t)

		req := protocol.Request{
			Cursor: "",
			Outbound: []protocol.Outbound{
				{LocalID: "o-killed-midflight", ContactID: theo, Subject: "hello", Body: "at least once"},
			},
		}

		before := h.mail.count(t, relativeAddress, inboxFolder)

		// Kill while the sync is genuinely in flight. Where it lands is not
		// controllable and does not need to be: §4.7 says the outcome is either
		// "nothing happened" or "sent but unacked", and the letter is never
		// lost in either.
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = dev.client.Sync(t.Context(), req)
		}()
		time.Sleep(25 * time.Millisecond)
		h.stack.kill(t, "wasi")
		<-done

		h.stack.start(t, "wasi")
		h.waitReady(t)
		assertAylluParses(t, h)

		// The device resends what it was never acked for. §4.7's contract is
		// that this costs at most a duplicate, never a loss.
		replay := dev.raw(t, req)
		if got := ackFor(t, replay, "o-killed-midflight").Status; got != protocol.AckSent {
			t.Errorf("after a mid-sync crash the replay acked %q, want %q", got, protocol.AckSent)
		}
		after := h.mail.count(t, relativeAddress, inboxFolder)
		switch {
		case after <= before:
			t.Errorf("the letter was lost across a mid-sync crash; §4.7 refuses to buy that")
		case after > before+2:
			t.Errorf("the letter arrived %d times; §4.7 permits at most one duplicate", after-before)
		}
	})
}

// assertAylluParses reads ayllu.toml straight off the volume and insists it is
// a whole TOML document. A missing file is fine — nothing has written one yet;
// a truncated one is the failure §3's write discipline exists to prevent.
func assertAylluParses(t *testing.T, h *harness) {
	t.Helper()
	data, ok := h.stack.volumeFiles(t, h.dataVolume)["ayllu.toml"]
	if !ok {
		return
	}
	var ff struct {
		Version  int           `toml:"version"`
		Contacts []fileContact `toml:"contacts"`
	}
	if err := toml.Unmarshal(data, &ff); err != nil {
		t.Fatalf("ayllu.toml did not survive a crash intact (§3):\n%v\n%s", err, data)
	}
}

// changeEvent is one line of ayllu-log.jsonl, the append-only audit trail.
type changeEvent struct {
	Action    string `json:"action"`
	ContactID string `json:"contact_id"`
	Name      string `json:"name"`
}

// changeLog reads /data/ayllu-log.jsonl. It is the durable record §7.6 writes
// before anything else can go wrong, which is what makes it the right thing to
// check a crashed write against.
func (h *harness) changeLog(t *testing.T) []changeEvent {
	t.Helper()
	data, ok := h.stack.volumeFiles(t, h.dataVolume)["ayllu-log.jsonl"]
	if !ok {
		return nil
	}
	var out []changeEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev changeEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("ayllu-log.jsonl line did not parse (§3): %v\n%s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

// contactIDByAddress returns the contact id holding addr, or "".
func (h *harness) contactIDByAddress(t *testing.T, addr string) string {
	t.Helper()
	for _, c := range h.contacts(t) {
		if strings.EqualFold(c.Address, addr) {
			return c.ID
		}
	}
	return ""
}
