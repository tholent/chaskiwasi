// Package filing implements wasi-server-plan §5.1 (filing on arrival and
// reconciliation), its spam-folder backstop, and the release mechanism of
// §8. It is the only package that decides whether a piece of mail sitting in
// INBOX belongs there.
//
// Three responsibilities, in the order the spec states them:
//
//  1. Arrival (HandleNotify) is what the IDLE goroutine on INBOX drives. It
//     is a NOTIFICATION path, not an ingest path: on new mail it resolves
//     the sender against ACTIVE contacts only. Resolved -> leave in INBOX,
//     ring the doorbell. Anything else — stranger, or deactivated contact —
//     IMAP MOVE to Held. No bounce, ever (I-3). Held is the quarantine;
//     there is no held table.
//  2. Reconciliation (Reconcile) makes filing not depend on uptime: at
//     startup and at the top of every sync, it scans INBOX and quarantines
//     anything that should not be there, so a stranger's mail that arrived
//     while Wasi was down is quarantined before any sync's cursor can pass
//     it by (V-15).
//  3. The spam-folder backstop (CheckSpam / RunSpamBackstop) MOVEs anything
//     sitting in the provider's Spam/Junk folder to Held, at startup, at
//     each sync, and at least every 15 minutes — the one path by which
//     family mail could vanish without device or guardian ever learning it
//     existed.
//
// # F-2: reconciliation and arrival deliberately use different resolutions
//
// specs/implementation-plan.md §4a, finding F-2, is binding here and is the
// subtlest thing in this package:
//
//   - HandleNotify (arrival) resolves against ACTIVE contacts only
//     (ayllu.Store.ResolveActive). This is the ONLY place that check runs.
//   - Reconcile resolves against the FULL table (ayllu.Store.Resolve),
//     tombstones and past addresses included, and quarantines senders that
//     do not resolve AT ALL — strangers only.
//
// Reconcile must never call ResolveActive. If it did, deactivating a contact
// would sweep all of that person's already-delivered letters into Held on
// the very next reconciliation pass, which V-6 explicitly forbids ("first
// two render with the name, third in Held") and which §7.2 settles: "the
// decision is made once, at arrival; history is immutable." The active-only
// check belongs to arrival and nowhere else. TestF2_ReconcileNeverCallsResolveActive
// and TestF2_HandleNotifyNeverCallsResolve in filing_test.go fail loudly if
// the two are ever collapsed into one resolution call.
//
// # Logging
//
// No letter content reaches the logger at any level (I-1): every log line
// here carries a UID and, where computable, a letter id — never an address,
// never a subject, never a body.
package filing

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"sync"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/letterid"
	"github.com/tholent/chaskiwasi/internal/mailbox"
)

// inboxFolder is the one IMAP folder name this package never gets from
// config: "INBOX" is a reserved name per RFC 3501/9051, not a per-provider
// choice, unlike Held and Spam (§13).
const inboxFolder = "INBOX"

// DefaultSpamFolder is used when Config.SpamFolder is left empty. Fastmail's
// own provider-side spam folder is named "Spam"; this repo's local maddy
// fixture overrides it to "Junk" via wasi.toml, because the folder name is
// deliberately configurable (§5.1, §13, deploy/README.md).
const DefaultSpamFolder = "Spam"

// DefaultSpamInterval is the backstop's minimum check cadence when nothing
// else (startup, a sync) is triggering one — §5.1's "at least every 15
// minutes".
const DefaultSpamInterval = 15 * time.Minute

// arrivalBatchSize bounds one FetchAbove call in HandleNotify. It is a
// package variable, not a const, only so tests can shrink it to exercise the
// multi-batch loop without seeding hundreds of messages.
var arrivalBatchSize = 50

// Config configures a Service.
type Config struct {
	Mailbox mailbox.Mailbox
	Ayllu   ayllu.Store

	// HeldFolder is the quarantine folder (§5.1, §13's mail.held_folder).
	// Required.
	HeldFolder string
	// SpamFolder is the provider-side spam/junk folder checked as a
	// backstop (§5.1, §13's mail.spam_folder). Defaults to
	// DefaultSpamFolder if empty.
	SpamFolder string
	// SpamInterval bounds RunSpamBackstop's sweep cadence. Defaults to
	// DefaultSpamInterval if <= 0.
	SpamInterval time.Duration

	// Doorbell is rung on arrival to an active contact and on release from
	// Held (§5.1, §8). NopDoorbell is used if nil.
	Doorbell Doorbell

	// Logger defaults to slog.Default. No letter content is ever passed to
	// it (I-1).
	Logger *slog.Logger
}

