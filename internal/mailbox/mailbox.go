// Package mailbox is the IMAP/SMTP boundary. The mailbox is the sole store of
// letters (invariant I-1, design-spec Principle 5): Wasi derives the device's
// view at read time and persists no letter content anywhere.
//
// Nothing here caches messages. If that ever seems worth adding, note that V-10
// depends on the absence of a cache — when strip is down, letters are delivered
// flagged degraded and re-derived cleanly on the next sync, which only works
// because there is no stale copy to invalidate (§5.3).
package mailbox

import (
	"context"
	"time"
)

// Raw is one message as it exists in the mailbox, untouched.
type Raw struct {
	UID uint32
	// InternalDate is the server's receipt time, used to sanity-check the Date
	// header before it reaches the wire (§4.3).
	InternalDate time.Time
	// Data is the full RFC 5322 message. Parsing is derivation's job (§5.2);
	// this package does not interpret it.
	Data []byte
}

// Mailbox is the IMAP side.
type Mailbox interface {
	// UIDValidity reports the current INBOX UIDVALIDITY. A cursor whose
	// uidvalidity no longer matches is treated exactly as an empty cursor, so
	// resets are invisible on the wire and need no firmware path (§4.4).
	UIDValidity(ctx context.Context) (uint32, error)

	// FetchAbove returns up to max messages with UID strictly above uid, in UID
	// order (§5.2).
	FetchAbove(ctx context.Context, uid uint32, max int) ([]Raw, error)

	// Recent returns the most recent n messages, newest span, for a window
	// resync. A device recovering from factory reset gets a bounded recent view;
	// the deep archive stays in the mailbox and graduates with it (§4.4).
	Recent(ctx context.Context, n int) ([]Raw, error)

	// List returns messages in a named folder — the guardian UI reads Held live
	// over IMAP, so there is no mirror to fall out of sync (§8).
	List(ctx context.Context, folder string) ([]Raw, error)

	// Move relocates a message between folders. Filing quarantines to Held;
	// release moves back to INBOX, where the message gets a new UID above the
	// cursor and flows through derivation like any arrival (§5.1, §8).
	Move(ctx context.Context, folder string, uid uint32, dest string) error

	// Append writes a message into a folder. Used for notice letters, which go
	// into INBOX from the reserved system contact so that one append informs
	// both the child and the guardians who hold the mailbox (§7.4).
	Append(ctx context.Context, folder string, msg []byte, at time.Time) error

	// Idle blocks, signalling on notify when INBOX changes. This is a
	// notification path, not an ingest path: filing still reconciles at startup
	// and at the top of every sync, so quarantine does not depend on uptime
	// (§5.1, test V-15).
	Idle(ctx context.Context, notify chan<- struct{}) error

	Close() error
}

// Submitter is the SMTP side. Invariant I-3: this carries only child-authored
// letters, plus the one narrow exception of operational copies to guardian
// addresses fixed in human-owned config (§7.5). No auto-replies, no bounces, no
// courtesy notifications to relatives (A.4).
type Submitter interface {
	Send(ctx context.Context, from string, to []string, msg []byte) error
}
