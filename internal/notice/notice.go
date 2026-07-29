// Package notice is the single mechanism through which every ayllu change is
// announced (I-4, §7.4). Add, deactivate, reactivate, and address-change
// events all pass through here on their way to becoming one letter APPENDed
// to INBOX from the reserved system contact c_sys, plus — optionally — one
// SMTP copy to guardian addresses (§7.5). It also owns the §12.3
// certificate-expiry guardian-copy alarm, which deliberately does NOT go
// through the INBOX path (see CertExpiryCopy).
//
// # What a notice must never contain
//
// I-2: no email address, old or new, in any wording. Guardians who need the
// actual address find it in ayllu-log.jsonl (already written by the time
// this package sees the event — see ayllu.Store.Mutate), surfaced by the web
// UI. V-14's vocabulary boundary also applies: this is outgoing mail, so
// "pututu", "ayllu", and "kipu" must never appear in generated text. All
// wording lives in one reviewable place, text.go.
//
// # Crash ordering (§7.6, test V-17)
//
// The caller — whoever just called ayllu.Store.Mutate and got an Event back
// — has already made the change durable: ayllu.toml is written and fsynced,
// and the change-log line is appended, before Mutate returns. This package
// covers the rest of §7.6's sequence for that Event:
//
//  1. Add the event to pending_notices in state.json, fsync (Announce).
//  2. APPEND the notice letter to INBOX (Announce).
//  3. Remove the event from pending_notices (Announce).
//  4. At startup, Flush re-drives steps 2-3 for anything still pending —
//     i.e. anything where the process died between step 1 and step 3 last
//     time.
//
// Callers MUST call Announce synchronously, immediately after a successful
// Mutate, with no other I/O in between — see Announce's doc comment for why
// that residual gap exists and cannot be closed from inside this package.
//
// # The APPEND-succeeded-but-removal-didn't case
//
// This is the one crash window that could turn "a little late" into "sent
// twice," and it gets a real answer rather than a shrug, because — unlike
// the outbound duplicate-send §4.7 accepts on purpose — a duplicate notice
// letter is not a cost this package is willing to pay:
//
// Every notice's Message-ID is deterministic, derived from the change event
// itself (see noticeIDFor) rather than minted at random, so the same change
// always maps to the same letter. Flush, which runs against ids that survived
// from a previous process's state.json, first lists INBOX once and checks
// each surviving pending notice's Message-ID against what is already there.
// A match means the APPEND from before the crash landed; Flush skips
// re-appending and goes straight to clearing pending_notices, so the crash
// costs at most a delayed removal, never a second letter. This is also why
// Flush must run once, at startup, before anything else calls Announce: a
// notice concurrently being minted by a fresh Announce call has an id Flush
// cannot yet know to look for, and the two must not race over the same
// mailbox state.
//
// # The change-happened-but-nothing-was-recorded case
//
// A crash between ayllu.toml being written and pending_notices being written
// leaves nothing for Flush to recover: the list changed and no record says a
// notice was owed. Reconcile closes that window from the other direction,
// using the append-only change log as the durable record. See Reconcile.
package notice

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/state"
	"github.com/tholent/chaskiwasi/internal/subject"
)

// inboxFolder is not configurable: "INBOX" is a reserved IMAP name (RFC
// 3501/9051), not a per-provider choice, unlike Held and Spam (§13) — same
// reasoning internal/filing uses for its own copy of this constant.
const inboxFolder = "INBOX"

// Mailbox is the narrow slice of mailbox.Mailbox this package needs: Append
// to place the letter, List to check INBOX for a notice that already made it
// there before a crash (see the package doc). Declared locally, rather than
// depended on as the concrete interface, so tests substitute an in-memory
// fake without standing up IMAP — the same pattern internal/derive uses for
// Stripper and AylluResolver.
type Mailbox interface {
	Append(ctx context.Context, folder string, msg []byte, at time.Time) error
	List(ctx context.Context, folder string) ([]mailbox.Raw, error)
}