// Service implements this package's responsibilities. It is safe for
// concurrent use: every method that touches IMAP state serializes through
// mu, because Reconcile, HandleNotify, CheckSpam, and the two release paths
// can all be triggered from different goroutines (the sync handler, the
// IDLE loop, the periodic spam ticker, and the guardian web UI) and must not
// interleave their MOVEs.
type Service struct {
	mailbox mailbox.Mailbox
	ayllu   ayllu.Store

	heldFolder   string
	spamFolder   string
	spamInterval time.Duration
	doorbell     Doorbell
	log          *slog.Logger

	mu sync.Mutex
	// arrivalHighWater is the highest INBOX UID this Service has already
	// made a decision about, in this process's lifetime — either by
	// Reconcile's full-table sweep or by a previous HandleNotify's
	// active-only check. HandleNotify only ever looks above this mark.
	//
	// This is what keeps the two resolutions from colliding: once a message
	// has been examined and left in INBOX, resolution to *something* in the
	// full table is permanent (rows are never deleted, only tombstoned;
	// addresses only ever move to PastAddresses, never disappear), so
	// Reconcile never needs to re-examine it — and HandleNotify must never
	// re-examine it under the active-only rule, because the contact behind
	// it may have been deactivated since, and re-deciding would violate
	// §7.2's "the decision is made once, at arrival."
	//
	// It is deliberately in-memory only: a restart resets it to zero, which
	// is exactly right, because Start's initial Reconcile then re-establishes
	// the mark from ground truth (the current state of INBOX) rather than
	// trusting a stale idea of what was already decided before the restart —
	// the same "filing must not depend on uptime" principle applied to
	// filing's own bookkeeping, not just to whether IDLE was connected.
	arrivalHighWater uint32
}

// NewService builds a Service. It does not touch the network; call Start
// before HandleNotify is ever invoked (see Start's doc comment for why the
// ordering matters).
func NewService(cfg Config) *Service {
	spamFolder := cfg.SpamFolder
	if spamFolder == "" {
		spamFolder = DefaultSpamFolder
	}
	interval := cfg.SpamInterval
	if interval <= 0 {
		interval = DefaultSpamInterval
	}
	doorbell := cfg.Doorbell
	if doorbell == nil {
		doorbell = NopDoorbell
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		mailbox:      cfg.Mailbox,
		ayllu:        cfg.Ayllu,
		heldFolder:   cfg.HeldFolder,
		spamFolder:   spamFolder,
		spamInterval: interval,
		doorbell:     doorbell,
		log:          logger,
	}
}

// Start runs the two "at startup" halves of §5.1: an initial Reconcile and
// an initial CheckSpam. Call it once, before the IDLE loop starts and before
// HandleNotify is ever invoked.
//
// The ordering requirement is not cosmetic: HandleNotify only ever looks at
// INBOX messages above arrivalHighWater, which starts at zero on a fresh
// Service. Reconcile's initial pass is what advances that mark past
// whatever is already sitting in INBOX — mail this process did not
// personally decide the fate of, most of it years old. Skipping this call
// (or calling HandleNotify before it) would make the very first
// HandleNotify treat the entire mailbox history as "new arrivals" and apply
// the active-only check to it, which is exactly the bug F-2 exists to
// prevent, arriving through a different door.
func (s *Service) Start(ctx context.Context) error {
	if err := s.Reconcile(ctx); err != nil {
		return fmt.Errorf("filing: startup reconcile: %w", err)
	}
	if _, err := s.CheckSpam(ctx); err != nil {
		return fmt.Errorf("filing: startup spam check: %w", err)
	}
	return nil
}

// Reconcile implements §5.1's reconciliation pass (test V-15): every message
// currently in INBOX whose sender does not resolve against the FULL ayllu
// table — Resolve, tombstones and past addresses included, never
// ResolveActive — is MOVEd to Held. Call it at startup (via Start) and at
// the top of every sync; it satisfies syncsvc's Reconciler interface by
// having exactly this signature.
//
// Reconcile never rings the doorbell: it is a cleanup pass over mail that
// may be arbitrarily old, not an arrival signal — only HandleNotify signals
// arrival (§10.1).
func (s *Service) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// No delivery boundary: strangers only. This is the startup pass and the
	// window-resync fallback, where there is no trustworthy cursor to separate
	// a removed contact's new mail from their history (see ReconcileSync).
	return s.reconcileLocked(ctx, 0, false)
}

