package state

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- AckRing -----------------------------------------------------------

func TestAckRing_LookupFindsRecorded(t *testing.T) {
	var r AckRing
	r.Record("o-1", "sent", time.Unix(100, 0))
	r.Record("o-2", "rejected_inactive", time.Unix(200, 0))

	got, ok := r.Lookup("o-2")
	if !ok {
		t.Fatal("Lookup(o-2) = not found, want found")
	}
	if got.Status != "rejected_inactive" {
		t.Errorf("Status = %q, want %q", got.Status, "rejected_inactive")
	}

	if _, ok := r.Lookup("o-nonexistent"); ok {
		t.Error("Lookup(o-nonexistent) = found, want not found")
	}
}

// TestAckRing_RejectionsAreTerminalOnReplay backs V-9: a replayed sync must
// get the SAME terminal ack, not a fresh retry, even when that ack was a
// rejection.
func TestAckRing_RejectionsAreTerminalOnReplay(t *testing.T) {
	var r AckRing
	r.Record("o-1", "rejected_unknown_contact", time.Unix(100, 0))

	first, ok := r.Lookup("o-1")
	if !ok {
		t.Fatal("expected the rejection to be recorded")
	}

	// A "replay" here is just looking it up again — the point is that nothing
	// about a second Lookup call re-processes or changes the entry.
	second, ok := r.Lookup("o-1")
	if !ok || second != first {
		t.Fatalf("replayed Lookup = %+v, want identical %+v", second, first)
	}
}

func TestAckRing_BoundedWithFIFOEviction(t *testing.T) {
	var r AckRing
	for i := 0; i < AckRingSize+10; i++ {
		r.Record(localID(i), "sent", time.Unix(int64(i), 0))
	}

	if got := len(r.Entries); got != AckRingSize {
		t.Fatalf("len(Entries) = %d, want capped at %d", got, AckRingSize)
	}

	// The oldest 10 entries (local_id 0..9) must have been evicted.
	for i := 0; i < 10; i++ {
		if _, ok := r.Lookup(localID(i)); ok {
			t.Errorf("evicted entry %s still present", localID(i))
		}
	}
	// The most recent AckRingSize entries must still be there, oldest-first.
	if _, ok := r.Lookup(localID(10)); !ok {
		t.Error("oldest surviving entry (10) missing")
	}
	if _, ok := r.Lookup(localID(AckRingSize + 9)); !ok {
		t.Error("newest entry missing")
	}
	if r.Entries[0].LocalID != localID(10) {
		t.Errorf("Entries[0].LocalID = %q, want %q (FIFO order preserved)", r.Entries[0].LocalID, localID(10))
	}
}

func localID(i int) string {
	return "o-" + strconv.Itoa(i)
}

// --- Pending notices -----------------------------------------------------

func TestPendingNotices_AddAndRemove(t *testing.T) {
	var s State
	s.AddPendingNotice(PendingNotice{ID: "n-1", ContactID: "c_07", Action: "add"})
	s.AddPendingNotice(PendingNotice{ID: "n-2", ContactID: "c_08", Action: "deactivate"})

	if len(s.PendingNotices) != 2 {
		t.Fatalf("len(PendingNotices) = %d, want 2", len(s.PendingNotices))
	}

	s.RemovePendingNotice("n-1")
	if len(s.PendingNotices) != 1 {
		t.Fatalf("len(PendingNotices) after remove = %d, want 1", len(s.PendingNotices))
	}
	if s.PendingNotices[0].ID != "n-2" {
		t.Fatalf("remaining notice = %+v, want n-2", s.PendingNotices[0])
	}
}

func TestPendingNotices_RemoveUnknownIDIsNoOp(t *testing.T) {
	var s State
	s.AddPendingNotice(PendingNotice{ID: "n-1"})

	s.RemovePendingNotice("does-not-exist")

	if len(s.PendingNotices) != 1 {
		t.Fatalf("len(PendingNotices) = %d, want unchanged 1", len(s.PendingNotices))
	}
}

// TestPendingNotices_RemoveIsIdempotent backs the crash-and-retry story in
// §7.6: removing an already-removed id must not error or panic, so a flush
// interrupted after removal but before some other bookkeeping can safely run
// again.
func TestPendingNotices_RemoveIsIdempotent(t *testing.T) {
	var s State
	s.AddPendingNotice(PendingNotice{ID: "n-1"})
	s.RemovePendingNotice("n-1")
	s.RemovePendingNotice("n-1") // must not panic or error
	if len(s.PendingNotices) != 0 {
		t.Fatalf("len(PendingNotices) = %d, want 0", len(s.PendingNotices))
	}
}

