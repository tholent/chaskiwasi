package syncsvc

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// outboxOfEveryOutcome is one request exercising all four terminal ack
// statuses at once (§4.7), so replay can be asserted over the whole set.
func outboxOfEveryOutcome() []protocol.Outbound {
	return []protocol.Outbound{
		{LocalID: "o-000001", ContactID: "c_01", Subject: "camping!", Body: "we saw a fox"},
		{LocalID: "o-000002", ContactID: "c_07", Body: "hi Rosa"},
		{LocalID: "o-000003", ContactID: "c_99", Body: "who?"},
		{LocalID: "o-000004", ContactID: "c_01", Body: "   "},
	}
}

func TestOutbound_TerminalStatuses(t *testing.T) {
	hn := newHarness(t)

	resp := hn.sync(protocol.Request{
		Cursor: hn.currentCursor(), AylluVersion: 7, Outbound: outboxOfEveryOutcome(),
	})

	want := []protocol.Ack{
		{LocalID: "o-000001", Status: protocol.AckSent},
		{LocalID: "o-000002", Status: protocol.AckRejectedInactive},
		{LocalID: "o-000003", Status: protocol.AckRejectedUnknownContact},
		{LocalID: "o-000004", Status: protocol.AckInvalid},
	}
	assertAcks(t, resp.Acks, want)

	if hn.sub.count() != 1 {
		t.Fatalf("SMTP sends = %d, want 1 — only the resolvable, valid letter is sent", hn.sub.count())
	}
	if to := hn.sub.sent[0].to; len(to) != 1 || to[0] != "abuela@example.test" {
		t.Fatalf("recipients = %v, want the resolved contact address only", to)
	}
	if from := hn.sub.sent[0].from; from != "kid@chaski.test" {
		t.Fatalf("From = %q, want the child's own address", from)
	}
}

func TestV9_ReplayedSyncReturnsIdenticalAcksIncludingRejections(t *testing.T) {
	hn := newHarness(t)
	req := protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 7, Outbound: outboxOfEveryOutcome()}

	first := hn.sync(req)
	sendsAfterFirst := hn.sub.count()

	// Between the two syncs the contact list changes underneath: c_07 comes
	// back, c_99 is added. A replayed rejection must still replay — the ack
	// ring is the answer, not a fresh evaluation (§4.7).
	hn.ayllu.contacts[1].Active = true
	hn.ayllu.contacts = append(hn.ayllu.contacts, hn.ayllu.contacts[0])
	hn.ayllu.contacts[2].ID = "c_99"

	second := hn.sync(req)

	assertAcks(t, second.Acks, first.Acks)
	if hn.sub.count() != sendsAfterFirst {
		t.Fatalf("SMTP sends = %d after replay, want %d — a replay must never re-send",
			hn.sub.count(), sendsAfterFirst)
	}
}

func TestOutbound_SMTPFailureLeavesTheLetterUnacked(t *testing.T) {
	hn := newHarness(t)
	hn.sub.err = fmt.Errorf("550 mailbox full")

	resp := hn.sync(protocol.Request{
		Cursor: hn.currentCursor(), AylluVersion: 7,
		Outbound: []protocol.Outbound{
			{LocalID: "o-1", ContactID: "c_01", Body: "hello"},
			{LocalID: "o-2", ContactID: "c_07", Body: "hello"},
		},
	})

	// Every ack is terminal and makes the device drop the letter, so a failed
	// send gets no ack at all: it stays in the outbox and is re-sent (§4.7).
	assertAcks(t, resp.Acks, []protocol.Ack{{LocalID: "o-2", Status: protocol.AckRejectedInactive}})

	// And it stays unacked in the ring, so the next sync genuinely retries.
	ring := hn.state.Snapshot().Acks
	if _, replayed := ring.Lookup("o-1"); replayed {
		t.Fatal("a letter that failed SMTP was recorded in the ack ring")
	}

	hn.sub.err = nil
	retry := hn.sync(protocol.Request{
		Cursor: hn.currentCursor(), AylluVersion: 7,
		Outbound: []protocol.Outbound{{LocalID: "o-1", ContactID: "c_01", Body: "hello"}},
	})
	assertAcks(t, retry.Acks, []protocol.Ack{{LocalID: "o-1", Status: protocol.AckSent}})
}