// ReconcileSync is the per-sync reconciliation pass (§5.1), given the device's
// own delivery cursor. Besides quarantining strangers like Reconcile, it MOVEs
// to Held any INBOX message that resolves to an INACTIVE contact and sits
// ABOVE deliveredUID — mail the child has not received yet, from someone who
// has been removed from the list.
//
// This closes finding F-9. Without it, such a message (typically one that
// arrived while Wasi was briefly down, so the arrival path never saw it) would
// resolve against the tombstone at read time and be delivered as if it were
// history — letting a contact removed *for a reason* reach the child by timing
// a letter to an outage. Because this runs at the top of the sync, before
// derivation, the message is in Held before the same sync could deliver it.
//
// deliveredUID is the device's authoritative cursor, so a rolled-back server
// state.json cannot fool it, and the caller passes it only for a concrete
// cursor — a window resync falls back to Reconcile, so a factory-reset device
// legitimately re-requesting recent mail never triggers a history sweep. Mail
// at or below deliveredUID — already-delivered history from a since-deactivated
// contact — is deliberately left untouched (§7.2, test V-6).
//
// The one over-hold this accepts: a letter that arrived while its sender was
// active but that the child had not yet synced to receive when the sender was
// deactivated is above the cursor and inactive, so it is held for guardian
// review rather than delivered. That is the safe direction — the guardian can
// release it — and it is the correct default for undelivered mail from someone
// just removed.
func (s *Service) ReconcileSync(ctx context.Context, deliveredUID uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileLocked(ctx, deliveredUID, true)
}

// reconcileLocked scans INBOX once and quarantines what does not belong.
// holdInactive gates the F-9 rule; holdInactiveAbove is the delivery boundary
// it uses. Caller holds mu.
func (s *Service) reconcileLocked(ctx context.Context, holdInactiveAbove uint32, holdInactive bool) error {
	msgs, err := s.mailbox.List(ctx, inboxFolder)
	if err != nil {
		return fmt.Errorf("filing: reconcile: listing %s: %w", inboxFolder, err)
	}

	var maxUID uint32
	for _, raw := range msgs {
		if raw.UID > maxUID {
			maxUID = raw.UID
		}

		h := parseHeaders(raw.Data)
		if h.fromOK {
			if contact, resolved := s.ayllu.Resolve(h.from); resolved {
				// Resolves in the full table (tombstones and past addresses
				// included). Leave it — history renders (§7.2) — unless it is
				// a removed contact's not-yet-delivered mail, which is the F-9
				// case: quarantine it before this sync could deliver it.
				if holdInactive && !contact.Active && raw.UID > holdInactiveAbove {
					if err := s.mailbox.Move(ctx, inboxFolder, raw.UID, s.heldFolder); err != nil {
						return fmt.Errorf("filing: reconcile: quarantining removed-contact mail uid %d: %w", raw.UID, err)
					}
					s.log.Info("filing: reconciliation quarantined a removed contact's undelivered mail",
						"uid", raw.UID, "letter_id", h.letterID)
				}
				continue
			}
		}
		// Either the sender header would not even parse, or it resolves
		// against nothing in the full table — tombstones and past
		// addresses included. Either way, default-deny: this is a
		// stranger (design-spec §3.1).
		if err := s.mailbox.Move(ctx, inboxFolder, raw.UID, s.heldFolder); err != nil {
			return fmt.Errorf("filing: reconcile: quarantining uid %d: %w", raw.UID, err)
		}
		s.log.Info("filing: reconciliation quarantined stranger",
			"uid", raw.UID, "letter_id", h.letterID)
	}

	if maxUID > s.arrivalHighWater {
		s.arrivalHighWater = maxUID
	}
	return nil
}

// HandleNotify implements §5.1's arrival path: the IDLE goroutine on INBOX
// is a notification path, not an ingest path. Call it whenever the mailbox
// signals that INBOX may have changed (an IMAP IDLE unilateral update is the
// expected trigger, but any cue works — HandleNotify is safe to call
// spuriously, since it does nothing when there is nothing new).
//
// It fetches only messages above arrivalHighWater — genuinely new mail this
// process has not yet decided the fate of — and for each one:
//
//   - resolves to an ACTIVE contact (ResolveActive) -> leaves it in INBOX,
//     rings the doorbell;
//   - anything else (stranger, or deactivated contact) -> MOVEs it to Held.
//     No bounce, ever (I-3).
//
// This is the only place ResolveActive decides a message's fate; see the
// package doc's F-2 note for why Reconcile must never do the same.
func (s *Service) HandleNotify(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handleNotifyLocked(ctx)
}

func (s *Service) handleNotifyLocked(ctx context.Context) error {
	for {
		raws, err := s.mailbox.FetchAbove(ctx, s.arrivalHighWater, arrivalBatchSize)
		if err != nil {
			return fmt.Errorf("filing: handle notify: fetching above uid %d: %w", s.arrivalHighWater, err)
		}
		if len(raws) == 0 {
			return nil
		}

		for _, raw := range raws {
			if err := s.fileArrivalLocked(ctx, raw); err != nil {
				return err
			}
			if raw.UID > s.arrivalHighWater {
				s.arrivalHighWater = raw.UID
			}
		}

		if len(raws) < arrivalBatchSize {
			return nil
		}
		// A full batch means more may remain above the new high-water mark
		// (mirrors syncsvc.assembleLetters' capped/more handling); loop
		// rather than waiting for the next notify.
	}
}

