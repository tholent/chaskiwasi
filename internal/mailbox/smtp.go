package mailbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// SMTPConfig configures an SMTPSubmitter. Credentials come from secrets,
// never wasi.toml (§3).
type SMTPConfig struct {
	// Addr is host:port for the submission server (e.g.
	// "smtp.fastmail.com:465").
	Addr     string
	Username string
	Password string

	TLSMode   TLSMode
	TLSConfig *tls.Config

	// DialTimeout bounds the TCP connect, separately from ctx, which bounds
	// the whole exchange (dial through QUIT). Default 10s.
	DialTimeout time.Duration

	// Dial, if set, replaces the default TLS/STARTTLS dial (see Config.Dial
	// on the IMAP side for why this exists — the same test-fixture reasoning
	// applies here).
	Dial func(ctx context.Context) (net.Conn, error)

	Logger *slog.Logger
}

func (c SMTPConfig) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return 10 * time.Second
}

func (c SMTPConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c SMTPConfig) tlsConfig() *tls.Config {
	var cfg *tls.Config
	if c.TLSConfig != nil {
		cfg = c.TLSConfig.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(c.Addr); err == nil {
			cfg.ServerName = host
		}
	}
	return cfg
}

// SMTPSubmitter is the Submitter implementation over
// github.com/emersion/go-smtp. Invariant I-3: every call here carries either
// a child-authored letter or an operational notice copy fixed in
// human-owned config (§7.5) — never anything else. This package doesn't
// enforce that; it is enforced by what callers choose to send.
//
// Unlike IMAPMailbox, there is no persistent connection to reconnect: SMTP
// submission is one connection per Send, dialed fresh each time and closed
// when it's done. That matches how it's used — a handful of sends per sync,
// not a long-lived session — and sidesteps idle-connection timeouts on the
// provider side.
type SMTPSubmitter struct {
	cfg SMTPConfig
}

var _ Submitter = (*SMTPSubmitter)(nil)

// NewSMTPSubmitter builds a Submitter.
func NewSMTPSubmitter(cfg SMTPConfig) *SMTPSubmitter {
	return &SMTPSubmitter{cfg: cfg}
}

// Send implements Submitter. msg must be a complete RFC 5322 message with
// CRLF line endings; this package does not construct or log its contents
// (I-1) — only the byte count and outcome are ever mentioned in logs.
func (s *SMTPSubmitter) Send(ctx context.Context, from string, to []string, msg []byte) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return fmt.Errorf("mailbox: smtp connect: %w", unreachable(err))
	}

	// go-smtp's Client has no context support. Enforce ctx cancellation by
	// closing the connection out from under a blocked command; every method
	// below returns promptly once that happens.
	abort := make(chan struct{})
	defer close(abort)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-abort:
		}
	}()

	c, err := s.negotiate(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mailbox: smtp negotiate: %w", unreachable(err))
	}
	defer c.Close()

	if s.cfg.Username != "" {
		if err := c.Auth(sasl.NewPlainClient("", s.cfg.Username, s.cfg.Password)); err != nil {
			// Wrong credentials is a configuration bug, not an outage: don't
			// label it unreachable. Note it is emphatically NOT undeliverable
			// either, even though the reply is a 5xx — see submit.
			return fmt.Errorf("mailbox: smtp auth: %w", err)
		}
	}

	if err := s.submit(ctx, c, from, to, msg); err != nil {
		return err
	}

	s.cfg.logger().Info("mailbox: smtp send ok", "bytes", len(msg), "recipients", len(to))
	return c.Quit()
}