// Submitter is the SMTP side of the §7.5 exception: fixed guardian addresses
// from human-owned config, system-generated text only. Structurally
// identical to mailbox.Submitter, declared locally for the same testing
// reason as Mailbox above.
type Submitter interface {
	Send(ctx context.Context, from string, to []string, msg []byte) error
}

// Config configures a Service.
type Config struct {
	// State is the single writer of state.json (§7.6's pending_notices).
	// Required.
	State state.Store
	// Mailbox APPENDs the notice letter to INBOX. Required.
	Mailbox Mailbox
	// Submitter sends the optional guardian SMTP copy (§7.5). Nil disables
	// the copy regardless of CopyAddresses — "off unless configured" degrades
	// safely either way.
	Submitter Submitter

	// MailboxAddress is the shared mailbox's own address (wasi.toml's
	// mail.address): the To: header on the INBOX notice letter, the From:
	// header on any guardian SMTP copy, and the domain half of every
	// generated Message-ID. Required. Passed as a plain string rather than a
	// config.Config dependency, matching how internal/subject takes
	// ownerName — this package stays free of a config import.
	MailboxAddress string
	// CopyAddresses is guardian.copy_addresses from wasi.toml (§7.5, §13).
	// Empty disables the SMTP exception entirely.
	CopyAddresses []string

	// Logger defaults to slog.Default(). No notice text or address ever
	// reaches it (I-1 covers content; I-2 covers addresses) — only ids,
	// contact ids, and actions.
	Logger *slog.Logger
}

// Service implements this package's responsibilities.
type Service struct {
	state     state.Store
	mailbox   Mailbox
	submitter Submitter

	mailboxAddr   string
	copyAddresses []string

	log *slog.Logger
}

