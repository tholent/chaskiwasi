package mailbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// fakeSMTPBackend is an in-process SMTP server (go-smtp's own server side)
// used as a fixture — no maddy, no network beyond loopback. It records what
// was submitted so tests can assert on envelope fields without ever needing
// to inspect message content by scraping logs (I-1 stays true for the test
// fixture too: nothing here logs the body).
type fakeSMTPBackend struct {
	mu       sync.Mutex
	messages []recordedMessage
	rejectTo string // if set, RCPT to this address is refused
	// Per-phase refusals, so a test can prove that *which phase* replied
	// decides whether a letter is condemned or retried (F-3, A.11).
	rejectToCode   int // code for rejectTo; 550 when unset
	rejectFromCode int // if set, MAIL FROM is refused with this code
	rejectDataCode int // if set, DATA is refused with this code
}

// refusal builds the SMTP error the fixture replies with. code 0 means "no
// refusal"; the caller checks that first.
func refusal(code int, msg string) *smtp.SMTPError {
	return &smtp.SMTPError{Code: code, Message: msg}
}

type recordedMessage struct {
	from string
	to   []string
	data []byte
}

func (b *fakeSMTPBackend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &fakeSMTPSession{backend: b}, nil
}

type fakeSMTPSession struct {
	backend *fakeSMTPBackend
	authed  bool
	from    string
	to      []string
}

func (s *fakeSMTPSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *fakeSMTPSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username != testUsername || password != testPassword {
			return errors.New("invalid credentials")
		}
		s.authed = true
		return nil
	}), nil
}

func (s *fakeSMTPSession) Mail(from string, opts *smtp.MailOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	if code := s.backend.rejectFromCode; code != 0 {
		return refusal(code, "sending disabled for this account")
	}
	s.from = from
	return nil
}

func (s *fakeSMTPSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	if s.backend.rejectTo != "" && to == s.backend.rejectTo {
		code := s.backend.rejectToCode
		if code == 0 {
			code = 550
		}
		// Deliberately echoes the address back, the way real servers do: the
		// I-2 test below depends on this text existing to prove it is dropped.
		return refusal(code, "<"+to+">: no such user")
	}
	s.to = append(s.to, to)
	return nil
}

func (s *fakeSMTPSession) Data(r io.Reader) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if code := s.backend.rejectDataCode; code != 0 {
		return refusal(code, "message rejected")
	}
	s.backend.mu.Lock()
	s.backend.messages = append(s.backend.messages, recordedMessage{from: s.from, to: append([]string(nil), s.to...), data: data})
	s.backend.mu.Unlock()
	return nil
}

func (s *fakeSMTPSession) Reset()        {}
func (s *fakeSMTPSession) Logout() error { return nil }

// newTestSMTPServer starts the fixture and returns an SMTPConfig wired to
// dial it in the clear (production always uses TLS; the Dial hook exists
// specifically so tests don't need a trusted certificate — see Config.Dial's
// doc comment on the IMAP side for the same reasoning).
func newTestSMTPServer(t *testing.T, backend *fakeSMTPBackend) SMTPConfig {
	t.Helper()

	s := smtp.NewServer(backend)
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	s.ReadTimeout = 5 * time.Second
	s.WriteTimeout = 5 * time.Second

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go s.Serve(ln)
	t.Cleanup(func() {
		s.Close()
		ln.Close()
	})

	return SMTPConfig{
		Addr:     ln.Addr().String(),
		Username: testUsername,
		Password: testPassword,
		Logger:   silentLogger(),
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		},
	}
}

func TestSMTPSend(t *testing.T) {
	backend := &fakeSMTPBackend{}
	cfg := newTestSMTPServer(t, backend)
	sub := NewSMTPSubmitter(cfg)

	msg := []byte("Message-Id: <l-1@example.com>\r\nSubject: hi\r\n\r\nHello!\r\n")
	err := sub.Send(context.Background(), "c_sys@wasi.local", []string{"rosa@example.com"}, msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.messages) != 1 {
		t.Fatalf("backend recorded %d messages, want 1", len(backend.messages))
	}
	got := backend.messages[0]
	if got.from != "c_sys@wasi.local" {
		t.Errorf("From = %q, want c_sys@wasi.local", got.from)
	}
	if len(got.to) != 1 || got.to[0] != "rosa@example.com" {
		t.Errorf("To = %v, want [rosa@example.com]", got.to)
	}
	if !bytes.Contains(got.data, []byte("Hello!")) {
		t.Errorf("server did not receive the message body")
	}
}

func TestSMTPSend_RejectedRecipient(t *testing.T) {
	backend := &fakeSMTPBackend{rejectTo: "nobody@example.com"}
	cfg := newTestSMTPServer(t, backend)
	sub := NewSMTPSubmitter(cfg)

	msg := []byte("Subject: hi\r\n\r\nHello!\r\n")
	err := sub.Send(context.Background(), "c_sys@wasi.local", []string{"nobody@example.com"}, msg)
	if err == nil {
		t.Fatal("Send succeeded, want rejection")
	}
	// A real SMTP rejection is not a reachability problem: it must not be
	// retried and must not be mislabelled ErrUnreachable.
	if errors.Is(err, ErrUnreachable) {
		t.Errorf("Send error wrongly classified as ErrUnreachable: %v", err)
	}
	if !errors.Is(err, ErrUndeliverable) {
		t.Errorf("a 5xx at RCPT TO must be ErrUndeliverable, got: %v", err)
	}
}