func TestOutbound_UnreachableSubmissionIs503(t *testing.T) {
	hn := newHarness(t)
	hn.sub.err = fmt.Errorf("dial: %w", mailbox.ErrUnreachable)

	w := hn.post(protocol.Request{
		Cursor: hn.currentCursor(), AylluVersion: 7,
		Outbound: []protocol.Outbound{{LocalID: "o-1", ContactID: "c_01", Body: "hello"}},
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
}

func TestOutbound_Validation(t *testing.T) {
	tests := []struct {
		name    string
		letter  protocol.Outbound
		rawJSON string
		want    protocol.AckStatus
	}{
		{
			name:   "empty body",
			letter: protocol.Outbound{LocalID: "o-1", ContactID: "c_01", Body: ""},
			want:   protocol.AckInvalid,
		},
		{
			name:   "whitespace-only body",
			letter: protocol.Outbound{LocalID: "o-1", ContactID: "c_01", Body: " \n\t "},
			want:   protocol.AckInvalid,
		},
		{
			name:   "body over the grapheme cap",
			letter: protocol.Outbound{LocalID: "o-1", ContactID: "c_01", Body: strings.Repeat("a", 501)},
			want:   protocol.AckInvalid,
		},
		{
			name:   "body exactly at the grapheme cap",
			letter: protocol.Outbound{LocalID: "o-1", ContactID: "c_01", Body: strings.Repeat("a", 500)},
			want:   protocol.AckSent,
		},
		{
			name: "emoji body counted in graphemes, not bytes",
			// 500 family emoji (ZWJ sequences): far over the byte and rune
			// caps, exactly at the grapheme cap (§0).
			letter: protocol.Outbound{LocalID: "o-1", ContactID: "c_01", Body: strings.Repeat("\U0001F468\u200d\U0001F469\u200d\U0001F467", 500)},
			want:   protocol.AckSent,
		},
		{
			name:   "local_id over its wire cap",
			letter: protocol.Outbound{LocalID: strings.Repeat("o", 33), ContactID: "c_01", Body: "hi"},
			want:   protocol.AckInvalid,
		},
		{
			name:   "writing to the system contact",
			letter: protocol.Outbound{LocalID: "o-1", ContactID: protocol.SysContactID, Body: "hi"},
			want:   protocol.AckRejectedUnknownContact,
		},
		{
			name:    "unknown field on one letter invalidates that letter only",
			rawJSON: `{"local_id":"o-1","contact_id":"c_01","body":"hi","reply_to":"l-123"}`,
			want:    protocol.AckInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hn := newHarness(t)

			var resp protocol.Response
			if tc.rawJSON != "" {
				body := fmt.Sprintf(`{"cursor":%q,"ayllu_version":7,"outbound":[%s]}`, hn.currentCursor(), tc.rawJSON)
				w := hn.postRaw(body, "Bearer "+testToken)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", w.Code)
				}
				decodeInto(t, w.Body.Bytes(), &resp)
			} else {
				resp = hn.sync(protocol.Request{
					Cursor: hn.currentCursor(), AylluVersion: 7,
					Outbound: []protocol.Outbound{tc.letter},
				})
			}

			if len(resp.Acks) != 1 || resp.Acks[0].Status != tc.want {
				t.Fatalf("acks = %+v, want a single %s", resp.Acks, tc.want)
			}
		})
	}
}

func TestOutbound_LetterWithNoLocalIDIsARequestError(t *testing.T) {
	hn := newHarness(t)
	// There is no ack channel for a letter with no local_id, so absorbing it
	// would strand the letter on the device with nothing in the logs.
	body := fmt.Sprintf(`{"cursor":%q,"ayllu_version":7,"outbound":[{"contact_id":"c_01","body":"hi"}]}`, hn.currentCursor())
	if got := hn.postRaw(body, "Bearer "+testToken).Code; got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
}

