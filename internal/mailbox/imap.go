package mailbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// TLSMode selects how the IMAP connection is secured.
type TLSMode int

const (
	// TLSImplicit wraps the TCP connection in TLS before any IMAP traffic —
	// what Fastmail requires on port 993. This is the zero value and the
	// default: an unconfigured Config never talks IMAP in the clear.
	TLSImplicit TLSMode = iota
	// TLSStartTLS connects in the clear and issues STARTTLS before LOGIN —
	// for providers that only offer the upgrade path (typically port 143).
	TLSStartTLS
)

// Config configures an IMAPMailbox. Credentials come from secrets, never
// wasi.toml (wasi-server-plan §3) — this package only consumes the already
// -resolved values.
type Config struct {
	// Addr is host:port for the IMAP server (e.g. "imap.fastmail.com:993").
	Addr     string
	Username string
	Password string
	// Inbox is the mailbox name to treat as INBOX. "INBOX" almost always.
	Inbox string

	TLSMode TLSMode
	// TLSConfig, if set, is cloned as the base for the connection's TLS
	// config (ServerName is filled in from Addr if empty). Nil is fine for
	// production; it exists so operators can pin a CA bundle if needed.
	TLSConfig *tls.Config

	// DialTimeout bounds the TCP connect. Default 10s.
	DialTimeout time.Duration
	// RetryBaseDelay / RetryMaxDelay bound the reconnect backoff (§4.1: the
	// caller's context is what ultimately caps total retry time, so the
	// sync handler can turn a prolonged outage into 503 + Retry-After
	// instead of hanging the request).
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration

	// Dial, if set, replaces the default TLS/STARTTLS dial and returns a
	// net.Conn on which IMAP framing can start immediately (any TLS already
	// negotiated). This exists so tests can point a Mailbox at an in-process
	// fixture without a certificate; production callers must leave it nil.
	Dial func(ctx context.Context) (net.Conn, error)

	Logger *slog.Logger
}

func (c Config) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return 10 * time.Second
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c Config) inbox() string {
	if c.Inbox != "" {
		return c.Inbox
	}
	return "INBOX"
}

func (c Config) tlsConfig() *tls.Config {
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

// IMAPMailbox is the Mailbox implementation over github.com/emersion/go-imap/v2.
//
// Nothing here caches messages (package doc, V-10). A single connection,
// guarded by mu, serves every method except Idle, which opens its own
// dedicated connection because IDLE blocks the connection it runs on for as
// long as it is active (§5.1).
type IMAPMailbox struct {
	cfg Config

	mu   sync.Mutex
	conn *imapclient.Client
}

var _ Mailbox = (*IMAPMailbox)(nil)

// NewIMAPMailbox builds a Mailbox. It does not connect; the first call does.
func NewIMAPMailbox(cfg Config) *IMAPMailbox {
	return &IMAPMailbox{cfg: cfg}
}

// dial performs one connection attempt: TCP (or the test Dial hook), TLS per
// cfg.TLSMode, LOGIN. It does not retry — retry and backoff live in
// withConn, which is what every exported method funnels through.
//
// opts carries per-connection imapclient.Options such as
// UnilateralDataHandler; these must be set at construction (the client has
// no setter), so Idle's dedicated connection passes its own. Pass nil for
// the default.
func (m *IMAPMailbox) dial(ctx context.Context, opts *imapclient.Options) (*imapclient.Client, error) {
	if opts == nil {
		opts = &imapclient.Options{}
	}

	var (
		conn net.Conn
		err  error
	)
	if m.cfg.Dial != nil {
		conn, err = m.cfg.Dial(ctx)
	} else {
		d := &net.Dialer{Timeout: m.cfg.dialTimeout()}
		conn, err = d.DialContext(ctx, "tcp", m.cfg.Addr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	var c *imapclient.Client
	switch {
	case m.cfg.Dial != nil:
		// Fixture hook: the caller already handed us a ready connection.
		c = imapclient.New(conn, opts)
	case m.cfg.TLSMode == TLSStartTLS:
		startOpts := *opts
		startOpts.TLSConfig = m.cfg.tlsConfig()
		c, err = imapclient.NewStartTLS(conn, &startOpts)
		if err != nil {
			return nil, fmt.Errorf("starttls: %w", err)
		}
	default: // TLSImplicit
		tlsConn := tls.Client(conn, m.cfg.tlsConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		c = imapclient.New(tlsConn, opts)
	}

	if err := c.Login(m.cfg.Username, m.cfg.Password).Wait(); err != nil {
		c.Close()
		// Never wrap the password into the error chain; go-imap's Login
		// error only ever carries the server's own response text.
		return nil, fmt.Errorf("login: %w", err)
	}
	return c, nil
}

// isClosed reports whether the client's underlying connection has died —
// the signal withConn uses to tell "IMAP unreachable" (reconnect and retry)
// apart from an ordinary protocol-level NO/BAD response on a live connection
// (a real error, not a reachability problem, so it must not be retried or
// relabelled).
func isClosed(c *imapclient.Client) bool {
	select {
	case <-c.Closed():
		return true
	default:
		return false
	}
}

// withConn runs fn against a live connection, reconnecting with backoff on
// dial failure or a connection that died mid-command, bounded by ctx. When
// ctx expires without a working connection, the caller gets ErrUnreachable
// so it can turn that into 503 + Retry-After (§4.1) instead of blocking the
// request indefinitely.
func (m *IMAPMailbox) withConn(ctx context.Context, fn func(*imapclient.Client) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	bo := newBackoff(m.cfg.RetryBaseDelay, m.cfg.RetryMaxDelay)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return unreachable(lastErr, err)
		}

		if m.conn == nil {
			c, err := m.dial(ctx, nil)
			if err != nil {
				lastErr = err
				m.cfg.logger().Warn("mailbox: imap connect failed, retrying", "addr", m.cfg.Addr, "err", err)
				if !bo.wait(ctx) {
					return unreachable(lastErr, ctx.Err())
				}
				continue
			}
			m.conn = c
			bo = newBackoff(m.cfg.RetryBaseDelay, m.cfg.RetryMaxDelay)
		}

		err := fn(m.conn)
		if err == nil {
			return nil
		}
		if isClosed(m.conn) {
			m.conn.Close()
			m.conn = nil
			lastErr = err
			m.cfg.logger().Warn("mailbox: imap connection lost, reconnecting", "err", err)
			if !bo.wait(ctx) {
				return unreachable(lastErr, ctx.Err())
			}
			continue
		}
		// The connection is still alive; this is a real protocol error
		// (bad login state, no such mailbox, ...), not an outage.
		return fmt.Errorf("mailbox: %w", err)
	}
}

// selectMailbox selects folder on c, wrapping the error with folder context.
func selectMailbox(c *imapclient.Client, folder string, readOnly bool) (*imap.SelectData, error) {
	data, err := c.Select(folder, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", folder, err)
	}
	return data, nil
}

// bodySection is BODY[] (whole message), peeked so filing/derivation never
// flips \Seen as a side effect of reading.
var bodySection = &imap.FetchItemBodySection{Peek: true}

func fetchOptions() *imap.FetchOptions {
	return &imap.FetchOptions{
		UID:          true,
		InternalDate: true,
		BodySection:  []*imap.FetchItemBodySection{bodySection},
	}
}

func toRaw(buf *imapclient.FetchMessageBuffer) Raw {
	return Raw{
		UID:          uint32(buf.UID),
		InternalDate: buf.InternalDate,
		Data:         buf.FindBodySection(bodySection),
	}
}

// UIDValidity implements Mailbox (§4.4).
func (m *IMAPMailbox) UIDValidity(ctx context.Context) (uint32, error) {
	var uidValidity uint32
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		data, err := selectMailbox(c, m.cfg.inbox(), true)
		if err != nil {
			return err
		}
		uidValidity = data.UIDValidity
		return nil
	})
	if err != nil {
		return 0, err
	}
	return uidValidity, nil
}

