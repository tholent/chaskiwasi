//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mailFixture drives maddy through its own CLI, which is what deploy/README.md
// documents for seeding exact IMAP state.
//
// It reads and writes the mail store directly rather than over IMAP on
// purpose. The IMAP client under test lives inside Wasi; a suite that also
// talked IMAP would be asserting one emersion/go-imap client against another,
// and a shared bug would cancel out. Going around the protocol to check the
// result is what makes the assertion independent of it.
type mailFixture struct{ s *stack }

// maddyCmd runs one `maddy` subcommand inside the maddy container.
func (m *mailFixture) maddyCmd(t testing.TB, stdin []byte, args ...string) []byte {
	t.Helper()
	return m.s.exec(t, "maddy", stdin, append([]string{"maddy"}, args...)...)
}

// resetAccounts returns every mailbox in the fixture to empty.
//
// It deletes and recreates the storage accounts rather than deleting messages,
// for two reasons. maddy refuses to delete INBOX ("DeleteMailbox: can't delete
// INBOX"), so there is no per-folder way to reset it; and account recreation
// is also what rolls UIDVALIDITY, which V-21 needs and which nothing else in
// this fixture can produce.
func (m *mailFixture) resetAccounts(t testing.TB) {
	t.Helper()
	for _, account := range []string{childAddress, relativeAddress, strangerAddress} {
		m.maddyCmd(t, nil, "imap-acct", "remove", "-y", account)
		m.maddyCmd(t, nil, "imap-acct", "create", account)
	}
	// Held is Wasi's quarantine folder (§5.1) and is not one of maddy's
	// defaults; INBOX, Junk and the rest come with the account.
	m.maddyCmd(t, nil, "imap-mboxes", "create", childAddress, heldFolder)
}

// add files a raw RFC 5322 message straight into a folder, returning its UID.
// This is the only inbound path the fixture has: maddy's submission endpoint
// enforces `authorize_sender`, so nothing can submit mail *as* a relative, and
// its port-25 endpoint applies SPF/DMARC/MX checks no .test domain can pass.
//
// It is not a shortcut past the code under test. maddy's CLI writes through
// the same storage the running server reads, so a message added this way
// raises the same IMAP unilateral update a delivery would: Wasi's IDLE
// goroutine sees it and files it as an arrival, doorbell and all.
func (m *mailFixture) add(t testing.TB, account, folder string, raw []byte) uint32 {
	t.Helper()
	out := m.maddyCmd(t, raw, "imap-msgs", "add", account, folder)
	uid, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32)
	if err != nil {
		t.Fatalf("parsing UID from `imap-msgs add` output %q: %v", out, err)
	}
	return uint32(uid)
}

var uidLine = regexp.MustCompile(`^UID (\d+):`)

// uids lists the UIDs in a folder, oldest first.
func (m *mailFixture) uids(t testing.TB, account, folder string) []uint32 {
	t.Helper()
	out := m.maddyCmd(t, nil, "imap-msgs", "list", "--uid", account, folder)

	var uids []uint32
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		match := uidLine.FindStringSubmatch(sc.Text())
		if match == nil {
			continue
		}
		uid, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil {
			t.Fatalf("parsing UID from %q: %v", sc.Text(), err)
		}
		uids = append(uids, uint32(uid))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning `imap-msgs list` output: %v", err)
	}
	return uids
}

// dump returns one message exactly as it is stored.
func (m *mailFixture) dump(t testing.TB, account, folder string, uid uint32) []byte {
	t.Helper()
	return m.maddyCmd(t, nil, "imap-msgs", "dump", "--uid", account, folder,
		strconv.FormatUint(uint64(uid), 10))
}

// stored is one message in the fixture, parsed far enough to assert on.
type stored struct {
	UID     uint32
	Raw     []byte
	Header  mail.Header
	Body    string
	Subject string
	From    string
}

// To returns the first To address, or "" if the header is absent or unparseable.
func (s stored) To() string {
	addrs, err := s.Header.AddressList("To")
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return addrs[0].Address
}

