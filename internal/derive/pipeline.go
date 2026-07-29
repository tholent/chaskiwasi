package derive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/mail"
	"net/textproto"
	"strings"

	"github.com/jhillyerd/enmime/v2"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/graphemes"
	"github.com/tholent/chaskiwasi/internal/letterid"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
	"github.com/tholent/chaskiwasi/internal/strip"
	"github.com/tholent/chaskiwasi/internal/subject"
)

// mimeParser is built once and reused for every message. DisableTextConversion
// stops enmime from down-converting an HTML body to text when text/plain is
// absent — that down-conversion is itself a form of rendering HTML, which
// §5.2 forbids outright ("HTML is never rendered"). The parser carries no
// per-message state, so sharing one instance across calls does not threaten
// determinism (test V-9).
var mimeParser = enmime.NewParser(enmime.DisableTextConversion(true))

// Stripper is the one method of *strip.Client that Derive depends on:
// something that turns raw text into a stripped result and never fails the
// caller (§5.3 — see strip's package doc for why that property matters).
// Declared here, rather than depended on as the concrete type, so tests can
// substitute a stub without standing up an HTTP server.
type Stripper interface {
	Strip(ctx context.Context, text string, formatFlowed bool) strip.Result
}

// AylluResolver is the one ayllu.Store method Derive needs. Always full-table
// resolution — tombstones and past addresses included, never ResolveActive —
// because derivation renders a contact's entire history even after they are
// deactivated or re-addressed; the active/inactive decision belongs to
// filing and sending, not to read-time rendering (§7.2, §5.2).
type AylluResolver interface {
	Resolve(addr string) (ayllu.Contact, bool)
}