// New builds a Service from cfg, rejecting a configuration missing a
// required dependency rather than deferring the failure to the first
// Announce call.
func New(cfg Config) (*Service, error) {
	if cfg.State == nil {
		return nil, errors.New("notice: Config.State is required")
	}
	if cfg.Mailbox == nil {
		return nil, errors.New("notice: Config.Mailbox is required")
	}
	if strings.TrimSpace(cfg.MailboxAddress) == "" {
		return nil, errors.New("notice: Config.MailboxAddress is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		state:         cfg.State,
		mailbox:       cfg.Mailbox,
		submitter:     cfg.Submitter,
		mailboxAddr:   cfg.MailboxAddress,
		copyAddresses: cfg.CopyAddresses,
		log:           logger,
	}, nil
}

// Announce implements §7.4/§7.6 for one ayllu.Event, freshly returned by
// ayllu.Store.Mutate (or filing.ReleaseStranger / ReleaseDeactivated, which
// return the same type). Call it synchronously, immediately after Mutate
// succeeds, with nothing else in between: ayllu.toml and the change-log line
// are already durable by the time Mutate returns, but this event is not
// recorded anywhere else yet, so a crash between Mutate returning and this
// call recording pending_notices would lose the notice entirely — a gap this
// package cannot close from the inside, because it does not own the call
// site. Keeping the two calls back-to-back, with no I/O between them, is the
// mitigation; see the package doc for the crash window §7.6 assigns to this
// package's own steps.
//
// ayllu.ActionCosmetic is a deliberate no-op: ayllu's own package doc says
// the youth's cosmetic overlay gets "no notice, no log," so this returns nil
// immediately rather than consulting text.go for wording that must not
// exist.
func (s *Service) Announce(ctx context.Context, ev ayllu.Event) error {
	if ev.Action == ayllu.ActionCosmetic {
		return nil
	}

	pn := state.PendingNotice{
		ID:        noticeIDFor(ev),
		At:        ev.At,
		Action:    string(ev.Action),
		ContactID: ev.ContactID,
		Name:      ev.Name,
		Actor:     ev.Actor,
	}

	// Step 1: add to pending_notices, fsync (§7.6).
	if err := s.addPending(pn); err != nil {
		return fmt.Errorf("notice: recording pending notice: %w", err)
	}

	// Step 2: APPEND. id was just minted, so it cannot already be in the
	// mailbox — no dedup check needed on this path (see package doc).
	body, err := s.appendLetter(ctx, pn)
	if err != nil {
		// The letter did not go out. pending_notices still holds it, so a
		// later Flush will retry — "arrives a little late," not silent.
		return fmt.Errorf("notice: appending notice for %s: %w", pn.ContactID, err)
	}

	// Step 3: remove from pending_notices (§7.6). If this fails, the letter
	// already went out; the stale pending_notices entry is exactly what
	// Flush's dedup check exists to make safe on the next restart.
	if err := s.removePending(pn.ID); err != nil {
		return fmt.Errorf("notice: clearing pending notice: %w", err)
	}

	s.deliverGuardianCopy(ctx, subjectFor(pn), body)
	return nil
}

// Flush implements §7.6's startup recovery: every pending_notices entry that
// survived a previous process's lifetime gets APPENDed (unless it turns out
// to already be in INBOX — see the package doc) and then cleared. Call it
// once, at startup, after State and Mailbox are both ready but before
// anything else can call Announce — test V-17.
//
// Flush processes every surviving entry even if one fails: a problem with
// one pending notice must not delay the others, per §7.6's "late, never
// silent." Errors are joined and returned after the whole pass completes.
func (s *Service) Flush(ctx context.Context) error {
	pending := s.state.Snapshot().PendingNotices
	if len(pending) == 0 {
		return nil
	}

	alreadySent, err := s.alreadyAppended(ctx, pending)
	if err != nil {
		// Without this check, appending now could duplicate a notice that
		// went out just before the crash. Refuse to guess: fail the whole
		// flush and let the next startup try again.
		return fmt.Errorf("notice: flush: checking INBOX for already-sent notices: %w", err)
	}

	var errs []error
	for _, pn := range pending {
		var body string
		if alreadySent[s.messageIDFor(pn.ID)] {
			s.log.Info("notice: flush found notice already in INBOX, skipping duplicate append",
				"contact_id", pn.ContactID, "action", pn.Action)
			var err error
			if body, err = bodyFor(pn); err != nil {
				// Can't render a guardian copy for it, but the letter is
				// already delivered on the channel that matters — clear the
				// stale pending entry and move on rather than looping on it.
				s.log.Error("notice: flush: rendering body for guardian copy failed", "action", pn.Action, "error", err)
			}
		} else {
			var err error
			if body, err = s.appendLetter(ctx, pn); err != nil {
				errs = append(errs, fmt.Errorf("notice: flush: appending pending notice for %s: %w", pn.ContactID, err))
				continue // leave this one pending; try the rest
			}
		}

		if err := s.removePending(pn.ID); err != nil {
			errs = append(errs, fmt.Errorf("notice: flush: clearing pending notice for %s: %w", pn.ContactID, err))
			continue
		}
		s.deliverGuardianCopy(ctx, subjectFor(pn), body)
	}
	return errors.Join(errs...)
}

// Reconcile closes the crash window Flush cannot reach, and it is what makes
// I-4 literally true rather than nearly true.
//
// §7.6 writes ayllu.toml and the change-log line before it records anything in
// pending_notices. A crash in between leaves a contact list that changed with
// no record anywhere that a notice was owed — Flush sees an empty
// pending_notices and has nothing to recover from. The change log is the
// durable record (append-only, written before the gap), so reconciliation runs
// the other way round: take recent change-log events, derive each one's notice
// id, and append any whose Message-ID is not already in INBOX.
//
// Call it once at startup, after Flush and before anything can call Announce.
// Pass events from ayllu.ReadLog with a window comfortably wider than any
// plausible outage; re-examining an already-announced event is free, because
// the deterministic id makes the INBOX check exact.
func (s *Service) Reconcile(ctx context.Context, events []ayllu.Event) error {
	pending := make([]state.PendingNotice, 0, len(events))
	for _, ev := range events {
		if ev.Action == ayllu.ActionCosmetic {
			continue // cosmetic changes are never announced (§7.4)
		}
		pending = append(pending, state.PendingNotice{
			ID:        noticeIDFor(ev),
			At:        ev.At,
			Action:    string(ev.Action),
			ContactID: ev.ContactID,
			Name:      ev.Name,
			Actor:     ev.Actor,
		})
	}
	if len(pending) == 0 {
		return nil
	}

	alreadySent, err := s.alreadyAppended(ctx, pending)
	if err != nil {
		// Same refusal to guess as Flush: appending without knowing what is
		// already there trades a lost notice for a duplicate one, and neither
		// is acceptable when the alternative is trying again next startup.
		return fmt.Errorf("notice: reconcile: checking INBOX for already-sent notices: %w", err)
	}

	var errs []error
	for _, pn := range pending {
		if alreadySent[s.messageIDFor(pn.ID)] {
			continue // announced already; the common case by far
		}
		s.log.Warn("notice: change log holds a change with no notice in INBOX, announcing late",
			"contact_id", pn.ContactID, "action", pn.Action)
		if err := s.addPending(pn); err != nil {
			errs = append(errs, fmt.Errorf("notice: reconcile: recording pending notice for %s: %w", pn.ContactID, err))
			continue
		}
		body, err := s.appendLetter(ctx, pn)
		if err != nil {
			errs = append(errs, fmt.Errorf("notice: reconcile: appending notice for %s: %w", pn.ContactID, err))
			continue // stays pending; Flush retries on the next startup
		}
		if err := s.removePending(pn.ID); err != nil {
			errs = append(errs, fmt.Errorf("notice: reconcile: clearing pending notice for %s: %w", pn.ContactID, err))
			continue
		}
		s.deliverGuardianCopy(ctx, subjectFor(pn), body)
	}
	return errors.Join(errs...)
}

// CertExpiryCopy implements §12.3's alarm path: at <45 days remaining on the
// device listener's certificate, send the optional guardian SMTP copy.
// Deliberately NOT an INBOX notice letter — certificate operations are
// operator noise, not family record, and the child's inbox is not an ops
// channel (§12.3) — so this never touches pending_notices or the mailbox
// APPEND path at all. The caller owns inspecting the certificate and
// deciding when 45 days has been crossed (and, per §12.3, the guardian-UI
// banner); this method owns only sending.
//
// Returns nil without sending if the guardian-copy exception is not
// configured — "off unless configured" (§7.5) applies here too.
func (s *Service) CertExpiryCopy(ctx context.Context, daysRemaining int) error {
	return s.guardianCopy(ctx, certExpirySubject, certExpiryBody(daysRemaining))
}

// deliverGuardianCopy sends the best-effort guardian SMTP copy of a notice
// already durably APPENDed to INBOX (§7.5). Failures are logged, never
// returned: the mandatory channel (the INBOX letter I-4 depends on) has
// already succeeded by the time this runs, and the optional copy must not
// retroactively turn that success into an error the caller has to unwind.
func (s *Service) deliverGuardianCopy(ctx context.Context, subj, body string) {
	if body == "" {
		return
	}
	if err := s.guardianCopy(ctx, subj, body); err != nil {
		s.log.Error("notice: guardian copy send failed", "error", err)
	}
}

// guardianCopy is the shared implementation behind the notice copy and
// CertExpiryCopy: build one message and hand it to Submitter. Returns nil
// without sending when the exception is not configured.
func (s *Service) guardianCopy(ctx context.Context, subj, body string) error {
	if s.submitter == nil || len(s.copyAddresses) == 0 {
		return nil
	}
	msg, err := s.buildGuardianMessage(subj, body, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("building guardian copy: %w", err)
	}
	if err := s.submitter.Send(ctx, s.mailboxAddr, s.copyAddresses, msg); err != nil {
		return fmt.Errorf("sending guardian copy: %w", err)
	}
	return nil
}

// addPending persists pn into state.json's pending_notices and fsyncs before
// returning (§7.6 step 1) — state.Store.Update does not return until the new
// contents are durable.
func (s *Service) addPending(pn state.PendingNotice) error {
	return s.state.Update(func(st *state.State) error {
		st.AddPendingNotice(pn)
		return nil
	})
}

// removePending clears id from pending_notices (§7.6 step 3).
// state.RemovePendingNotice is a no-op for an id already gone, which is what
// makes a retried Flush call safe (see the package doc).
func (s *Service) removePending(id string) error {
	return s.state.Update(func(st *state.State) error {
		st.RemovePendingNotice(id)
		return nil
	})
}

// appendLetter builds pn's notice message and APPENDs it to INBOX, returning
// the rendered body so callers can reuse it for the guardian copy without
// rendering twice. Callers decide whether it's safe to call (Announce always
// is, freshly; Flush only after the dedup check in alreadyAppended finds no
// prior copy).
func (s *Service) appendLetter(ctx context.Context, pn state.PendingNotice) (string, error) {
	body, err := bodyFor(pn)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	msg, err := s.buildNoticeMessage(pn, body, now)
	if err != nil {
		return "", fmt.Errorf("building message: %w", err)
	}
	if err := s.mailbox.Append(ctx, inboxFolder, msg, now); err != nil {
		return "", err
	}
	s.log.Info("notice: appended", "contact_id", pn.ContactID, "action", pn.Action)
	return body, nil
}

// alreadyAppended lists INBOX once and reports, for each pending notice's
// deterministic Message-ID, whether a message bearing it is already there —
// the crash-recovery check the package doc describes. Keyed by Message-ID
// rather than by pn.ID so the caller's lookup is a simple map read.
func (s *Service) alreadyAppended(ctx context.Context, pending []state.PendingNotice) (map[string]bool, error) {
	msgs, err := s.mailbox.List(ctx, inboxFolder)
	if err != nil {
		return nil, err
	}

	want := make(map[string]bool, len(pending))
	for _, pn := range pending {
		want[s.messageIDFor(pn.ID)] = true
	}

	found := make(map[string]bool)
	for _, m := range msgs {
		if id := messageIDOf(m.Data); id != "" && want[id] {
			found[id] = true
		}
	}
	return found, nil
}

// messageIDFor is the deterministic Message-ID a notice with this pending-
// notice id carries, on both the original Announce attempt and any later
// Flush retry — the identity the dedup check in the package doc matches on.
func (s *Service) messageIDFor(id string) string {
	return fmt.Sprintf("<notice-%s@%s>", id, domainOf(s.mailboxAddr))
}

// messageIDOf extracts the Message-ID header from a raw RFC 5322 message,
// or "" if the message doesn't parse — treated as "not a match" by the
// caller rather than an error, since INBOX can and does hold real mail from
// relatives that this function has no business rejecting the whole List
// scan over.
func messageIDOf(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(msg.Header.Get("Message-Id"))
}

// buildNoticeMessage renders pn's notice as RFC 5322 bytes ready for APPEND.
// From is always the reserved system contact (§7.4) — never trusted from any
// input, always ayllu.SystemName/SystemAddress, which is what makes
// resolveLocked's unconditional c_sys match in internal/ayllu correct: any
// message actually sent this way really did come from the system.
func (s *Service) buildNoticeMessage(pn state.PendingNotice, body string, now time.Time) ([]byte, error) {
	from := fmt.Sprintf("%s <%s>", ayllu.SystemName, ayllu.SystemAddress)
	return buildPlainTextMessage(messageHeaders{
		messageID: s.messageIDFor(pn.ID),
		from:      from,
		to:        s.mailboxAddr,
		subject:   subjectFor(pn),
	}, body, now)
}

// buildGuardianMessage renders the §7.5 SMTP-copy message. Unlike the INBOX
// notice, this one is never re-derived or dedup-checked — it is best-effort
// (see deliverGuardianCopy) — so its Message-ID only needs to be unique, not
// deterministic.
func (s *Service) buildGuardianMessage(subj, body string, now time.Time) ([]byte, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("minting message id: %w", err)
	}
	id := hex.EncodeToString(b[:])
	return buildPlainTextMessage(messageHeaders{
		messageID: fmt.Sprintf("<copy-%s@%s>", id, domainOf(s.mailboxAddr)),
		from:      s.mailboxAddr,
		to:        strings.Join(s.copyAddresses, ", "),
		subject:   subj,
	}, body, now)
}

