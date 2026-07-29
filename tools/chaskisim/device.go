package chaskisim

import (
	"context"
	"sync"
	"time"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

// MaxMoreRounds is the wire contract's hard cap on how many immediate
// resync rounds one wake may run when the server keeps saying more: true
// (§4.6) — "a defense against server bugs," in the spec's own words. A
// drain loop that hit this cap counts as ONE "recently synced" event for
// pututu coalescing, not MaxMoreRounds of them (§4.6, §10.1); Device does
// not implement coalescing itself (that's server-side, §10.1), but Wake's
// single-wake framing is what makes that distinction meaningful to model.
const MaxMoreRounds = 10

// Device is a simulated Chaski: the persisted State plus the behaviour the
// wire contract requires firmware to have around it. It is not safe to share
// a single on-disk path between two concurrently-running Devices — like a
// real device, there is exactly one of these per state file — but it is
// safe for concurrent use by multiple goroutines within one process.
type Device struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	state *State
}

// Open loads path, or starts a fresh simulated device if it does not exist
// yet. Call (*Device).Save to persist any change back to path — nothing
// here auto-saves, so a caller that wants restart-durable behaviour (as
// V-21 requires for the dedup ring) must call Save after every operation
// that should survive a "restart" of this process.
func Open(path string) (*Device, error) {
	st, err := loadState(path)
	if err != nil {
		return nil, err
	}
	return &Device{path: path, now: time.Now, state: st}, nil
}

// Save persists the device's current state to its path (temp file, fsync,
// rename, fsync directory — the same discipline the server itself uses for
// state.json).
func (d *Device) Save() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return saveState(d.path, d.state)
}

// Reset simulates a factory reset: every piece of local memory is wiped,
// including the dedup ring, matching what a real factory reset does to
// flash. This is deliberately NOT what happens on an ordinary process
// restart (Open reloads the existing file untouched) or on a server-side
// UIDVALIDITY reset (§4.4) — neither of those is a factory reset, and V-21
// specifically depends on the dedup ring surviving both of those, unlike
// this. Call Save afterward to make the reset durable.
func (d *Device) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = freshState()
}

// State returns a snapshot of the device's current state, safe for a caller
// to read (e.g. the `dump` CLI command, or an e2e assertion) without racing
// further Device calls. Slices are shared, not deep-copied; callers must
// treat them as read-only.
func (d *Device) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := *d.state
	return snapshot
}

// Compose adds a child-authored letter to the outbox (§4.2). It does not
// send anything — there is no server-side outbound queue (§4.7); the
// letter is sent on the next Sync/Wake and stays in the outbox, resent
// every sync, until any ack (sent or rejected) removes it. localID, if
// empty, is assigned by the device (unique and stable across restarts);
// callers exercising a specific local_id (e.g. to test idempotent replay,
// V-9) may pass their own.
func (d *Device) Compose(contactID, subject, body, localID string) protocol.Outbound {
	d.mu.Lock()
	defer d.mu.Unlock()

	if localID == "" {
		localID = d.state.nextLocalID()
	}
	letter := protocol.Outbound{
		LocalID:   localID,
		ContactID: contactID,
		Subject:   subject,
		Body:      body,
	}
	d.state.Outbox = append(d.state.Outbox, letter)
	return letter
}

// SyncOnce performs exactly one POST /sync round trip: build the request
// from current state, send it, apply the response. It never loops on More —
// see Wake for the firmware-required drain loop (§4.6).
func (d *Device) SyncOnce(ctx context.Context, c *Client) (*protocol.Response, error) {
	d.mu.Lock()
	req := d.buildRequestLocked()
	d.mu.Unlock()

	resp, err := c.Sync(ctx, req)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	d.applyResponseLocked(resp)
	d.mu.Unlock()

	return resp, nil
}

// Wake runs one whole wake: SyncOnce, then SyncOnce again immediately for
// every more: true, until more: false or MaxMoreRounds rounds have run
// (§4.6). It returns every response in order, even if a later round errors —
// a caller can inspect how far a wake got before a transport failure cut it
// short.
//
// Ten rounds hitting the cap and 0-9 rounds ending naturally are both
// success from Wake's point of view: hitting the cap is the documented
// defense against a server bug looping forever, not itself an error.
func (d *Device) Wake(ctx context.Context, c *Client) ([]*protocol.Response, error) {
	var responses []*protocol.Response
	for i := 0; i < MaxMoreRounds; i++ {
		resp, err := d.SyncOnce(ctx, c)
		if err != nil {
			return responses, err
		}
		responses = append(responses, resp)
		if !resp.More {
			break
		}
	}
	return responses, nil
}

// buildRequestLocked assembles the next sync request from current state.
// Caller must hold mu.
func (d *Device) buildRequestLocked() protocol.Request {
	return protocol.Request{
		// §4.4: stored and echoed verbatim, never parsed — see State.Cursor.
		Cursor:            d.state.Cursor,
		AylluVersion:      d.state.AylluVersion,
		PututuCounterSeen: d.state.PututuCounterSeen,
		Outbound:          append([]protocol.Outbound(nil), d.state.Outbox...),
	}
}

// applyResponseLocked updates state from one sync response. Caller must
// hold mu. This is where every firmware requirement in the package doc
// except Wake's round loop and pututu's SMS path actually happens.
func (d *Device) applyResponseLocked(resp *protocol.Response) {
	// §4.4: the cursor is opaque; store exactly what was received.
	d.state.Cursor = resp.Cursor

	// §4.7: every ack is terminal, whatever its status — the letter leaves
	// the outbox and is never resent, full stop. A rejection is not
	// distinguished from a success here because the wire contract does not
	// distinguish them for this purpose; a caller inspecting resp.Acks
	// directly can still see which was which for display purposes.
	if len(resp.Acks) > 0 {
		acked := make(map[string]bool, len(resp.Acks))
		for _, ack := range resp.Acks {
			acked[ack.LocalID] = true
		}
		kept := d.state.Outbox[:0]
		for _, o := range d.state.Outbox {
			if !acked[o.LocalID] {
				kept = append(kept, o)
			}
		}
		d.state.Outbox = kept
	}

	// §4.5: silently drop anything already seen; remember the rest.
	for _, letter := range resp.Letters {
		if d.state.hasSeen(letter.ID) {
			continue
		}
		d.state.markSeen(letter.ID)
		d.state.Letters = append(d.state.Letters, letter)
	}

	if resp.Ayllu != nil {
		d.state.AylluVersion = resp.Ayllu.Version
		d.state.Ayllu = resp.Ayllu.Contacts
	}
	if resp.Config != nil {
		d.state.MaxLetterChars = resp.Config.MaxLetterChars
	}
}

// NewLetters returns the letters delivered by resp that were NOT already in
// the dedup ring at the time applyResponseLocked ran for it — i.e. exactly
// what a real device would newly render. It is a convenience for callers
// (the CLI, e2e assertions) that want "what's new this round" without
// re-deriving it from a before/after State diff themselves.
func NewLetters(before State, resp *protocol.Response) []protocol.Letter {
	seen := make(map[string]bool, len(before.SeenLetterIDs))
	for _, id := range before.SeenLetterIDs {
		seen[id] = true
	}
	var out []protocol.Letter
	for _, l := range resp.Letters {
		if !seen[l.ID] {
			out = append(out, l)
			seen[l.ID] = true // guards against the response itself repeating an id
		}
	}
	return out
}
