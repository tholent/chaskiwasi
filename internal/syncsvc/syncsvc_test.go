package syncsvc

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
	"github.com/tholent/chaskiwasi/internal/state"
)

// harness wires a Handler to fakes plus a real state.FileStore, so the ack
// ring's fsync-then-ack ordering (§4.7) is exercised against real files.
type harness struct {
	t     *testing.T
	h     *Handler
	cfg   *fakeConfig
	ayllu *fakeAyllu
	mbox  *fakeMailbox
	sub   *fakeSubmitter
	kipu  *fakeKipu
	state *state.FileStore
	now   time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	hn := &harness{
		t:     t,
		cfg:   &fakeConfig{cfg: testConfig()},
		ayllu: testAyllu(),
		mbox:  &fakeMailbox{uidValidity: 42},
		sub:   &fakeSubmitter{},
		kipu:  &fakeKipu{},
		state: st,
		now:   time.Unix(1785420202, 0).UTC(),
	}

	h, err := New(Deps{
		Config:    hn.cfg,
		Ayllu:     hn.ayllu,
		State:     st,
		Mailbox:   hn.mbox,
		Submitter: hn.sub,
		Deriver:   fakeDeriver{},
		Kipu:      hn.kipu,
		Now:       func() time.Time { return hn.now },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hn.h = h
	return hn
}

// post sends req with a valid bearer token and returns the recorder.
func (hn *harness) post(req protocol.Request) *httptest.ResponseRecorder {
	hn.t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		hn.t.Fatalf("marshal request: %v", err)
	}
	return hn.postRaw(string(body), "Bearer "+testToken)
}

func (hn *harness) postRaw(body, authorization string) *httptest.ResponseRecorder {
	hn.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(body))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	hn.h.ServeHTTP(w, r)
	return w
}

// sync posts req, requires 200, and returns the decoded response.
func (hn *harness) sync(req protocol.Request) protocol.Response {
	hn.t.Helper()
	w := hn.post(req)
	if w.Code != http.StatusOK {
		hn.t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	var resp protocol.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		hn.t.Fatalf("decode response: %v", err)
	}
	return resp
}

func decodeInto(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func (hn *harness) currentCursor() string {
	hn.t.Helper()
	return encodeCursor(hn.mbox.uidValidity, 0)
}

// --- §4.1 transport -------------------------------------------------------

func TestAuth_RejectsAnythingButTheRightToken(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"correct token", "Bearer " + testToken, http.StatusOK},
		{"scheme is case-insensitive", "bearer " + testToken, http.StatusOK},
		{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"token prefix only", "Bearer test-device-toke", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + testToken, http.StatusUnauthorized},
		{"raw token, no scheme", testToken, http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hn := newHarness(t)
			w := hn.postRaw(`{"cursor":"","ayllu_version":7}`, tc.header)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}

func TestTransport_MethodAndSizeCap(t *testing.T) {
	hn := newHarness(t)

	r := httptest.NewRequest(http.MethodGet, "/sync", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	hn.h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", w.Code)
	}

	// A body past the 64 KB cap is refused rather than buffered (§4.1).
	oversized := fmt.Sprintf(`{"cursor":"","ayllu_version":7,"outbound":[{"local_id":"o-1","contact_id":"c_01","body":%q}]}`,
		strings.Repeat("x", protocol.MaxRequestBytes))
	if got := hn.postRaw(oversized, "Bearer "+testToken).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", got)
	}
}

func TestUnreachableMailbox_Returns503WithRetryAfter(t *testing.T) {
	hn := newHarness(t)
	hn.mbox.err = fmt.Errorf("dial tcp: %w", mailbox.ErrUnreachable)

	w := hn.post(protocol.Request{Cursor: "", AylluVersion: 7})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	retryAfter, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || retryAfter <= 0 {
		t.Fatalf("Retry-After = %q, want a positive integer", w.Header().Get("Retry-After"))
	}
}

// --- §4.4 cursor ----------------------------------------------------------

