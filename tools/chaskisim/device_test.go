package chaskisim

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

// syncServer builds an httptest.Server that decodes each request and
// delegates to handler for the response, serialising calls so handler can
// keep simple, non-atomic counters.
func syncServer(t *testing.T, handler func(protocol.Request) protocol.Response) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("server: decoding request: %v", err)
		}
		mu.Lock()
		resp := handler(req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("server: encoding response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openTestDevice(t *testing.T) *Device {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return d
}

func TestDevice_Compose_AssignsUniqueLocalIDs(t *testing.T) {
	d := openTestDevice(t)
	a := d.Compose("c_01", "", "hi", "")
	b := d.Compose("c_01", "", "bye", "")
	if a.LocalID == b.LocalID {
		t.Fatalf("two composed letters share local_id %q", a.LocalID)
	}
	if got := len(d.State().Outbox); got != 2 {
		t.Fatalf("outbox length = %d, want 2", got)
	}
}

func TestDevice_Wake_CapsAtMaxMoreRounds(t *testing.T) {
	srv := syncServer(t, func(req protocol.Request) protocol.Response {
		// A deliberately buggy server that never stops saying More: the
		// hard cap (§4.6) is the device's own defense, not something the
		// server is trusted to respect.
		return protocol.Response{Cursor: "c", More: true}
	})

	d := openTestDevice(t)
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	responses, err := d.Wake(context.Background(), c)
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if len(responses) != MaxMoreRounds {
		t.Fatalf("Wake made %d requests, want exactly the %d-round cap", len(responses), MaxMoreRounds)
	}
}

func TestDevice_Wake_StopsAssoonAsMoreIsFalse(t *testing.T) {
	calls := 0
	srv := syncServer(t, func(req protocol.Request) protocol.Response {
		calls++
		return protocol.Response{Cursor: "c", More: calls < 3}
	})

	d := openTestDevice(t)
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	responses, err := d.Wake(context.Background(), c)
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if len(responses) != 3 {
		t.Fatalf("Wake made %d requests, want 3 (stops the round after More: false)", len(responses))
	}
}

func TestDevice_SyncOnce_EveryAckIsTerminalRegardlessOfStatus(t *testing.T) {
	srv := syncServer(t, func(req protocol.Request) protocol.Response {
		var acks []protocol.Ack
		for _, o := range req.Outbound {
			status := protocol.AckSent
			if o.ContactID == "c_bad" {
				status = protocol.AckRejectedInactive
			}
			acks = append(acks, protocol.Ack{LocalID: o.LocalID, Status: status})
		}
		return protocol.Response{Cursor: "c", Acks: acks}
	})

	d := openTestDevice(t)
	good := d.Compose("c_good", "", "hi", "")
	bad := d.Compose("c_bad", "", "hi", "")

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	resp, err := d.SyncOnce(context.Background(), c)
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(resp.Acks) != 2 {
		t.Fatalf("acks = %v, want 2", resp.Acks)
	}

	if got := d.State().Outbox; len(got) != 0 {
		t.Fatalf("outbox after acks (sent=%q, rejected=%q) = %v, want empty — every ack is terminal (§4.7)", good.LocalID, bad.LocalID, got)
	}
}

func TestDevice_SyncOnce_UnackedOutboxSurvivesForResend(t *testing.T) {
	srv := syncServer(t, func(req protocol.Request) protocol.Response {
		// No acks at all this round — e.g. the letter simply hasn't been
		// processed yet from the device's point of view.
		return protocol.Response{Cursor: "c"}
	})

	d := openTestDevice(t)
	d.Compose("c_01", "", "still on the road", "")

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	if _, err := d.SyncOnce(context.Background(), c); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if got := len(d.State().Outbox); got != 1 {
		t.Fatalf("outbox after an ackless sync = %d entries, want 1 (there is no server-side queue, §4.7)", got)
	}
}

func TestDevice_ApplyResponse_CursorStoredAndEchoedVerbatim(t *testing.T) {
	// A cursor value chosen to look nothing like anything this package
	// would derive on its own — proof that it round-trips as an opaque
	// blob, not something decoded and re-encoded.
	const weirdCursor = "!!not-real-base64??...///"

	var secondRequestCursor string
	calls := 0
	srv := syncServer(t, func(req protocol.Request) protocol.Response {
		calls++
		if calls == 1 {
			return protocol.Response{Cursor: weirdCursor}
		}
		secondRequestCursor = req.Cursor
		return protocol.Response{Cursor: weirdCursor}
	})

	d := openTestDevice(t)
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	if _, err := d.SyncOnce(context.Background(), c); err != nil {
		t.Fatalf("first SyncOnce: %v", err)
	}
	if got := d.State().Cursor; got != weirdCursor {
		t.Fatalf("stored cursor = %q, want %q verbatim", got, weirdCursor)
	}
	if _, err := d.SyncOnce(context.Background(), c); err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	if secondRequestCursor != weirdCursor {
		t.Fatalf("second request echoed cursor %q, want %q verbatim", secondRequestCursor, weirdCursor)
	}
}

func TestDevice_ApplyResponse_AylluAndConfigCached(t *testing.T) {
	srv := syncServer(t, func(req protocol.Request) protocol.Response {
		return protocol.Response{
			Cursor: "c",
			Ayllu: &protocol.Ayllu{
				Version:  9,
				Contacts: []protocol.AylluContact{{ID: "c_01", Name: "Rosa"}},
			},
			Config: &protocol.DeviceConfig{MaxLetterChars: 500, SyncIntervalS: 21600},
		}
	})

	d := openTestDevice(t)
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	if _, err := d.SyncOnce(context.Background(), c); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	st := d.State()
	if st.AylluVersion != 9 || len(st.Ayllu) != 1 || st.Ayllu[0].Name != "Rosa" {
		t.Errorf("Ayllu state = %+v, want version 9 with Rosa", st)
	}
	if st.MaxLetterChars != 500 {
		t.Errorf("MaxLetterChars = %d, want 500", st.MaxLetterChars)
	}
}

// TestV21_DedupAcrossRestartWithRepeatedDelivery is the full-loop version of
// V-21: a server that (standing in for a UIDVALIDITY reset forcing a window
// resync) keeps re-delivering the same letters is exactly the scenario
// §4.5 requires the device to be safe under, and doing it across a real
// restart (a fresh Device Open on the same path, exactly as
// TestV21_DedupRingSurvivesRestart_ButFactoryResetClearsIt exercises in
// isolation) is what makes this the wire-level version of that guarantee.
func TestV21_DedupAcrossRestartWithRepeatedDelivery(t *testing.T) {
	redelivered := []protocol.Letter{
		{ID: "l-aaaaaaaaaa", ContactID: "c_01", Body: "hi"},
		{ID: "l-bbbbbbbbbb", ContactID: "c_01", Body: "there"},
	}
	srv := syncServer(t, func(req protocol.Request) protocol.Response {
		return protocol.Response{Cursor: "window-resync-cursor", Letters: redelivered}
	})
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	path := filepath.Join(t.TempDir(), "state.json")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := d1.SyncOnce(context.Background(), c); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if got := len(d1.State().Letters); got != 2 {
		t.Fatalf("letters after first sync = %d, want 2", got)
	}
	if err := d1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Restart: a fresh process opening the same state file.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	before := d2.State()
	resp, err := d2.SyncOnce(context.Background(), c)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := len(d2.State().Letters); got != 2 {
		t.Fatalf("letters after redelivery post-restart = %d, want still 2 (no duplicates)", got)
	}
	if newOnes := NewLetters(before, resp); len(newOnes) != 0 {
		t.Errorf("NewLetters after full redelivery = %v, want none — everything was already seen", newOnes)
	}
}

func TestNewLetters_ReportsOnlyGenuinelyNewOnes(t *testing.T) {
	before := State{SeenLetterIDs: []string{"l-old"}}
	resp := &protocol.Response{Letters: []protocol.Letter{
		{ID: "l-old", Body: "seen before"},
		{ID: "l-new", Body: "fresh"},
	}}
	got := NewLetters(before, resp)
	if len(got) != 1 || got[0].ID != "l-new" {
		t.Fatalf("NewLetters = %v, want just l-new", got)
	}
}