// submit runs MAIL/RCPT/DATA by hand instead of calling go-smtp's
// Client.SendMail, which collapses all three phases into one undifferentiated
// error. Which phase a 5xx came from is exactly what decides whether a child's
// letter is dropped, so the phases cannot be collapsed here (§4.7 step 4,
// A.11):
//
//   - RCPT TO 5xx — the recipient address is dead. Permanent: ErrUndeliverable,
//     which earns the terminal `rejected_undeliverable` ack.
//   - DATA 5xx — the server refuses this message (too large, content
//     rejected). Also permanent for this letter: ErrUndeliverable.
//   - MAIL FROM 5xx — our own sender is refused (quota, sending disabled).
//   - AUTH 5xx — our own credentials are wrong (handled in Send).
//
// The last two are the server refusing *us*, not the letter. They are
// guardian-fixable config faults that hit every queued letter at once, so they
// stay plain errors and the letters are left unacked to be retried once the
// config is repaired. Treating them as permanent would drop the child's entire
// outbox because an app password got rotated — a silent, unrecoverable loss,
// which is the one outcome §4.7 refuses to buy.
func (s *SMTPSubmitter) submit(ctx context.Context, c *smtp.Client, from string, to []string, msg []byte) error {
	if err := c.Mail(from, nil); err != nil {
		return fmt.Errorf("mailbox: smtp mail from: %w", s.envelopeErr(ctx, err, false))
	}
	for _, addr := range to {
		if err := c.Rcpt(addr, nil); err != nil {
			return fmt.Errorf("mailbox: smtp rcpt to: %w", s.envelopeErr(ctx, err, true))
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailbox: smtp data: %w", s.envelopeErr(ctx, err, true))
	}
	if _, err := io.Copy(w, bytes.NewReader(msg)); err != nil {
		w.Close()
		// A write that dies mid-body is a broken connection, not a verdict on
		// the letter: retry it.
		return fmt.Errorf("mailbox: smtp data write (%d bytes): %w", len(msg), unreachable(err))
	}
	// The server's verdict on the message arrives at the end of DATA, not
	// before it, so this Close is the reply that matters.
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailbox: smtp data close (%d bytes): %w", len(msg), s.envelopeErr(ctx, err, true))
	}
	return nil
}

// envelopeErr classifies one envelope-phase failure. permanentIfRefused says
// whether a 5xx in this phase condemns the message (RCPT, DATA) or merely
// reports that the server is refusing us right now (MAIL FROM).
//
// The server's reply text is dropped rather than wrapped: a RCPT rejection
// routinely echoes the address back ("550 5.1.1 <rosa@example.com>: no such
// user"), and this error is logged by the sync handler, so keeping the text
// would put a contact address in the logs (I-2). Only the numeric codes
// survive, which is what an operator actually needs to tell 550 from 452.
func (s *SMTPSubmitter) envelopeErr(ctx context.Context, err error, permanentIfRefused bool) error {
	if isTransportErr(ctx, err) {
		return unreachable(err)
	}
	var smtpErr *smtp.SMTPError
	errors.As(err, &smtpErr) // always true: isTransportErr said so
	coded := fmt.Errorf("server refused with code %d%s", smtpErr.Code, formatEnhanced(smtpErr.EnhancedCode))
	if permanentIfRefused && isPermanentCode(smtpErr.Code) {
		return undeliverable(coded)
	}
	return coded
}

func formatEnhanced(c smtp.EnhancedCode) string {
	if c == (smtp.EnhancedCode{}) {
		return ""
	}
	return fmt.Sprintf(" (%d.%d.%d)", c[0], c[1], c[2])
}

// isPermanentCode reports whether code is a permanent negative reply (RFC 5321
// §4.2.1) — the server will refuse this exchange no matter how often it is
// retried. A 4xx ("try again later") is transient and is not permanent.
func isPermanentCode(code int) bool { return code >= 500 && code < 600 }

func (s *SMTPSubmitter) connect(ctx context.Context) (net.Conn, error) {
	if s.cfg.Dial != nil {
		return s.cfg.Dial(ctx)
	}
	d := &net.Dialer{Timeout: s.cfg.dialTimeout()}
	return d.DialContext(ctx, "tcp", s.cfg.Addr)
}

// negotiate wraps conn per cfg.TLSMode and returns a ready-to-authenticate
// smtp.Client. When cfg.Dial is set (test fixtures), the connection is
// assumed to already be whatever the fixture wants and is used as-is.
func (s *SMTPSubmitter) negotiate(conn net.Conn) (*smtp.Client, error) {
	if s.cfg.Dial != nil {
		return smtp.NewClient(conn), nil
	}
	if s.cfg.TLSMode == TLSStartTLS {
		return smtp.NewClientStartTLS(conn, s.cfg.tlsConfig())
	}
	tlsConn := tls.Client(conn, s.cfg.tlsConfig())
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	return smtp.NewClient(tlsConn), nil
}

// isTransportErr reports whether err looks like a broken connection (ctx
// cancelled, TCP reset, timeout) rather than the server sending a genuine
// SMTP-level rejection (*smtp.SMTPError), which is a real, non-retryable
// outcome — e.g. a rejected recipient — and must not be relabelled
// unreachable.
func isTransportErr(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var smtpErr *smtp.SMTPError
	return !errors.As(err, &smtpErr)
}