func TestCursor_RoundTrip(t *testing.T) {
	tests := []struct {
		name              string
		uidValidity, uid  uint32
		wantDecodeSuccess bool
	}{
		{name: "zero", wantDecodeSuccess: true},
		{name: "typical", uidValidity: 42, uid: 1207, wantDecodeSuccess: true},
		{name: "max", uidValidity: ^uint32(0), uid: ^uint32(0), wantDecodeSuccess: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotV, gotU, ok := decodeCursor(encodeCursor(tc.uidValidity, tc.uid))
			if ok != tc.wantDecodeSuccess || gotV != tc.uidValidity || gotU != tc.uid {
				t.Fatalf("round trip = (%d, %d, %v), want (%d, %d, %v)",
					gotV, gotU, ok, tc.uidValidity, tc.uid, tc.wantDecodeSuccess)
			}
		})
	}

	for _, bad := range []string{"", "!!!!", "AAAA", encodeCursor(1, 1) + "AA"} {
		if _, _, ok := decodeCursor(bad); ok {
			t.Fatalf("decodeCursor(%q) reported success, want failure", bad)
		}
	}
}

func TestV2_EmptyCursorIsBoundedByResyncWindow(t *testing.T) {
	hn := newHarness(t)
	hn.cfg.cfg.Sync.ResyncWindow = 3
	for uid := uint32(1); uid <= 10; uid++ {
		hn.mbox.messages = append(hn.mbox.messages, message(uid, 20))
	}

	resp := hn.sync(protocol.Request{Cursor: "", AylluVersion: 7})

	if len(hn.mbox.recentCalls) != 1 || hn.mbox.recentCalls[0] != 3 {
		t.Fatalf("Recent calls = %v, want exactly one call for 3 (resync_window)", hn.mbox.recentCalls)
	}
	if len(hn.mbox.aboveCalls) != 0 {
		t.Fatalf("FetchAbove calls = %v, want none on an empty cursor", hn.mbox.aboveCalls)
	}
	if len(resp.Letters) != 3 {
		t.Fatalf("letters = %d, want the 3 most recent", len(resp.Letters))
	}
	// The newest span, not the oldest: uids 8, 9, 10.
	if resp.Letters[0].ID != fmt.Sprintf("l-%08x", 8) {
		t.Fatalf("first letter = %s, want uid 8 (newest span)", resp.Letters[0].ID)
	}
}

func TestV21_StaleUIDValidityBehavesExactlyLikeAnEmptyCursor(t *testing.T) {
	hn := newHarness(t)
	hn.cfg.cfg.Sync.ResyncWindow = 2
	for uid := uint32(1); uid <= 5; uid++ {
		hn.mbox.messages = append(hn.mbox.messages, message(uid, 20))
	}

	// A cursor minted before a UIDVALIDITY reset: right shape, wrong validity.
	stale := encodeCursor(hn.mbox.uidValidity-1, 3)
	staleResp := hn.sync(protocol.Request{Cursor: stale, AylluVersion: 7})

	hn.mbox.recentCalls = nil
	hn.mbox.aboveCalls = nil
	emptyResp := hn.sync(protocol.Request{Cursor: "", AylluVersion: 7})

	if len(staleResp.Letters) != len(emptyResp.Letters) {
		t.Fatalf("stale cursor delivered %d letters, empty cursor %d — must be identical",
			len(staleResp.Letters), len(emptyResp.Letters))
	}
	for i := range staleResp.Letters {
		if staleResp.Letters[i].ID != emptyResp.Letters[i].ID {
			t.Fatalf("letter %d: stale %s, empty %s — must be identical",
				i, staleResp.Letters[i].ID, emptyResp.Letters[i].ID)
		}
	}
	if staleResp.Cursor != emptyResp.Cursor {
		t.Fatalf("cursor: stale %q, empty %q — must be identical", staleResp.Cursor, emptyResp.Cursor)
	}
	// And no error surfaced to the device: the reset is invisible on the wire.
	if _, _, ok := decodeCursor(staleResp.Cursor); !ok {
		t.Fatal("response cursor does not decode")
	}
}

