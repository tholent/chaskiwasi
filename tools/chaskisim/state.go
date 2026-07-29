package chaskisim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tholent/chaskiwasi/internal/atomicfile"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// SeenLetterIDCap is the minimum dedup memory the wire contract requires:
// firmware MUST remember at least its 1000 most recently seen letter ids and
// silently drop repeats (§4.5). This simulator keeps exactly the minimum, on
// purpose — it is the boundary the spec actually guarantees, so a test that
// only passes with a larger ring would be testing this simulator, not the
// contract.
const SeenLetterIDCap = 1000

// PututuWakeRateLimit independently rate-limits SMS-triggered wakes to at
// most one per 5 minutes, regardless of token validity (§10.2) — a defense
// against a validation bug becoming a battery- or balance-drain attack.
const PututuWakeRateLimit = 5 * time.Minute

// State is chaskisim's whole persisted view of one simulated device. It is
// the thing that survives a "restart" (a fresh Device Open on the same
// path) — modelling flash, not RAM.
//
// Load/Save are the only supported way in or out of durable storage; hand
// construction is fine for tests that never save, but anything that gets
// persisted and reloaded should go through them so the dedup index stays
// consistent with SeenLetterIDs.
type State struct {
	// Cursor is opaque to the device by contract (§4.4): a base64 string the
	// server mints. This field exists ONLY to be stored verbatim and echoed
	// back unchanged on the next request. No code in this package ever
	// base64-decodes it, inspects its length, or derives anything from its
	// contents — doing so would be firmware reaching into a field the
	// contract explicitly tells it not to understand, and would be exactly
	// the kind of coupling that makes a future cursor-encoding change (like
	// A.9 changing the encoded (uidvalidity, uid) pair) a firmware release
	// instead of a server-only change.
	Cursor string `json:"cursor"`

	// AylluVersion is the last contact-list revision this device has
	// applied; sent on every request so the server knows whether to include
	// the Ayllu block (§4.2, §4.3).
	AylluVersion int                     `json:"ayllu_version"`
	Ayllu        []protocol.AylluContact `json:"ayllu,omitempty"`

	// SeenLetterIDs is the device's dedup memory (§4.5), oldest first,
	// capped at SeenLetterIDCap. The server MAY re-send any previously
	// delivered letter at any time; this is what makes that safe.
	SeenLetterIDs []string `json:"seen_letter_ids,omitempty"`

	// Outbox holds composed letters not yet acked. There is no server-side
	// outbound queue (§4.7): this IS the queue. Anything here is sent again
	// on every sync until an ack of ANY status removes it — sent and every
	// rejection are equally terminal.
	Outbox []protocol.Outbound `json:"outbox,omitempty"`

	// OutboxSeq is the next local_id suffix to assign (nextLocalID),
	// persisted so local_ids stay unique across restarts, not just within
	// one process's lifetime.
	OutboxSeq int `json:"outbox_seq"`

	// Letters holds bodies actually delivered to this simulated device,
	// post-dedup, for demonstration and assertion purposes (the `dump`
	// command, and what test/e2e reads to assert "device saw exactly these
	// letters"). Persisting delivered content is a normal thing for a real
	// Chaski to do — I-1 constrains Wasi, the server, not the device it
	// talks to.
	Letters []protocol.Letter `json:"letters,omitempty"`

	// MaxLetterChars mirrors the last config pushed by the server (§4.3).
	// Advisory only: composing a letter longer than this is still allowed
	// locally (Compose does not enforce it) because the server is the
	// authority that actually validates and acks "invalid" (§4.7) — a
	// client-side cap here would just be a second, potentially stale, copy
	// of a rule the wire contract already places server-side.
	MaxLetterChars int `json:"max_letter_chars,omitempty"`

	// PututuCounterSeen is the highest SMS doorbell counter this device has
	// accepted (§10.2): verified by MAC, strictly greater than the previous
	// value, persisted across power loss. It advances ONLY via AcceptPututu
	// — never from a sync response — because accepting a counter the device
	// has not itself verified would defeat the point of the MAC check.
	// Reported to the server on every request so a server counter rolled
	// back by a state.json restore can heal (§10.3).
	PututuCounterSeen uint64 `json:"pututu_counter_seen"`

	// LastPututuWakeAt backs PututuWakeRateLimit.
	LastPututuWakeAt time.Time `json:"last_pututu_wake_at,omitempty"`

	seenIndex map[string]bool // rebuilt by rebuildIndex; never persisted
}

// freshState returns a zero-value State with its index initialised — what
// both a brand-new device and a factory reset start from.
func freshState() *State {
	return &State{seenIndex: make(map[string]bool)}
}

// hasSeen reports whether letterID is already in the dedup ring (§4.5).
func (s *State) hasSeen(letterID string) bool {
	return s.seenIndex[letterID]
}

// markSeen records letterID as seen, evicting the oldest entry once the ring
// is at SeenLetterIDCap.
func (s *State) markSeen(letterID string) {
	if s.seenIndex == nil {
		s.seenIndex = make(map[string]bool)
	}
	if s.seenIndex[letterID] {
		return
	}
	s.SeenLetterIDs = append(s.SeenLetterIDs, letterID)
	s.seenIndex[letterID] = true
	if over := len(s.SeenLetterIDs) - SeenLetterIDCap; over > 0 {
		for _, evicted := range s.SeenLetterIDs[:over] {
			delete(s.seenIndex, evicted)
		}
		s.SeenLetterIDs = s.SeenLetterIDs[over:]
	}
}

// nextLocalID mints a device-assigned outbound id, unique per letter and
// stable across restarts because outboxSeq is itself persisted (§4.2:
// local_id must be unique per letter, <=32 bytes).
func (s *State) nextLocalID() string {
	s.OutboxSeq++
	return fmt.Sprintf("o-%06d", s.OutboxSeq)
}

// rebuildIndex reconstructs the dedup lookup index after a JSON load, since
// the index itself is never persisted (it's redundant with SeenLetterIDs and
// persisting it would just be another place for the two to drift apart).
func (s *State) rebuildIndex() {
	s.seenIndex = make(map[string]bool, len(s.SeenLetterIDs))
	for _, id := range s.SeenLetterIDs {
		s.seenIndex[id] = true
	}
}

// loadState reads path, or returns a fresh zero-value State if it does not
// exist yet — a missing file is a brand-new simulated device, not an error.
func loadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return freshState(), nil
	case err != nil:
		return nil, fmt.Errorf("chaskisim: reading %s: %w", path, err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("chaskisim: parsing %s: %w", path, err)
	}
	st.rebuildIndex()
	return &st, nil
}

// saveState writes st to path atomically (temp file, fsync, rename, fsync
// directory — internal/atomicfile, the same discipline Wasi's own state.json
// uses), creating the parent directory if needed so a first save into a
// fresh working directory just works.
func saveState(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("chaskisim: creating %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("chaskisim: marshalling state: %w", err)
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("chaskisim: writing %s: %w", path, err)
	}
	return nil
}