// --- Pututu counter --------------------------------------------------------

func TestPututuCounter_NextIncrements(t *testing.T) {
	var s State
	if got := s.NextPututuCounter(); got != 1 {
		t.Fatalf("first NextPututuCounter() = %d, want 1", got)
	}
	if got := s.NextPututuCounter(); got != 2 {
		t.Fatalf("second NextPututuCounter() = %d, want 2", got)
	}
}

// TestPututuCounter_ReconcileJumpsForward backs V-20: restoring state.json
// from an older backup rolls the counter back, and a device report above the
// server's value must heal it forward.
func TestPututuCounter_ReconcileJumpsForward(t *testing.T) {
	s := State{PututuCounter: 5}
	s.ReconcilePututuCounter(41)
	if s.PututuCounter != 41 {
		t.Fatalf("PututuCounter = %d, want 41", s.PututuCounter)
	}
}

func TestPututuCounter_ReconcileNeverMovesBackward(t *testing.T) {
	s := State{PututuCounter: 41}
	s.ReconcilePututuCounter(5)
	if s.PututuCounter != 41 {
		t.Fatalf("PututuCounter = %d, want unchanged 41 (a lower report must never roll the counter back)", s.PututuCounter)
	}
}

// --- I-1: state.json holds ids, integers, and timestamps only ------------

// TestI1_TopLevelKeysAreAllowlisted marshals a fully-populated State and
// checks its top-level JSON keys against a fixed allowlist. Any new
// top-level field on State must be added here deliberately — the point is to
// force a human to look at this test (and I-1) before a new field can ship.
func TestI1_TopLevelKeysAreAllowlisted(t *testing.T) {
	full := State{
		UIDValidity:   7,
		LastUID:       1234,
		PututuCounter: 41,
		LastSyncAt:    time.Now(),
		Acks: AckRing{Entries: []AckEntry{
			{LocalID: "o-1", Status: "sent", At: time.Now()},
		}},
		PendingNotices: []PendingNotice{
			{ID: "n-1", At: time.Now(), Action: "add", ContactID: "c_07", Name: "Rosa", Actor: "dad"},
		},
	}

	data, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}

	allowed := map[string]bool{
		"uidvalidity":     true,
		"last_uid":        true,
		"acks":            true,
		"pututu_counter":  true,
		"last_sync_at":    true,
		"pending_notices": true,
	}
	for k := range raw {
		if !allowed[k] {
			t.Errorf("unexpected top-level key %q in state.json — review against I-1 before adding", k)
		}
	}
	for k := range allowed {
		if _, ok := raw[k]; !ok {
			t.Errorf("expected key %q missing from marshaled output; test fixture and struct have drifted", k)
		}
	}
}

// TestI1_NoContentShapedFieldNames walks every json-tagged field reachable
// from State (recursing into structs and slices) and fails if any field name
// contains a substring that would suggest letter content or an address —
// "body", "subject", "address", "email" — the exact shapes I-1 forbids. This
// is a structural guard: it does not need to enumerate every possible bad
// value, only catch a field that should never have been added in the first
// place.
func TestI1_NoContentShapedFieldNames(t *testing.T) {
	banned := []string{"body", "subject", "address", "email", "content"}

	for _, name := range jsonFieldNames(reflect.TypeOf(State{})) {
		lower := strings.ToLower(name)
		for _, b := range banned {
			if strings.Contains(lower, b) {
				t.Errorf("state.json field %q looks content-shaped (matches %q) — this must never be persisted (I-1)", name, b)
			}
		}
	}
}

// jsonFieldNames recursively collects every json tag name reachable from t,
// following pointers, slices, and arrays into nested structs.
func jsonFieldNames(t reflect.Type) []string {
	switch t.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Array:
		return jsonFieldNames(t.Elem())
	case reflect.Struct:
		var names []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "" || name == "-" {
				name = f.Name
			}
			names = append(names, name)
			names = append(names, jsonFieldNames(f.Type)...)
		}
		return names
	default:
		return nil
	}
}