func TestCursor_RequestIsAuthoritativeOverTheMirror(t *testing.T) {
	hn := newHarness(t)
	for uid := uint32(1); uid <= 4; uid++ {
		hn.mbox.messages = append(hn.mbox.messages, message(uid, 20))
	}

	// Drive the mirror forward to uid 4.
	first := hn.sync(protocol.Request{Cursor: encodeCursor(42, 0), AylluVersion: 7})
	if got := hn.state.Snapshot().LastUID; got != 4 {
		t.Fatalf("mirror last_uid = %d, want 4", got)
	}
	if len(first.Letters) != 4 {
		t.Fatalf("letters = %d, want 4", len(first.Letters))
	}

	// The device now asks from uid 2. The mirror says 4; the request wins.
	second := hn.sync(protocol.Request{Cursor: encodeCursor(42, 2), AylluVersion: 7})
	if len(second.Letters) != 2 {
		t.Fatalf("letters = %d, want 2 (uids 3 and 4) — the request cursor is authoritative",
			len(second.Letters))
	}
}

// --- §4.6 budget ----------------------------------------------------------

func TestV2_BudgetAssembly(t *testing.T) {
	tests := []struct {
		name        string
		budgetBytes int
		bodyLen     int
		messages    int
		wantLetters int
		wantMore    bool
	}{
		{
			name:        "everything fits",
			budgetBytes: 8192,
			bodyLen:     100,
			messages:    3,
			wantLetters: 3,
			wantMore:    false,
		},
		{
			name:        "budget stops assembly and sets more",
			budgetBytes: 2048,
			bodyLen:     600,
			messages:    3,
			wantLetters: 2,
			wantMore:    true,
		},
		{
			name: "always at least one complete letter, however small the budget",
			// A budget below the size of a single letter must still ship one:
			// letters are atomic on the wire and the budget is a cost target,
			// not a transport ceiling (§4.6).
			budgetBytes: 1,
			bodyLen:     600,
			messages:    3,
			wantLetters: 1,
			wantMore:    true,
		},
		{
			name:        "one letter, one message, nothing left",
			budgetBytes: 1,
			bodyLen:     600,
			messages:    1,
			wantLetters: 1,
			wantMore:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hn := newHarness(t)
			hn.cfg.cfg.Sync.BudgetBytes = tc.budgetBytes
			for uid := uint32(1); uid <= uint32(tc.messages); uid++ {
				hn.mbox.messages = append(hn.mbox.messages, message(uid, tc.bodyLen))
			}

			resp := hn.sync(protocol.Request{Cursor: encodeCursor(42, 0), AylluVersion: 7})

			if len(resp.Letters) != tc.wantLetters {
				t.Fatalf("letters = %d, want %d", len(resp.Letters), tc.wantLetters)
			}
			if resp.More != tc.wantMore {
				t.Fatalf("more = %v, want %v", resp.More, tc.wantMore)
			}
			// Whatever was delivered arrived whole: no partial bodies.
			for _, l := range resp.Letters {
				if len(l.Body) != tc.bodyLen {
					t.Fatalf("letter %s body = %d bytes, want %d — letters are never split",
						l.ID, len(l.Body), tc.bodyLen)
				}
			}
		})
	}
}

func TestBudget_MoreDrainsToCompletion(t *testing.T) {
	hn := newHarness(t)
	hn.cfg.cfg.Sync.BudgetBytes = 1
	for uid := uint32(1); uid <= 5; uid++ {
		hn.mbox.messages = append(hn.mbox.messages, message(uid, 400))
	}

	cursor := hn.currentCursor()
	var delivered []string
	for round := 0; round < 10; round++ {
		resp := hn.sync(protocol.Request{Cursor: cursor, AylluVersion: 7})
		for _, l := range resp.Letters {
			delivered = append(delivered, l.ID)
		}
		cursor = resp.Cursor
		if !resp.More {
			break
		}
	}

	if len(delivered) != 5 {
		t.Fatalf("drained %d letters in <=10 rounds, want 5: %v", len(delivered), delivered)
	}
	for i, id := range delivered {
		if want := fmt.Sprintf("l-%08x", i+1); id != want {
			t.Fatalf("letter %d = %s, want %s (UID order, no repeats)", i, id, want)
		}
	}
}

// --- §4.3 ayllu and config blocks -----------------------------------------

