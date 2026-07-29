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

// Lookup returns the recorded terminal ack for localID, if this ring has one.
// A replayed sync uses this to hand back the exact same outcome — including a
// rejection — rather than reprocessing the letter (§4.7, test V-9).
//
// The scan runs newest-first: local_id is meant to be unique per outbound
// letter, but if a bug ever produced two entries for the same id, the most
// recent is the one a replay should see.
func (r *AckRing) Lookup(localID string) (AckEntry, bool) {
	for i := len(r.Entries) - 1; i >= 0; i-- {
		if r.Entries[i].LocalID == localID {
			return r.Entries[i], true
		}
	}
	return AckEntry{}, false
}

// Record appends a terminal ack, evicting the oldest entry once the ring is
// at capacity (§4.7). status is a protocol.AckStatus value passed as a plain
// string, per this package's policy of staying free of a protocol import
// (see AckEntry's doc comment).
func (r *AckRing) Record(localID, status string, at time.Time) {
	r.Entries = append(r.Entries, AckEntry{LocalID: localID, Status: status, At: at})
	if over := len(r.Entries) - AckRingSize; over > 0 {
		r.Entries = r.Entries[over:]
	}
}

// AddPendingNotice records an ayllu change whose notice letter has not been
// APPENDed yet (§7.6).
func (s *State) AddPendingNotice(n PendingNotice) {
	s.PendingNotices = append(s.PendingNotices, n)
}

// RemovePendingNotice drops a pending notice once its letter has been
// APPENDed. It is a no-op if id is not present, which makes it safe to call
// again after a crash-and-retry of the flush in §7.6 without checking first.
func (s *State) RemovePendingNotice(id string) {
	kept := s.PendingNotices[:0]
	for _, n := range s.PendingNotices {
		if n.ID != id {
			kept = append(kept, n)
		}
	}
	s.PendingNotices = kept
}

// NextPututuCounter increments and returns the counter for a newly sent SMS
// (§10.2). Counter-based rather than time-based because the device has no
// clock it can trust except just after a sync (A.8).
func (s *State) NextPututuCounter() uint64 {
	s.PututuCounter++
	return s.PututuCounter
}

// ReconcilePututuCounter jumps the counter forward if the device reports
// having already accepted a higher value than the server currently holds —
// the healing step for a state.json restored from an older backup, which
// would otherwise roll the counter back and leave every subsequent SMS
// silently ignored by the device (§10.3, test V-20). It never moves the
// counter backward: a device report lower than the server's counter is stale
// or lying, and honouring it would let an SMS be replayed.
func (s *State) ReconcilePututuCounter(seen uint64) {
	if seen > s.PututuCounter {
		s.PututuCounter = seen
	}
}
