// Package ayllu is the contact list: contact id -> address, the active flag, and
// the youth's cosmetic overlay. It is the only place email addresses exist
// (invariant I-2), which is what makes the send allowlist true by construction
// and a lost device leak no addresses at all.
//
// Glossary (design-spec §0.1): ayllu = the extended kinship group = the contact
// list. The word is greppable on purpose and never appears in guardian-facing UI
// text (§9.1, test V-14).
//
// Two rules shape this package and are easy to violate by accident:
//
//   - Removal never deletes (I-5, §7.2). A removed contact becomes a permanent
//     tombstone with its address retained, because in a read-time architecture
//     the contact row is the rendering key for every letter that person ever
//     sent. Deleting the row would make their history stop resolving on the next
//     resync — silently, exactly like spam.
//   - Nothing changes silently (I-4, §7.4). Every mutation returns an Event that
//     the caller must turn into a change-log line and a notice letter.
package ayllu

import (
	"errors"
	"time"
)

// Contact is one row of ayllu.toml. Address is guardian-owned; Name, Pinned,
// Order and Portrait are the youth's cosmetic overlay (§3, design-spec §3.2).
type Contact struct {
	ID      string `toml:"id"`
	Name    string `toml:"name"`
	Address string `toml:"address"`
	// PastAddresses are addresses this contact previously used, retained
	// forever and consulted only at read time (Resolve, never ResolveActive).
	//
	// This exists because I-5 — removal never deletes — has to apply to
	// addresses, not just rows. In a read-time architecture the address is the
	// rendering key for every letter a person ever sent, so replacing it on a
	// readdress would make all of their history stop resolving: on the next
	// reconciliation pass their old letters would be swept into Held, and after
	// a factory-reset window resync they would be gone from the device
	// entirely. That is the "silently, exactly like spam" failure §7.1 exists
	// to prevent, arriving through the one door §7.1 left open.
	//
	// New mail from a past address still goes to Held, because ResolveActive
	// does not consult this list: a readdress usually means the person lost
	// access to the old account, and mail from it afterwards is exactly the
	// case a guardian should look at. History renders; the channel does not
	// silently reopen.
	PastAddresses []string `toml:"past_addresses,omitempty"`

	// Active false is a tombstone: history still renders, new mail is Held, and
	// outbound is rejected (§7.2).
	Active   bool   `toml:"active"`
	Pinned   bool   `toml:"pinned"`
	Order    int    `toml:"order"`
	Portrait string `toml:"portrait"`
}

// Store owns /data/ayllu.toml and /data/ayllu-log.jsonl.
//
// The resolution split is the heart of §7.2 and the reason V-6 exists:
//
//	Filing / reconciliation (arrival)  -> ResolveActive: new mail from a
//	                                      tombstone goes to Held.
//	Derivation (read time)             -> Resolve: old letters still render
//	                                      with the correct name.
//	Sending                            -> ResolveActive / ByIDActive:
//	                                      rejected_inactive.
type Store interface {
	// List returns the current version and every contact, tombstones included.
	// The version bumps on every mutation and drives the response Ayllu block.
	List() (version int, contacts []Contact)

	// Resolve matches an address against the FULL table including tombstones.
	// Read-time derivation only (§7.2).
	Resolve(addr string) (Contact, bool)

	// ResolveActive matches an address against active contacts only. Filing and
	// reconciliation only (§7.2).
	ResolveActive(addr string) (Contact, bool)

	// ByID looks up a contact id against the full table.
	ByID(id string) (Contact, bool)

	// Mutate applies one change atomically: rewrite ayllu.toml, append the
	// change-log line, and return the Event for the notice path. actor is the
	// guardian username recorded in the log. Crash ordering is §7.6.
	Mutate(actor string, m Mutation) (Event, error)
}

// Action names a kind of change. Address change is announced with the same
// weight as add and remove, because silently repointing a contact id at a new
// address is precisely how this system would be turned into a redirection
// attack (§7.4).
type Action string

const (
	ActionAdd        Action = "add"
	ActionDeactivate Action = "deactivate"
	ActionReactivate Action = "reactivate"
	ActionReaddress  Action = "readdress"
	ActionCosmetic   Action = "cosmetic" // youth-owned overlay; no notice, no log
)

// Mutation is a requested change. Re-adding an address reuses the original
// contact id, matched against tombstones, so a person who leaves and returns
// stays one person in the archive (§7.2).
type Mutation struct {
	Action    Action
	ContactID string // empty on add: the store assigns or reuses one
	Name      string
	Address   string
	Pinned    bool
	Order     int
	Portrait  string
}

// Event is what a mutation produced. It feeds two sinks: one JSON line in
// ayllu-log.jsonl carrying full detail including addresses (permitted by I-2
// because that file is never device-visible), and one notice letter that names
// the person and never the address (§7.4).
type Event struct {
	At         time.Time `json:"at"`
	Actor      string    `json:"actor"`
	Action     Action    `json:"action"`
	ContactID  string    `json:"contact_id"`
	Name       string    `json:"name"`
	OldAddress string    `json:"old_address,omitempty"`
	NewAddress string    `json:"new_address,omitempty"`
	Version    int       `json:"version"`
}

var (
	// ErrUnknownContact is returned for a contact id not in the table.
	ErrUnknownContact = errors.New("ayllu: unknown contact")
	// ErrInactive is returned when a send targets a tombstone (§7.2).
	ErrInactive = errors.New("ayllu: contact is inactive")
	// ErrMaxContacts is returned when the cap in wasi.toml is reached. The
	// guardian UI surfaces it as a clear error rather than a silent no-op (A.3).
	ErrMaxContacts = errors.New("ayllu: contact list is full")
	// ErrSystemContact is returned for any attempt to mutate or write to c_sys,
	// the reserved system contact (§7.4).
	ErrSystemContact = errors.New("ayllu: the system contact cannot be modified")
)
