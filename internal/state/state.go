// Package state owns /data/state.json — the machine-written file that holds
// what the mailbox cannot represent and a human should never edit.
//
// Ids, integers, and timestamps only: no bodies, no subjects, no addresses
// (invariant I-1, wasi-server-plan §3). The storage-invariant test V-11 greps
// /data and /backups for letter substrings; anything content-shaped landing here
// is a defect, not a convenience.
//
// There is no database (A.9). What a database would have provided — atomicity,
// crash consistency, idempotent replay — is provided here explicitly: every
// Update writes a temp file in the same directory, fsyncs it, renames, and
// fsyncs the directory, behind a single writer mutex (§3, test V-12).
package state

import "time"

// State is the whole of state.json.
type State struct {
	// UIDValidity and LastUID mirror the device's cursor. The mirror is used
	// only for the pututu skip-if-recently-synced check and operator display —
	// the cursor in the request is authoritative and always wins (§4.4).
	UIDValidity uint32 `json:"uidvalidity"`
	LastUID     uint32 `json:"last_uid"`

	// Acks is the outbound idempotency ring (§4.7).
	Acks AckRing `json:"acks"`

	// PututuCounter is the monotonic doorbell counter, incremented per SMS sent
	// (§10.2). It can be jumped forward by wire reconciliation (§10.3).
	PututuCounter uint64 `json:"pututu_counter"`

	// LastSyncAt drives pututu coalescing: skip the SMS entirely if the device
	// synced since the triggering arrival (§10.1).
	LastSyncAt time.Time `json:"last_sync_at"`

	// PendingNotices holds ayllu changes that landed on disk but whose notice
	// letter has not been APPENDed yet. Flushed at startup, so the purchasable
	// failure is "notice arrives a little late" and never "change happened
	// silently" (§7.6, test V-17).
	PendingNotices []PendingNotice `json:"pending_notices"`
}

// AckRingSize is the ack ring capacity — comfortably above any plausible device
// outbox (§4.7).
const AckRingSize = 4096

// AckRing records the terminal outcome of every outbound letter the server has
// processed, keyed by the device's local_id.
//
// Rejections are recorded too, so a replayed sync receives the *same* rejection
// rather than a fresh attempt (§4.7, test V-9).
type AckRing struct {
	Entries []AckEntry `json:"entries"`
}

// AckEntry is one terminal ack. Status is a protocol.AckStatus; it is stored as
// a plain string so this package stays free of a protocol import cycle.
type AckEntry struct {
	LocalID string    `json:"local_id"`
	Status  string    `json:"status"`
	At      time.Time `json:"at"`
}

// PendingNotice is an ayllu change awaiting its notice letter. It carries ids
// and names, never addresses: the addresses live in ayllu-log.jsonl, which is
// never device-visible (§7.4).
type PendingNotice struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Action    string    `json:"action"`
	ContactID string    `json:"contact_id"`
	Name      string    `json:"name"`
	Actor     string    `json:"actor"`
}

// Store is the single writer of state.json.
//
// Update must not return until the new contents are durable. The ordering that
// depends on it is explicit and untransactional by design: an outbound letter is
// SMTP-sent, then its ack is written and fsynced, and only then acked to the
// device. A crash in between costs a duplicate send on replay, never a lost
// letter — a relative seeing a letter twice is the correct failure; a letter the
// kid watched leave that never arrives is not (§4.7, test V-9).
type Store interface {
	// Snapshot returns a copy of the current state. Callers must not assume it
	// stays current.
	Snapshot() State

	// Update applies fn under the writer mutex and persists the result
	// atomically, returning only once it is durable.
	Update(fn func(*State) error) error
}