func TestV8_OutboundCarriesMessageIDAndNoThreadingHeaders(t *testing.T) {
	hn := newHarness(t)

	hn.sync(protocol.Request{
		Cursor: hn.currentCursor(), AylluVersion: 7,
		Outbound: []protocol.Outbound{{LocalID: "o-1", ContactID: "c_01", Subject: "camping!", Body: "we saw a fox"}},
	})
	if hn.sub.count() != 1 {
		t.Fatalf("SMTP sends = %d, want 1", hn.sub.count())
	}
	msg := string(hn.sub.sent[0].msg)
	headers, _, _ := strings.Cut(msg, "\r\n\r\n")

	// A.1: the product is passing notes, not conducting email. This test fails
	// loudly if anyone "fixes" that.
	for _, banned := range []string{"In-Reply-To:", "References:", "Thread-Topic:", "Thread-Index:"} {
		if strings.Contains(headers, banned) {
			t.Fatalf("outbound carries %s — A.1 forbids every threading header:\n%s", banned, headers)
		}
	}
	if strings.Contains(headers, "Subject: Re:") || strings.Contains(headers, "Subject: RE:") {
		t.Fatalf("outbound subject carries a Re: prefix — A.1 forbids it:\n%s", headers)
	}

	if !strings.Contains(headers, "Message-ID: <") || !strings.Contains(headers, "@chaski.test>") {
		t.Fatalf("outbound is missing a Message-ID at the child's own domain:\n%s", headers)
	}
	if !strings.Contains(headers, "Subject: camping!") {
		t.Fatalf("child-authored subject not used verbatim:\n%s", headers)
	}
	if !strings.Contains(headers, "From: kid@chaski.test") || !strings.Contains(headers, "To: abuela@example.test") {
		t.Fatalf("From/To wrong:\n%s", headers)
	}
	if !strings.Contains(headers, "Date: ") {
		t.Fatalf("outbound has no Date header:\n%s", headers)
	}
}

func TestV3_OutboundSubjectCannotInjectAHeader(t *testing.T) {
	hn := newHarness(t)

	hn.sync(protocol.Request{
		Cursor: hn.currentCursor(), AylluVersion: 7,
		Outbound: []protocol.Outbound{{
			LocalID:   "o-1",
			ContactID: "c_01",
			Subject:   "hi\r\nBcc: attacker@evil.test",
			Body:      "hello",
		}},
	})

	msg := string(hn.sub.sent[0].msg)
	headers, _, _ := strings.Cut(msg, "\r\n\r\n")

	// The payload survives as flattened text inside the Subject value, which
	// is fine and even honest; what must not exist is a header line of its
	// own. Assert per line, on the field name, not on a substring of the file.
	subjects := 0
	for _, line := range strings.Split(headers, "\r\n") {
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "bcc", "cc":
			t.Fatalf("subject injected a header:\n%s", headers)
		case "subject":
			subjects++
		}
	}
	if subjects != 1 {
		t.Fatalf("Subject headers = %d, want exactly 1:\n%s", subjects, headers)
	}
}

func TestOutbound_GeneratedSubjectWhenTheDeviceSendsNone(t *testing.T) {
	hn := newHarness(t)

	hn.sync(protocol.Request{
		Cursor: hn.currentCursor(), AylluVersion: 7,
		Outbound: []protocol.Outbound{{LocalID: "o-1", ContactID: "c_01", Body: "we saw a fox by the river"}},
	})

	headers, _, _ := strings.Cut(string(hn.sub.sent[0].msg), "\r\n\r\n")
	if !strings.Contains(headers, "Subject: we saw a fox by the river") {
		t.Fatalf("generated subject missing or wrong:\n%s", headers)
	}
}

func assertAcks(t *testing.T, got, want []protocol.Ack) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("acks = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ack %d = %+v, want %+v (full: %+v)", i, got[i], want[i], got)
		}
	}
}
