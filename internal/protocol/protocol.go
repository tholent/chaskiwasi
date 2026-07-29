// Package protocol holds the wire types for the single device<->server endpoint,
// POST /sync. It is shared by the server handler and by tools/chaskisim, so that
// a change to the contract cannot be made on one side only.
//
// Glossary (design-spec §0.1): ayllu = the contact list; kipu = device health
// telemetry; pututu = the SMS doorbell. These names are internal only and must
// never reach guardian-facing UI text or generated mail (wasi-server-plan §0).
//
// Every field below is specified in wasi-server-plan §4.2 (request) and §4.3
// (response). Deliberate absences — Held counts, email addresses, read state,
// threading, reply linkage — are listed in §1.1 and must stay absent.
package protocol

// Request is the body of POST /sync (§4.2). Only Cursor and AylluVersion are
// required; an empty sync is the normal heartbeat.
type Request struct {
	// Cursor is opaque to the device: server-minted, stored verbatim, echoed
	// back. "" means window resync (§4.4). The request cursor is authoritative;
	// the mirror in state.json never overrides it.
	Cursor string `json:"cursor"`

	// AylluVersion is the contact-list revision the device currently holds. The
	// response carries an Ayllu block only when this differs from the server's.
	AylluVersion int `json:"ayllu_version"`

	// PututuCounterSeen is the highest doorbell counter the device has accepted
	// (§10.3). A value above the server's heals a counter rolled back by a
	// state.json restore.
	PututuCounterSeen uint64 `json:"pututu_counter_seen,omitempty"`

	// Kipu is optional device health. Accepted from day one, stored to day-files
	// from M1 (§4.8). Unknown fields are preserved as received; the whole block
	// is capped at 512 bytes.
	Kipu map[string]any `json:"kipu,omitempty"`

	// Outbound is the device's outbox. There is no server-side outbound queue:
	// anything unacked stays on the device and is re-sent next sync (§4.7).
	Outbound []Outbound `json:"outbound,omitempty"`
}

// Outbound is one child-authored letter awaiting SMTP submission (§4.2).
type Outbound struct {
	// LocalID is device-assigned, <=32 bytes, unique per letter. It is the
	// idempotency key for the ack ring (§4.7).
	LocalID string `json:"local_id"`

	// ContactID resolves against active contacts only (§7.2).
	ContactID string `json:"contact_id"`

	// Subject is optional. Absent or empty means the server generates one (§6.2).
	// This is the one place child-supplied text enters an email header, so it is
	// sanitised server-side and never trusted (§6.2, test V-3).
	Subject string `json:"subject,omitempty"`

	// Body is capped at max_letter_chars graphemes; beyond that the letter is
	// acked "invalid" rather than truncated (§4.6).
	Body string `json:"body"`
}

// Response is the body returned for a processed sync (§4.3).
type Response struct {
	// ServerTime is epoch seconds. The device has no RTC.
	ServerTime int64 `json:"server_time"`

	// Cursor is stored verbatim by the device and echoed on the next sync.
	Cursor string `json:"cursor"`

	// PututuCounter is the server's current doorbell counter (§10.3).
	PututuCounter uint64 `json:"pututu_counter"`

	// Acks are terminal, every one of them: on any ack the device removes the
	// letter from its outbox and never resends it (§4.7).
	Acks []Ack `json:"acks,omitempty"`

	Letters []Letter `json:"letters,omitempty"`

	// More reports that UIDs remain above the new cursor. The device SHOULD sync
	// again immediately, hard-capped at 10 rounds per wake (§4.6).
	More bool `json:"more"`

	// Ayllu is present only when the request's ayllu_version differs from the
	// server's. It is exempt from the response byte budget (§4.6).
	Ayllu *Ayllu `json:"ayllu,omitempty"`

	// Config is device configuration pushed from wasi.toml (§13). It carries no
	// layout numbers: pagination and line breaking are device-owned (§4.9).
	Config *DeviceConfig `json:"config,omitempty"`
}