// Config configures a Pipeline.
type Config struct {
	// Ayllu resolves From addresses against the full contact table (§7.2).
	Ayllu AylluResolver
	// Strip removes quoted tails and signatures, live service or fallback
	// (§5.3).
	Strip Stripper
	// MaxLetterChars is sync.max_letter_chars from wasi.toml (§13, §4.6): the
	// grapheme cap the body is truncated to. Must be positive.
	MaxLetterChars int

	Logger *slog.Logger
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Pipeline is the concrete Deriver described in the package doc: enmime
// parse -> resolve -> strip -> normalise subject -> truncate (§5.2).
type Pipeline struct {
	cfg Config
}

var _ Deriver = (*Pipeline)(nil)

// New builds a Pipeline from cfg, rejecting a configuration that would make
// every derived letter wrong in the same way (a missing dependency, a
// non-positive character cap) rather than deferring the failure to the first
// real message.
func New(cfg Config) (*Pipeline, error) {
	if cfg.Ayllu == nil {
		return nil, errors.New("derive: Config.Ayllu is required")
	}
	if cfg.Strip == nil {
		return nil, errors.New("derive: Config.Strip is required")
	}
	if cfg.MaxLetterChars <= 0 {
		return nil, fmt.Errorf("derive: Config.MaxLetterChars must be positive, got %d", cfg.MaxLetterChars)
	}
	return &Pipeline{cfg: cfg}, nil
}

// ParseError reports that enmime could not parse a raw message at all. UID
// is included so the caller can log which message failed; the raw bytes
// never are (I-1).
type ParseError struct {
	UID uint32
	Err error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("derive: uid %d: parse: %v", e.UID, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// UnresolvedSenderError reports that a message's From address does not
// resolve to any contact, active or tombstoned. §5.2 says "after
// reconciliation, everything left in INBOX resolves" — but Derive must not
// assume the caller upheld that. When it doesn't hold, this is something the
// caller has to act on (log and skip, most likely), not a letter to render
// with a blank name and not a letter to drop silently. UID and LetterID are
// safe to log (I-2 covers addresses, not ids); the address behind the
// failure is not carried on this error at all.
type UnresolvedSenderError struct {
	UID      uint32
	LetterID string
}

func (e *UnresolvedSenderError) Error() string {
	return fmt.Sprintf("derive: uid %d (id %s): sender does not resolve to any contact", e.UID, e.LetterID)
}

// Derive implements Deriver (§5.2).
func (p *Pipeline) Derive(ctx context.Context, r mailbox.Raw) (protocol.Letter, error) {
	env, err := mimeParser.ReadEnvelope(bytes.NewReader(r.Data))
	if err != nil {
		return protocol.Letter{}, &ParseError{UID: r.UID, Err: err}
	}
	header := env.Root.Header

	id := p.letterID(header, r.Data)

	fromAddrs, err := env.AddressList("From")
	if err != nil || len(fromAddrs) == 0 {
		p.cfg.logger().Warn("derive: message has no usable From address", "uid", r.UID, "letter_id", id)
		return protocol.Letter{}, &UnresolvedSenderError{UID: r.UID, LetterID: id}
	}
	contact, ok := p.cfg.Ayllu.Resolve(fromAddrs[0].Address)
	if !ok {
		p.cfg.logger().Warn("derive: sender does not resolve to any contact", "uid", r.UID, "letter_id", id)
		return protocol.Letter{}, &UnresolvedSenderError{UID: r.UID, LetterID: id}
	}

	bodyText, formatFlowed := textPlainPart(env)

	// A message with no text/plain part (or an empty one) produces an empty
	// body rather than an error or synthesised text: §5.2 says HTML is never
	// rendered, so there is nothing honest to show from an HTML-only letter,
	// and the letter must not vanish — subject, sender and date still carry
	// real information the device can show. Skipping Strip here is not just
	// an optimisation: stripping empty text is a no-op on both the live
	// service and the fallback, so the zero-value strip.Result already
	// matches what a real call would return.
	var stripResult strip.Result
	if bodyText != "" {
		stripResult = p.cfg.Strip.Strip(ctx, bodyText, formatFlowed)
	}

	truncatedBody, truncated := graphemes.Truncate(stripResult.Body, p.cfg.MaxLetterChars)

	headerDate, dateErr := env.Date()
	date := sanityCheckedDate(headerDate, dateErr == nil, r.InternalDate)

	return protocol.Letter{
		ID:        id,
		ContactID: contact.ID,
		Subject:   subject.NormalizeInbound(header.Get("Subject")),
		Date:      date.Unix(),
		Body:      truncatedBody,
		Trimmed:   stripResult.Trimmed,
		Truncated: truncated,
		Degraded:  stripResult.Degraded,
	}, nil
}

// letterID computes the wire letter id (§4.5): from Message-ID when present,
// or the fallback derived from From + Date + up to 1 KB of the raw body when
// it is absent — rare, but legal per RFC 5322, and §5.2 forbids dropping
// such a letter. header.Get returns header values exactly as they appeared
// on the wire (folding aside): letterid.FromMessageID depends on that raw
// form, since the hash — not the header itself — is what may ever reach the
// device.
func (p *Pipeline) letterID(header textproto.MIMEHeader, rawData []byte) string {
	if raw := header.Get("Message-Id"); raw != "" {
		return letterid.FromMessageID(raw)
	}
	return letterid.FromFallback(header.Get("From"), header.Get("Date"), rawBodyPrefix(rawData))
}

// rawBodyPrefix returns up to 1 KB of data's body, split from its headers
// the same way net/mail does. Feeds letterid.FromFallback's "first 1 KB of
// the raw body" (§4.5) when Message-ID is absent. A message net/mail cannot
// even split into header/body contributes an empty prefix instead of
// failing derivation over a case §5.2 already treats as legal and rare.
func rawBodyPrefix(data []byte) []byte {
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(msg.Body, 1024))
	return body
}

// textPlainPart finds the message's text/plain part (§5.2 step 1):
// breadth-first over the MIME tree, skipping attachments, so a single-part
// text message (where the part IS the root) and a multipart/alternative
// body are both found by the same rule enmime itself uses to pick a body
// part. formatFlowed comes from that part's own Content-Type parameter
// (§11.1) — RFC 3676's format=flowed is a per-part property, not a
// per-message one.
//
// A message with more than one qualifying part (malformed mail, or a
// genuine multipart/mixed with more than one bare text/plain body) yields
// whichever one BreadthMatchFirst reaches first. That's deterministic for a
// given message, which is all §5.2's determinism claim requires — there is
// no spec-mandated way to pick among several text/plain bodies, and
// attachments are out of scope for v1 regardless.
func textPlainPart(env *enmime.Envelope) (text string, formatFlowed bool) {
	if env.Root == nil {
		return "", false
	}
	part := env.Root.BreadthMatchFirst(func(p *enmime.Part) bool {
		return p.ContentType == "text/plain" && p.Disposition != "attachment"
	})
	if part == nil {
		return "", false
	}

	// part.ContentTypeParams is populated only when *building* a Part with
	// enmime's builder API, never when reading one — it stays an empty map
	// for anything ReadEnvelope produced. The real Content-Type parameters
	// of a parsed part live in its raw header, so format=flowed is read
	// straight from there with the stdlib parser (no need for enmime's more
	// tolerant one: derivation already got this far, so the header parsed
	// once already).
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
	formatFlowed = err == nil && strings.EqualFold(params["format"], "flowed")
	return string(part.Content), formatFlowed
}