// messages returns every message in a folder, parsed.
func (m *mailFixture) messages(t testing.TB, account, folder string) []stored {
	t.Helper()
	var out []stored
	for _, uid := range m.uids(t, account, folder) {
		raw := m.dump(t, account, folder, uid)
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parsing %s uid %d in %s: %v", account, uid, folder, err)
		}
		body, err := decodeBody(msg)
		if err != nil {
			t.Fatalf("decoding %s uid %d in %s: %v", account, uid, folder, err)
		}
		s := stored{
			UID:     uid,
			Raw:     raw,
			Header:  msg.Header,
			Body:    body,
			Subject: decodeHeader(msg.Header.Get("Subject")),
		}
		if addrs, err := msg.Header.AddressList("From"); err == nil && len(addrs) > 0 {
			s.From = addrs[0].Address
		}
		out = append(out, s)
	}
	return out
}

// count returns how many messages sit in a folder. Used by every "nothing
// arrived" assertion, where the interesting value is zero.
func (m *mailFixture) count(t testing.TB, account, folder string) int {
	t.Helper()
	return len(m.uids(t, account, folder))
}

// waitForCount blocks until a folder holds exactly n messages, then returns
// them. Filing happens on an IDLE notification and on a per-sync
// reconciliation pass, so every assertion about where a message ended up is
// eventually-consistent by design (§5.1).
func (m *mailFixture) waitForCount(t testing.TB, account, folder string, n int, timeout time.Duration) []stored {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("%d message(s) in %s/%s", n, account, folder), func() error {
		if got := len(m.uids(t, account, folder)); got != n {
			return fmt.Errorf("have %d", got)
		}
		return nil
	})
	return m.messages(t, account, folder)
}

// holds reports whether a folder contains a message carrying mark, in its
// subject or its body. Nonce marks make this exact where a count would be
// ambiguous — "one message in Held" cannot tell *which* one.
func (m *mailFixture) holds(t testing.TB, account, folder, mark string) bool {
	t.Helper()
	for _, msg := range m.messages(t, account, folder) {
		if strings.Contains(msg.Subject, mark) || strings.Contains(msg.Body, mark) {
			return true
		}
	}
	return false
}

// waitForMark blocks until a folder holds a message carrying mark.
func (m *mailFixture) waitForMark(t testing.TB, account, folder, mark string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("the letter marked %s to reach %s/%s", mark, account, folder), func() error {
		if !m.holds(t, account, folder, mark) {
			return fmt.Errorf("not there yet")
		}
		return nil
	})
}

// decodeBody returns a message's body as text. The fixture only ever produces
// or receives single-part text/plain, so this handles quoted-printable — which
// Wasi's own submitter emits — and nothing more exotic.
func decodeBody(msg *mail.Message) (string, error) {
	raw, err := io.ReadAll(msg.Body)
	if err != nil {
		return "", fmt.Errorf("reading message body: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(msg.Header.Get("Content-Transfer-Encoding")), "quoted-printable") {
		return string(raw), nil
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return "", fmt.Errorf("decoding quoted-printable body: %w", err)
	}
	return string(decoded), nil
}

var headerDecoder = new(mime.WordDecoder)

// decodeHeader RFC 2047-decodes a header value, leaving it alone if it is not
// encoded. §6.2 requires non-ASCII subjects to be encoded on the way out, so
// asserting on one means decoding it first (V-3).
func decodeHeader(v string) string {
	decoded, err := headerDecoder.DecodeHeader(v)
	if err != nil {
		return v
	}
	return decoded
}

// letter builds a raw RFC 5322 message for the fixture to inject. Fields are
// written verbatim, deliberately: V-3 and V-4 need to put things in headers
// that a well-behaved mail client would not.
type letter struct {
	From      string
	To        string
	Subject   string // written as given; already-encoded values pass through
	MessageID string // without angle brackets
	Date      time.Time
	Body      string
}

func (l letter) bytes() []byte {
	date := l.Date
	if date.IsZero() {
		date = time.Now().UTC()
	}
	to := l.To
	if to == "" {
		to = childAddress
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", l.From)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", l.Subject)
	if l.MessageID != "" {
		fmt.Fprintf(&b, "Message-ID: <%s>\r\n", l.MessageID)
	}
	fmt.Fprintf(&b, "Date: %s\r\n", date.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(l.Body, "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