func TestAylluBlock_OnlyOnVersionChangeAndNeverAnAddress(t *testing.T) {
	hn := newHarness(t)

	current := hn.sync(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 7})
	if current.Ayllu != nil {
		t.Fatal("ayllu block shipped to a device already on the current version")
	}

	stale := hn.sync(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 6})
	if stale.Ayllu == nil {
		t.Fatal("ayllu block missing for a device on an older version")
	}
	if stale.Ayllu.Version != 7 || len(stale.Ayllu.Contacts) != 2 {
		t.Fatalf("ayllu = version %d with %d contacts, want version 7 with 2",
			stale.Ayllu.Version, len(stale.Ayllu.Contacts))
	}

	// Tombstones ship so the device can name an old letter's sender while
	// hiding them from the compose picker (§7.2).
	var sawTombstone bool
	for _, c := range stale.Ayllu.Contacts {
		if c.ID == "c_07" && !c.Active {
			sawTombstone = true
		}
	}
	if !sawTombstone {
		t.Fatal("tombstone c_07 absent from the ayllu block")
	}

	// I-2: no address anywhere in the response, in any block.
	raw := hn.post(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 6}).Body.String()
	for _, addr := range []string{"abuela@example.test", "rosa@example.test", "@example.test"} {
		if strings.Contains(raw, addr) {
			t.Fatalf("response carries an email address (%q) — I-2 violation", addr)
		}
	}
}

func TestConfigBlock_CarriesNoLayoutNumbers(t *testing.T) {
	hn := newHarness(t)
	w := hn.post(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 7})

	var envelope struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Config == nil {
		t.Fatal("config block absent")
	}

	want := map[string]bool{"max_letter_chars": true, "sync_interval_s": true, "rat": true, "cover": true}
	for key := range envelope.Config {
		if !want[key] {
			// Layout is device-owned: no pages, no chars_per_page, ever
			// (§4.9, A.10).
			t.Fatalf("config carries unexpected key %q — layout numbers never live here", key)
		}
	}
	if envelope.Config["max_letter_chars"] != float64(500) {
		t.Fatalf("max_letter_chars = %v, want 500", envelope.Config["max_letter_chars"])
	}
}

// --- §4.8 kipu and §10.3 counter ------------------------------------------

func TestKipu_AcceptedAndNeverFailsASync(t *testing.T) {
	hn := newHarness(t)
	hn.kipu.err = fmt.Errorf("disk on fire")

	block := map[string]any{"battery_pct": float64(84), "rat": "ltem", "unknown_future_field": "kept"}
	resp := hn.sync(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 7, Kipu: block})

	if resp.ServerTime == 0 {
		t.Fatal("sync did not complete")
	}
	if len(hn.kipu.blocks) != 1 {
		t.Fatalf("kipu blocks recorded = %d, want 1", len(hn.kipu.blocks))
	}
	if got := hn.kipu.blocks[0]["unknown_future_field"]; got != "kept" {
		t.Fatalf("unknown kipu field = %v, want it preserved as received", got)
	}
}

func TestV20_PututuCounterReconciliation(t *testing.T) {
	hn := newHarness(t)

	// A device reporting a higher counter than the server holds — the shape of
	// a state.json restored from an older backup — jumps the server forward.
	resp := hn.sync(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 7, PututuCounterSeen: 41})
	if resp.PututuCounter != 41 {
		t.Fatalf("pututu_counter = %d, want 41", resp.PututuCounter)
	}
	if got := hn.state.Snapshot().PututuCounter; got != 41 {
		t.Fatalf("persisted counter = %d, want 41", got)
	}

	// A lower report is stale or lying and never moves the counter backward.
	resp = hn.sync(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 7, PututuCounterSeen: 3})
	if resp.PututuCounter != 41 {
		t.Fatalf("pututu_counter = %d after a lower report, want 41", resp.PututuCounter)
	}
}

func TestLastSyncAt_IsRecorded(t *testing.T) {
	hn := newHarness(t)
	hn.sync(protocol.Request{Cursor: hn.currentCursor(), AylluVersion: 7})
	if got := hn.state.Snapshot().LastSyncAt; !got.Equal(hn.now) {
		t.Fatalf("last_sync_at = %v, want %v", got, hn.now)
	}
}