// messageHeaders is the small set of headers every message this package
// generates needs. Both notice letters and guardian copies are plain-text,
// single-part, quoted-printable, English-only system mail — there is no
// child-authored content anywhere in this package (§7.5), so there is no
// sanitisation concern beyond the Subject header, same as outbound child
// mail (§6.2, V-3).
type messageHeaders struct {
	messageID string
	from      string
	to        string
	subject   string
}

// buildPlainTextMessage renders one RFC 5322 message: h's headers, then body
// as quoted-printable text/plain. Quoted-printable rather than 8bit, same
// reasoning as internal/syncsvc's outbound builder — a name with an accent
// must survive a submission path that never promised 8BITMIME.
func buildPlainTextMessage(h messageHeaders, body string, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	header := func(name, value string) {
		fmt.Fprintf(&buf, "%s: %s\r\n", name, value)
	}
	header("Message-ID", h.messageID)
	header("From", h.from)
	header("To", h.to)
	header("Date", now.Format(time.RFC1123Z))
	// Subject text here is always server-generated (text.go), never
	// child-authored, but it does interpolate a guardian-supplied contact
	// name — sanitised and RFC 2047-encoded the same way outbound child
	// subjects are, on the same "never trust free text going into a raw
	// header" principle (§6.2, V-3).
	header("Subject", subject.EncodeHeader(subject.Sanitize(h.subject)))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, fmt.Errorf("encoding body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("encoding body: %w", err)
	}
	return buf.Bytes(), nil
}