func (s *Service) fileArrivalLocked(ctx context.Context, raw mailbox.Raw) error {
	h := parseHeaders(raw.Data)
	if h.fromOK {
		if _, active := s.ayllu.ResolveActive(h.from); active {
			s.log.Info("filing: arrival kept in INBOX", "uid", raw.UID, "letter_id", h.letterID)
			s.doorbell.Ring(ctx)
			return nil
		}
	}
	if err := s.mailbox.Move(ctx, inboxFolder, raw.UID, s.heldFolder); err != nil {
		return fmt.Errorf("filing: arrival: quarantining uid %d: %w", raw.UID, err)
	}
	s.log.Info("filing: arrival quarantined", "uid", raw.UID, "letter_id", h.letterID)
	return nil
}

// CheckSpam implements §5.1's spam-folder backstop (test V-16): every
// message currently in the configured spam/junk folder is MOVEd to Held,
// unconditionally — no ayllu resolution is consulted at all, because the
// backstop exists precisely for mail that never reached a resolution
// decision: provider-side filtering pulled it out of INBOX before this
// package ever saw it. Call it at startup (via Start), at the top of every
// sync, and periodically via RunSpamBackstop. Returns the number of messages
// moved.
func (s *Service) CheckSpam(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	msgs, err := s.mailbox.List(ctx, s.spamFolder)
	if err != nil {
		return 0, fmt.Errorf("filing: spam backstop: listing %s: %w", s.spamFolder, err)
	}
	for _, raw := range msgs {
		if err := s.mailbox.Move(ctx, s.spamFolder, raw.UID, s.heldFolder); err != nil {
			return 0, fmt.Errorf("filing: spam backstop: moving uid %d: %w", raw.UID, err)
		}
		h := parseHeaders(raw.Data)
		s.log.Info("filing: spam backstop quarantined", "uid", raw.UID, "letter_id", h.letterID)
	}
	return len(msgs), nil
}

// RunSpamBackstop calls CheckSpam every Config.SpamInterval (DefaultSpamInterval
// if unset) until ctx is cancelled — the "at least every 15 minutes" half of
// §5.1's backstop; the "at startup" and "at each sync" halves are Start and a
// direct CheckSpam call from the sync path.
//
// A transient IMAP failure is logged and swallowed rather than stopping the
// loop: this check is mail's last line of defense against silently
// vanishing into a provider spam filter, and one failed sweep must not
// cancel the next one.
func (s *Service) RunSpamBackstop(ctx context.Context) {
	ticker := time.NewTicker(s.spamInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.CheckSpam(ctx); err != nil {
				s.log.Warn("filing: periodic spam backstop failed", "error", err)
			}
		}
	}
}

// findInHeldLocked fetches the message at uid from the Held folder. Callers
// must hold mu. Release always re-reads the live article rather than
// trusting a cached view — the guardian UI reads Held live over IMAP with no
// mirror to fall out of sync (§8), and neither does this.
func (s *Service) findInHeldLocked(ctx context.Context, uid uint32) (mailbox.Raw, error) {
	msgs, err := s.mailbox.List(ctx, s.heldFolder)
	if err != nil {
		return mailbox.Raw{}, fmt.Errorf("filing: listing %s: %w", s.heldFolder, err)
	}
	for _, raw := range msgs {
		if raw.UID == uid {
			return raw, nil
		}
	}
	return mailbox.Raw{}, fmt.Errorf("filing: uid %d not found in %s", uid, s.heldFolder)
}

// parsedHeaders is the small slice of a raw message this package ever
// inspects: who it's from (for resolution) and enough to compute a letter id
// (for log correlation only — I-1).
type parsedHeaders struct {
	from     string
	fromOK   bool
	letterID string
}

// parseHeaders extracts what filing needs from a raw RFC 5322 message. A
// message that will not even parse, or has no readable From address, comes
// back with fromOK false — treated as an unparseable sender, which every
// caller here defaults to "not resolved" (design-spec §3.1's default-deny
// resolution, applied to a header that failed to parse instead of one that
// resolved to nothing).
func parseHeaders(raw []byte) parsedHeaders {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return parsedHeaders{}
	}

	var h parsedHeaders
	if addrs, err := msg.Header.AddressList("From"); err == nil && len(addrs) > 0 {
		h.from, h.fromOK = addrs[0].Address, true
	}
	if id := msg.Header.Get("Message-Id"); id != "" {
		// Log correlation only (§4.5's stable wire id is letterid's job at
		// derivation time too; recomputing it here from the same raw header
		// is what keeps a filing log line and a later derivation log line
		// pointing at the same id without this package ever needing to see
		// derive's output).
		h.letterID = letterid.FromMessageID(id)
	}
	return h
}
