package derive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/strip"
)

// fakeAyllu resolves whatever addresses it's given at construction, keyed
// verbatim (no normalisation) — the real normalisation rules belong to and
// are tested by the ayllu package; Derive only needs to know it calls
// Resolve, never ResolveActive.
type fakeAyllu map[string]ayllu.Contact

func (f fakeAyllu) Resolve(addr string) (ayllu.Contact, bool) {
	c, ok := f[addr]
	return c, ok
}

// stripCall records one invocation of fakeStripper.Strip, for tests that
// need to assert what Derive passed in (the text/plain part only, and the
// right formatFlowed flag).
type stripCall struct {
	text         string
	formatFlowed bool
}

// fakeStripper is a Stripper whose behaviour is supplied by the test, so
// derive's own tests never depend on talon or a live HTTP server — that
// belongs to internal/strip's own test suite. fn defaults to a pass-through
// stripper that never trims anything.
type fakeStripper struct {
	calls []stripCall
	fn    func(text string, formatFlowed bool) strip.Result
}

func (f *fakeStripper) Strip(ctx context.Context, text string, formatFlowed bool) strip.Result {
	f.calls = append(f.calls, stripCall{text, formatFlowed})
	if f.fn != nil {
		return f.fn(text, formatFlowed)
	}
	return strip.Result{Body: text}
}

// cutAtQuoteMarker is a fakeStripper behaviour standing in for the live
// service or the Go fallback: it removes everything from the first "\n> "
// onward, exactly the shape of a trailing quoted reply V-4 exercises,
// without pulling in a real strip implementation into derive's own tests.
func cutAtQuoteMarker(text string, formatFlowed bool) strip.Result {
	if i := strings.Index(text, "\n> "); i >= 0 {
		return strip.Result{Body: text[:i], Trimmed: true}
	}
	return strip.Result{Body: text}
}

func testConfig(t *testing.T, resolver AylluResolver, stripper Stripper, maxChars int) Config {
	t.Helper()
	return Config{Ayllu: resolver, Strip: stripper, MaxLetterChars: maxChars}
}

func mustPipeline(t *testing.T, cfg Config) *Pipeline {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// rawLetter builds a minimal RFC 5322 message. extraHeaders is inserted
// verbatim (already CRLF-terminated) between the fixed headers and the
// blank line, so tests can add Content-Type/format=flowed or omit
// Message-Id without a combinatorial explosion of helper signatures.
func rawLetter(uid uint32, from, subject, extraHeaders, body string) mailbox.Raw {
	msg := fmt.Sprintf(
		"From: %s\r\n"+
			"To: kid@example.com\r\n"+
			"Subject: %s\r\n"+
			"Date: Wed, 7 Jan 2026 11:00:00 -0500\r\n"+
			"%s"+
			"\r\n"+
			"%s",
		from, subject, extraHeaders, body,
	)
	return mailbox.Raw{
		UID:          uid,
		InternalDate: time.Date(2026, 1, 7, 16, 1, 0, 0, time.UTC), // just after the Date header, in UTC
		Data:         []byte(msg),
	}
}

var rosa = ayllu.Contact{ID: "c_01", Name: "Rosa", Address: "rosa@example.com", Active: true}

// TestV4_QuotedTailAndEncodedSubject is the named regression for V-4:
// inbound with a quoted tail and a chained, RFC 2047-encoded subject must
// come out stripped (trimmed: true) with the subject decoded and collapsed.
func TestV4_QuotedTailAndEncodedSubject(t *testing.T) {
	body := "Camping was so much fun, thank you!\n> Did you have fun camping?\n> - Rosa"
	r := rawLetter(1, rosa.Address,
		"=?utf-8?B?UmU6IFJlOiBGd2Q6IGNhbXBpbmch?=", // "Re: Re: Fwd: camping!"
		"Content-Type: text/plain; charset=utf-8\r\n"+
			"Message-Id: <v4@example.com>\r\n",
		body)

	p := mustPipeline(t, testConfig(t,
		fakeAyllu{rosa.Address: rosa},
		&fakeStripper{fn: cutAtQuoteMarker},
		500,
	))

	letter, err := p.Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	if letter.Subject != "camping!" {
		t.Errorf("Subject = %q, want %q", letter.Subject, "camping!")
	}
	if !letter.Trimmed {
		t.Errorf("Trimmed = false, want true")
	}
	if letter.Truncated {
		t.Errorf("Truncated = true, want false")
	}
	if letter.Degraded {
		t.Errorf("Degraded = true, want false")
	}
	if letter.ContactID != rosa.ID {
		t.Errorf("ContactID = %q, want %q", letter.ContactID, rosa.ID)
	}
	if strings.Contains(letter.Body, "Did you have fun camping") {
		t.Errorf("Body still contains the quoted tail: %q", letter.Body)
	}
	if !strings.Contains(letter.Body, "so much fun") {
		t.Errorf("Body lost the real reply text: %q", letter.Body)
	}
}

// TestV4_LongBodyTruncated is the other half of V-4: a 5000-grapheme body
// comes back truncated, and the source mailbox.Raw is left untouched — the
// full text graduates with the mailbox; only the device's view is capped.
func TestV4_LongBodyTruncated(t *testing.T) {
	longBody := strings.Repeat("a", 5000)
	r := rawLetter(2, rosa.Address, "long letter",
		"Content-Type: text/plain; charset=utf-8\r\nMessage-Id: <long@example.com>\r\n",
		longBody)
	originalData := append([]byte(nil), r.Data...)

	p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, &fakeStripper{}, 500))

	letter, err := p.Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	if !letter.Truncated {
		t.Errorf("Truncated = false, want true for a 5000-grapheme body against a 500 cap")
	}
	if n := len([]rune(letter.Body)); n != 500 {
		t.Errorf("truncated body has %d runes, want 500 (all-ASCII input, so runes == graphemes)", n)
	}
	if string(r.Data) != string(originalData) {
		t.Errorf("Derive mutated the raw mailbox.Raw.Data it was given")
	}
}