// newNoticeID mints a random 128-bit hex id: state.PendingNotice.ID, and the
// left-hand side of the notice's deterministic Message-ID. Randomness here
// is what makes Announce's fresh APPEND safe to perform without a dedup
// check (package doc) — no id this call mints can already be in the mailbox
// — while remaining fixed for the lifetime of that one pending notice, which
// is what makes Flush's later dedup check by the same id correct.
// noticeIDFor derives a notice's id from the change event itself, so the same
// event always maps to the same id — and therefore the same Message-ID.
//
// A random id would have been enough for the crash window Flush covers, where
// state.json remembers what was in flight. It is not enough for the wider
// window: §7.6 writes ayllu.toml before it writes pending_notices, and a crash
// between those two costs the notice entirely — the list changed and nothing
// ever said so, which is the I-4 failure the whole mechanism exists to prevent.
// Closing that needs the durable record (ayllu-log.jsonl, appended before the
// gap) to be matchable against what is already in the mailbox, and a random id
// is by construction not matchable. Hence: derive, don't mint.
//
// Version is included because it is unique per change; the rest is defence in
// depth against a log line whose version was somehow reused.
func noticeIDFor(ev ayllu.Event) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s\x00%d",
		ev.Version, ev.Action, ev.ContactID, ev.At.UTC().UnixNano())
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// domainOf returns the domain half of addr, or "localhost" if addr has none
// — same fallback internal/syncsvc's newMessageID uses, so a malformed
// MailboxAddress degrades to a locally-unique-only Message-ID rather than a
// panic.
func domainOf(addr string) string {
	if at := strings.LastIndexByte(addr, '@'); at >= 0 && at+1 < len(addr) {
		return addr[at+1:]
	}
	return "localhost"
}