// FetchAbove implements Mailbox (§5.2). Results stream off the wire and stop
// as soon as max messages have been collected, so a huge backlog above uid
// does not force a full download when the caller only wants the next handful.
func (m *IMAPMailbox) FetchAbove(ctx context.Context, uid uint32, max int) ([]Raw, error) {
	var out []Raw
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		out = nil
		if _, err := selectMailbox(c, m.cfg.inbox(), true); err != nil {
			return err
		}
		if max <= 0 {
			return nil
		}

		set := imap.UIDSet{imap.UIDRange{Start: imap.UID(uid) + 1, Stop: 0}}
		fetchCmd := c.Fetch(set, fetchOptions())

		var ferr error
		for len(out) < max {
			msg := fetchCmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				ferr = err
				break
			}
			out = append(out, toRaw(buf))
		}
		if cerr := fetchCmd.Close(); cerr != nil && ferr == nil {
			ferr = cerr
		}
		return ferr
	})
	if err != nil {
		return nil, fmt.Errorf("mailbox: fetch above uid %d: %w", uid, err)
	}
	return out, nil
}

// Recent implements Mailbox (§4.4 window resync): the most recent n
// messages by sequence number, in ascending (oldest-first) order — the same
// UID-ascending order FetchAbove uses, so callers can treat both the same
// way.
func (m *IMAPMailbox) Recent(ctx context.Context, n int) ([]Raw, error) {
	var out []Raw
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		out = nil
		data, err := selectMailbox(c, m.cfg.inbox(), true)
		if err != nil {
			return err
		}
		if n <= 0 || data.NumMessages == 0 {
			return nil
		}

		start := uint32(1)
		if data.NumMessages > uint32(n) {
			start = data.NumMessages - uint32(n) + 1
		}
		set := imap.SeqSet{imap.SeqRange{Start: start, Stop: data.NumMessages}}
		fetchCmd := c.Fetch(set, fetchOptions())

		var ferr error
		for {
			msg := fetchCmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				ferr = err
				break
			}
			out = append(out, toRaw(buf))
		}
		if cerr := fetchCmd.Close(); cerr != nil && ferr == nil {
			ferr = cerr
		}
		return ferr
	})
	if err != nil {
		return nil, fmt.Errorf("mailbox: recent %d: %w", n, err)
	}
	return out, nil
}