// TestDerive_Deterministic pins §5.2's determinism claim: the same UID under
// the same config yields byte-identical output.
func TestDerive_Deterministic(t *testing.T) {
	r := rawLetter(3, rosa.Address, "=?utf-8?Q?camping=21?=",
		"Content-Type: text/plain; charset=utf-8\r\nMessage-Id: <det@example.com>\r\n",
		"see you soon\n> old quote\n> more old quote")

	newPipeline := func() *Pipeline {
		return mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, &fakeStripper{fn: cutAtQuoteMarker}, 500))
	}

	a, err := newPipeline().Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive (first): %v", err)
	}
	b, err := newPipeline().Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive (second): %v", err)
	}

	if a != b {
		t.Fatalf("non-deterministic output:\n  first:  %+v\n  second: %+v", a, b)
	}
}

// TestDerive_DegradedPath: when the Stripper reports Degraded (§5.3 — strip
// unreachable, Go fallback rules used), that flag must reach the wire letter
// unchanged, distinct from Trimmed (§4.3).
func TestDerive_DegradedPath(t *testing.T) {
	r := rawLetter(4, rosa.Address, "hi",
		"Content-Type: text/plain; charset=utf-8\r\nMessage-Id: <degraded@example.com>\r\n",
		"hello there\n-- \nRosa")

	stripper := &fakeStripper{fn: func(text string, formatFlowed bool) strip.Result {
		// Stands in for the real Fallback (§5.3): the service was
		// unreachable, so whatever this returns is degraded by
		// construction.
		return strip.Result{Body: "hello there", Trimmed: true, Degraded: true}
	}}
	p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, stripper, 500))

	letter, err := p.Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if !letter.Degraded {
		t.Errorf("Degraded = false, want true")
	}
	if !letter.Trimmed {
		t.Errorf("Trimmed = false, want true")
	}
}

// TestDerive_NoTextPlainPart: an HTML-only message must not vanish and must
// not have its HTML rendered into text — the device gets an empty body
// alongside the real subject, sender and date, and Strip is never called
// (nothing to strip, and no reason to pay a network round trip for it).
func TestDerive_NoTextPlainPart(t *testing.T) {
	r := rawLetter(5, rosa.Address, "html only",
		"Content-Type: text/html; charset=utf-8\r\nMessage-Id: <html@example.com>\r\n",
		"<p>hello</p>")

	stripper := &fakeStripper{}
	p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, stripper, 500))

	letter, err := p.Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if letter.Body != "" {
		t.Errorf("Body = %q, want empty (HTML must never be rendered, §5.2)", letter.Body)
	}
	if letter.Trimmed || letter.Truncated || letter.Degraded {
		t.Errorf("flags = (trimmed=%v truncated=%v degraded=%v), want all false", letter.Trimmed, letter.Truncated, letter.Degraded)
	}
	if letter.Subject != "html only" {
		t.Errorf("Subject = %q, want %q — the letter must not vanish", letter.Subject, "html only")
	}
	if len(stripper.calls) != 0 {
		t.Errorf("Strip was called %d times for an empty body, want 0", len(stripper.calls))
	}
}

// TestDerive_MissingMessageID_UsesFallback: Message-ID is rare but legal to
// omit (§4.5); derivation must still produce a stable, well-shaped id rather
// than dropping the letter.
func TestDerive_MissingMessageID_UsesFallback(t *testing.T) {
	r := rawLetter(6, rosa.Address, "no message id",
		"Content-Type: text/plain; charset=utf-8\r\n", // no Message-Id header
		"hello without a message id")

	p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, &fakeStripper{}, 500))

	a, err := p.Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	b, err := p.Derive(context.Background(), r)
	if err != nil {
		t.Fatalf("Derive (again): %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("fallback id not stable across re-derivation: %q vs %q", a.ID, b.ID)
	}
	if !strings.HasPrefix(a.ID, "l-") || len(a.ID) != len("l-")+10 {
		t.Errorf("id %q does not match the l-<10 hex> shape (§4.5)", a.ID)
	}
}