// TestSMTPSend_PhaseDecidesPermanence is the regression guard for the F-3 bug:
// a 5xx is only permanent when it condemns the *message*. A 5xx that refuses
// *us* — a rotated app password at AUTH, a disabled sender at MAIL FROM — is a
// guardian-fixable config fault that hits every queued letter at once, so it
// must stay retryable. Classifying it undeliverable would terminally reject the
// child's whole outbox and lose every letter in it (§4.7, A.11).
func TestSMTPSend_PhaseDecidesPermanence(t *testing.T) {
	const dead = "nobody@example.com"

	tests := []struct {
		name string
		// Phase config only, not a whole fakeSMTPBackend: that carries a mutex
		// and a table of them would copy the lock (go vet catches it).
		rejectToCode   int
		rejectFromCode int
		rejectDataCode int
		badPassword    bool
		undeliverable  bool
	}{
		{
			name:          "5xx at RCPT TO condemns the letter",
			rejectToCode:  550,
			undeliverable: true,
		},
		{
			name:           "5xx at DATA condemns the letter",
			rejectDataCode: 552,
			undeliverable:  true,
		},
		{
			name:          "4xx at RCPT TO is transient",
			rejectToCode:  451,
			undeliverable: false,
		},
		{
			name:           "4xx at DATA is transient",
			rejectDataCode: 452,
			undeliverable:  false,
		},
		{
			name:           "5xx at MAIL FROM refuses us, not the letter",
			rejectFromCode: 550,
			undeliverable:  false,
		},
		{
			name:          "5xx at AUTH refuses us, not the letter",
			badPassword:   true,
			undeliverable: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeSMTPBackend{
				rejectToCode:   tc.rejectToCode,
				rejectFromCode: tc.rejectFromCode,
				rejectDataCode: tc.rejectDataCode,
			}
			if tc.rejectToCode != 0 {
				backend.rejectTo = dead
			}
			cfg := newTestSMTPServer(t, backend)
			if tc.badPassword {
				cfg.Password = "wrong"
			}
			sub := NewSMTPSubmitter(cfg)

			err := sub.Send(context.Background(), "c_sys@wasi.local", []string{dead}, []byte("Subject: hi\r\n\r\nHi\r\n"))
			if err == nil {
				t.Fatal("Send succeeded, want a refusal")
			}
			if got := errors.Is(err, ErrUndeliverable); got != tc.undeliverable {
				t.Fatalf("errors.Is(err, ErrUndeliverable) = %v, want %v (err: %v)", got, tc.undeliverable, err)
			}
			// None of these is a reachability failure; mislabelling one would
			// abort the whole sync with a 503 (§4.1).
			if errors.Is(err, ErrUnreachable) {
				t.Errorf("refusal wrongly classified as ErrUnreachable: %v", err)
			}
		})
	}
}

// TestSMTPSend_RejectionErrorCarriesNoAddress covers I-2: a RCPT rejection
// routinely echoes the address back, and the sync handler logs this error, so
// the server's reply text must not survive into the error string.
func TestSMTPSend_RejectionErrorCarriesNoAddress(t *testing.T) {
	const dead = "nobody@example.com"
	backend := &fakeSMTPBackend{rejectTo: dead, rejectToCode: 550}
	cfg := newTestSMTPServer(t, backend)
	sub := NewSMTPSubmitter(cfg)

	err := sub.Send(context.Background(), "c_sys@wasi.local", []string{dead}, []byte("Subject: hi\r\n\r\nHi\r\n"))
	if err == nil {
		t.Fatal("Send succeeded, want rejection")
	}
	if strings.Contains(err.Error(), dead) || strings.Contains(err.Error(), "example.com") {
		t.Fatalf("I-2: rejection error leaks the recipient address: %q", err)
	}
	// The operator still needs to tell a 550 from a 452.
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("rejection error dropped the SMTP code, leaving nothing to debug with: %q", err)
	}
}

func TestSMTPSend_BadCredentials(t *testing.T) {
	backend := &fakeSMTPBackend{}
	cfg := newTestSMTPServer(t, backend)
	cfg.Password = "wrong"
	sub := NewSMTPSubmitter(cfg)

	err := sub.Send(context.Background(), "c_sys@wasi.local", []string{"rosa@example.com"}, []byte("Subject: hi\r\n\r\nHi\r\n"))
	if err == nil {
		t.Fatal("Send succeeded with bad credentials, want error")
	}
	if errors.Is(err, ErrUnreachable) {
		t.Errorf("bad credentials wrongly classified as ErrUnreachable: %v", err)
	}
}

func TestSMTPSend_Unreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	sub := NewSMTPSubmitter(SMTPConfig{
		Addr:        addr,
		Username:    testUsername,
		Password:    testPassword,
		DialTimeout: 200 * time.Millisecond,
		Logger:      silentLogger(),
	})

	err = sub.Send(context.Background(), "c_sys@wasi.local", []string{"rosa@example.com"}, []byte("Subject: hi\r\n\r\nHi\r\n"))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Send error = %v, want errors.Is(err, ErrUnreachable)", err)
	}
}

func TestSMTPSend_ContextCancelled(t *testing.T) {
	backend := &fakeSMTPBackend{}
	cfg := newTestSMTPServer(t, backend)
	sub := NewSMTPSubmitter(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Send even dials

	err := sub.Send(ctx, "c_sys@wasi.local", []string{"rosa@example.com"}, []byte("Subject: hi\r\n\r\nHi\r\n"))
	if err == nil {
		t.Fatal("Send succeeded despite a cancelled context")
	}
}