// AckStatus is the outcome of one outbound letter. All statuses are terminal.
type AckStatus string

const (
	// AckSent means handed to SMTP submission (§4.7 step 4).
	AckSent AckStatus = "sent"
	// AckRejectedInactive means the contact is a tombstone (§7.2).
	AckRejectedInactive AckStatus = "rejected_inactive"
	// AckRejectedUnknownContact means the contact id is not in the ayllu.
	AckRejectedUnknownContact AckStatus = "rejected_unknown_contact"
	// AckInvalid means the payload failed validation: empty body, over the
	// grapheme cap, or unknown fields (§4.7 step 2).
	AckInvalid AckStatus = "invalid"
)

// Ack reports the terminal outcome for one outbound local_id (§4.7).
type Ack struct {
	LocalID string    `json:"local_id"`
	Status  AckStatus `json:"status"`
}

// Letter is one inbound letter, derived at read time and never persisted by the
// server (I-1). The body is a single string; the device reflows it (§4.9).
type Letter struct {
	// ID is "l-" + the first 10 lowercase hex chars of SHA-256 over the raw
	// Message-ID header. Stable across UIDVALIDITY resets and window resyncs;
	// never exposes the raw header (§4.5).
	ID string `json:"id"`

	ContactID string `json:"contact_id"`

	// Subject is normalised per §5.4 and capped at 100 graphemes. Inbound
	// subjects are real and are never generated.
	Subject string `json:"subject"`

	// Date is the mailbox Date header, sanity-checked against INTERNALDATE.
	Date int64 `json:"date"`

	// Body is capped at max_letter_chars graphemes. The full text stays intact
	// in the mailbox and graduates with it (§5.2).
	Body string `json:"body"`

	// Trimmed reports that a quoted tail was removed by strip (§5.2).
	Trimmed bool `json:"trimmed"`

	// Truncated reports that the body was cut at max_letter_chars. Distinct from
	// Trimmed: the device may render them differently (§4.3).
	Truncated bool `json:"truncated"`

	// Degraded reports that strip was unreachable and the in-process fallback
	// rules were used. The next sync after the service returns re-derives it
	// cleanly (§5.3).
	Degraded bool `json:"degraded"`
}

// Ayllu is the device's view of the contact list. It carries no email addresses
// (I-2) and includes tombstones so the device can show a name on an old letter
// while hiding the person from the compose picker (§7.2).
type Ayllu struct {
	Version  int            `json:"version"`
	Contacts []AylluContact `json:"contacts"`
}

// AylluContact is one contact as the device sees it: identity, display name, the
// active flag, and the youth's cosmetic overlay. No address, ever (I-2).
type AylluContact struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Pinned bool   `json:"pinned"`
	Order  int    `json:"order"`
	// Portrait is a glyph identifier from the device's built-in set. Image bytes
	// never cross this wire in v1 (§4.3).
	Portrait string `json:"portrait"`
}

// DeviceConfig is the pushed device configuration block (§4.3, §13). It holds
// content knobs only; layout is device-owned (§4.9, A.10), so there is no
// chars_per_page and no page count anywhere.
type DeviceConfig struct {
	MaxLetterChars int    `json:"max_letter_chars"`
	SyncIntervalS  int    `json:"sync_interval_s"`
	RAT            string `json:"rat,omitempty"`
	Cover          string `json:"cover,omitempty"`
}

// MaxRequestBytes is the defensive server-side request cap (§4.1). A full
// 12-letter outbox is under 10 KB.
const MaxRequestBytes = 64 << 10

// MaxKipuBytes caps the kipu block (§4.8).
const MaxKipuBytes = 512

// MaxSubjectGraphemes is the wire cap on subjects, both directions (§4.6).
const MaxSubjectGraphemes = 100

// SysContactID is the reserved system contact that notice letters come from. It
// always resolves, cannot be deactivated, and cannot be written to (§7.4).
const SysContactID = "c_sys"