// TestDerive_UnresolvedSender: a sender that doesn't resolve against the
// full ayllu must come back as a typed error, never a silently-dropped
// letter and never a letter rendered under a blank identity.
func TestDerive_UnresolvedSender(t *testing.T) {
	r := rawLetter(7, "stranger@example.com", "hi",
		"Content-Type: text/plain; charset=utf-8\r\nMessage-Id: <stranger@example.com>\r\n",
		"hello")

	p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, &fakeStripper{}, 500))

	_, err := p.Derive(context.Background(), r)
	if err == nil {
		t.Fatalf("Derive returned nil error for an unresolved sender")
	}
	var unresolved *UnresolvedSenderError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error type = %T, want *UnresolvedSenderError", err)
	}
	if unresolved.UID != 7 {
		t.Errorf("UnresolvedSenderError.UID = %d, want 7", unresolved.UID)
	}
	// I-2: the error must never carry the address.
	if strings.Contains(err.Error(), "stranger@example.com") {
		t.Errorf("error message leaks the sender address: %q", err.Error())
	}
}

// TestDerive_ParseError: a message enmime cannot parse at all must be
// reported, not panic or silently produce an empty letter.
func TestDerive_ParseError(t *testing.T) {
	r := mailbox.Raw{UID: 8, Data: []byte("this is not a valid header block at all\x00\x01\x02")}

	p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, &fakeStripper{}, 500))

	_, err := p.Derive(context.Background(), r)
	if err == nil {
		t.Fatalf("Derive returned nil error for unparseable input")
	}
	var parseErr *ParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
	if parseErr.UID != 8 {
		t.Errorf("ParseError.UID = %d, want 8", parseErr.UID)
	}
}

// TestDerive_FormatFlowed confirms the format=flowed Content-Type parameter
// on the text/plain part reaches Strip's formatFlowed argument.
func TestDerive_FormatFlowed(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{"flowed", "Content-Type: text/plain; format=flowed; charset=utf-8\r\n", true},
		{"not flowed", "Content-Type: text/plain; charset=utf-8\r\n", false},
		{"flowed, different case", "Content-Type: text/plain; Format=Flowed\r\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := rawLetter(9, rosa.Address, "flow test",
				tt.contentType+"Message-Id: <flow@example.com>\r\n",
				"one two three \nfour five six")

			stripper := &fakeStripper{}
			p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, stripper, 500))

			if _, err := p.Derive(context.Background(), r); err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if len(stripper.calls) != 1 {
				t.Fatalf("Strip called %d times, want 1", len(stripper.calls))
			}
			if stripper.calls[0].formatFlowed != tt.want {
				t.Errorf("formatFlowed = %v, want %v", stripper.calls[0].formatFlowed, tt.want)
			}
		})
	}
}

// TestDerive_DateSanityCheck exercises the full pipeline's use of
// sanityCheckedDate: a Date header far enough from INTERNALDATE must not
// reach the wire as truth.
func TestDerive_DateSanityCheck(t *testing.T) {
	internal := time.Date(2026, 1, 7, 16, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		dateHeader string // full header line, or "" to omit it
		want       time.Time
	}{
		{
			name:       "normal skew: header date used",
			dateHeader: "Date: Wed, 7 Jan 2026 15:59:00 -0000\r\n",
			want:       time.Date(2026, 1, 7, 15, 59, 0, 0, time.UTC),
		},
		{
			name:       "wildly future: falls back to internal",
			dateHeader: "Date: Wed, 7 Jan 2030 11:00:00 -0000\r\n",
			want:       internal,
		},
		{
			name:       "wildly past: falls back to internal",
			dateHeader: "Date: Wed, 7 Jan 2010 11:00:00 -0000\r\n",
			want:       internal,
		},
		{
			name:       "missing: falls back to internal",
			dateHeader: "",
			want:       internal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := fmt.Sprintf(
				"From: %s\r\nTo: kid@example.com\r\nSubject: hi\r\n%s"+
					"Content-Type: text/plain; charset=utf-8\r\nMessage-Id: <date-%s@example.com>\r\n\r\nhello",
				rosa.Address, tt.dateHeader, strings.ReplaceAll(tt.name, " ", "-"),
			)
			r := mailbox.Raw{UID: 10, InternalDate: internal, Data: []byte(msg)}

			p := mustPipeline(t, testConfig(t, fakeAyllu{rosa.Address: rosa}, &fakeStripper{}, 500))
			letter, err := p.Derive(context.Background(), r)
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if got := time.Unix(letter.Date, 0).UTC(); !got.Equal(tt.want) {
				t.Errorf("Date = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	valid := testConfig(t, fakeAyllu{}, &fakeStripper{}, 500)

	tests := []struct {
		name string
		cfg  Config
	}{
		{"nil ayllu", Config{Ayllu: nil, Strip: valid.Strip, MaxLetterChars: 500}},
		{"nil strip", Config{Ayllu: valid.Ayllu, Strip: nil, MaxLetterChars: 500}},
		{"zero max chars", Config{Ayllu: valid.Ayllu, Strip: valid.Strip, MaxLetterChars: 0}},
		{"negative max chars", Config{Ayllu: valid.Ayllu, Strip: valid.Strip, MaxLetterChars: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Errorf("New(%+v) returned nil error, want one", tt.cfg)
			}
		})
	}
}
