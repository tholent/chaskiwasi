package mailbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
			// Wrong credentials is a configuration bug, not an outage:
			// don't label it unreachable, and don't retry it.
			return fmt.Errorf("mailbox: smtp auth: %w", err)
		}
	}

	if err := c.SendMail(from, to, bytes.NewReader(msg)); err != nil {
		if isTransportErr(ctx, err) {
			return fmt.Errorf("mailbox: smtp send: %w", unreachable(err))
		}
		return fmt.Errorf("mailbox: smtp send (%d bytes): %w", len(msg), err)
	}

	s.cfg.logger().Info("mailbox: smtp send ok", "bytes", len(msg), "recipients", len(to))
	return c.Quit()
}

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

// IsPermanentReject reports whether err is a genuine SMTP rejection carrying a
// 5xx code — a permanent negative reply (RFC 5321 §4.2.1). The server will
// refuse this exchange no matter how often it is retried: a recipient address
// that no longer exists is the archetype. A 4xx reply ("try again later") and a
// transport failure are transient and are NOT permanent.
//
// This is the classification F-3 turns on: only a permanent rejection earns the
// device a terminal "undeliverable" ack, so a dead address is surfaced to the
// child once instead of being retried on every sync forever.
func IsPermanentReject(err error) bool {
	var smtpErr *smtp.SMTPError
	if !errors.As(err, &smtpErr) {
		return false
	}
	return smtpErr.Code >= 500 && smtpErr.Code < 600
}