// List implements Mailbox. The guardian UI reads Held live over IMAP (§8),
// so this always goes to the wire — there is no mirror to fall stale.
func (m *IMAPMailbox) List(ctx context.Context, folder string) ([]Raw, error) {
	var out []Raw
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		out = nil
		data, err := selectMailbox(c, folder, true)
		if err != nil {
			return err
		}
		if data.NumMessages == 0 {
			return nil
		}

		set := imap.SeqSet{imap.SeqRange{Start: 1, Stop: data.NumMessages}}
		fetchCmd := c.Fetch(set, fetchOptions())

		var ferr error
		for {
			msg := fetchCmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				ferr = err
				break
			}
			out = append(out, toRaw(buf))
		}
		if cerr := fetchCmd.Close(); cerr != nil && ferr == nil {
			ferr = cerr
		}
		return ferr
	})
	if err != nil {
		return nil, fmt.Errorf("mailbox: list %s: %w", folder, err)
	}
	return out, nil
}

// Move implements Mailbox: quarantine to Held (§5.1) and release back to
// INBOX (§8) are both this one call from the caller's side.
func (m *IMAPMailbox) Move(ctx context.Context, folder string, uid uint32, dest string) error {
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		if _, err := selectMailbox(c, folder, false); err != nil {
			return err
		}
		set := imap.UIDSetNum(imap.UID(uid))
		if _, err := c.Move(set, dest).Wait(); err != nil {
			return fmt.Errorf("move uid %d: %w", uid, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mailbox: move %s/%d -> %s: %w", folder, uid, dest, err)
	}
	return nil
}

// Append implements Mailbox: notice letters into INBOX from c_sys (§7.4).
// at becomes INTERNALDATE; a zero Time lets the server pick "now", which is
// what every real caller wants (INTERNALDATE is a receipt time, not
// something worth back-dating).
func (m *IMAPMailbox) Append(ctx context.Context, folder string, msg []byte, at time.Time) error {
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		opts := &imap.AppendOptions{}
		if !at.IsZero() {
			opts.Time = at
		}
		cmd := c.Append(folder, int64(len(msg)), opts)
		if _, err := cmd.Write(msg); err != nil {
			cmd.Close()
			return fmt.Errorf("append write: %w", err)
		}
		if err := cmd.Close(); err != nil {
			return fmt.Errorf("append close: %w", err)
		}
		if _, err := cmd.Wait(); err != nil {
			return fmt.Errorf("append: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mailbox: append %s: %w", folder, err)
	}
	return nil
}

// Idle implements Mailbox. Unlike withConn's callers, Idle is meant to run
// for the life of the process: it never surfaces ErrUnreachable, it just
// keeps reconnecting with backoff, and only returns when ctx is cancelled.
func (m *IMAPMailbox) Idle(ctx context.Context, notify chan<- struct{}) error {
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages == nil {
					return
				}
				select {
				case notify <- struct{}{}:
				default: // a pending signal is enough; this is a doorbell, not a queue
				}
			},
		},
	}

	bo := newBackoff(m.cfg.RetryBaseDelay, m.cfg.RetryMaxDelay)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		c, err := m.dial(ctx, opts)
		if err != nil {
			m.cfg.logger().Warn("mailbox: idle connect failed, retrying", "addr", m.cfg.Addr, "err", err)
			if !bo.wait(ctx) {
				return ctx.Err()
			}
			continue
		}
		bo = newBackoff(m.cfg.RetryBaseDelay, m.cfg.RetryMaxDelay)

		err = m.runIdle(ctx, c)
		c.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			m.cfg.logger().Warn("mailbox: idle connection lost, reconnecting", "err", err)
		}
		if !bo.wait(ctx) {
			return ctx.Err()
		}
	}
}

// runIdle drives one IDLE session on its own connection until ctx is
// cancelled or the connection drops. This is a NOTIFICATION path, not an
// ingest path (§5.1): every signal on notify just means "something happened
// in INBOX, go look" — filing still reconciles at startup and at the top of
// every sync (V-15), so correctness never depends on IDLE staying connected.
func (m *IMAPMailbox) runIdle(ctx context.Context, c *imapclient.Client) error {
	// A fresh EXAMINE-only select re-establishes IMAP4rev2's implicit
	// "report changes from here" baseline on this connection; the
	// UnilateralDataHandler installed at dial time fires for subsequent
	// EXISTS updates.
	if _, err := c.Select(m.cfg.inbox(), &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return fmt.Errorf("select %s: %w", m.cfg.inbox(), err)
	}

	idleCmd, err := c.Idle()
	if err != nil {
		return fmt.Errorf("idle: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- idleCmd.Wait() }()

	select {
	case <-ctx.Done():
		idleCmd.Close()
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Close implements Mailbox.
func (m *IMAPMailbox) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		return nil
	}
	err := m.conn.Close()
	m.conn = nil
	return err
}
