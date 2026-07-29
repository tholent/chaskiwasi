package mailbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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
	s.from = from
	return nil
}

func (s *fakeSMTPSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	if s.backend.rejectTo != "" && to == s.backend.rejectTo {
		return &smtp.SMTPError{Code: 550, Message: "no such user"}
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
