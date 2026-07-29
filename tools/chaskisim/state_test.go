package chaskisim

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestState_MarkSeen_EvictsOldestPastCap(t *testing.T) {
	s := freshState()
	for i := 0; i < SeenLetterIDCap+10; i++ {
		s.markSeen(fmt.Sprintf("l-%04d", i))
	}
	if len(s.SeenLetterIDs) != SeenLetterIDCap {
		t.Fatalf("ring length = %d, want %d", len(s.SeenLetterIDs), SeenLetterIDCap)
	}
	// The oldest 10 must have been evicted; the ring must still recognise
	// the most recent SeenLetterIDCap ids.
	if s.hasSeen("l-0000") {
		t.Errorf("l-0000 still marked seen, want evicted")
	}
	if !s.hasSeen(fmt.Sprintf("l-%04d", SeenLetterIDCap+9)) {
		t.Errorf("most recently seen id was not retained")
	}
}

func TestState_MarkSeen_DuplicateIsNoOp(t *testing.T) {
	s := freshState()
	s.markSeen("l-1")
	s.markSeen("l-1")
	if len(s.SeenLetterIDs) != 1 {
		t.Errorf("ring = %v, want a single entry after marking the same id twice", s.SeenLetterIDs)
	}
}

func TestLoadSaveState_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s := freshState()
	s.Cursor = "opaque-cursor-value"
	s.markSeen("l-a")
	s.markSeen("l-b")
	s.PututuCounterSeen = 7
	localID := s.nextLocalID()

	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	loaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if loaded.Cursor != s.Cursor {
		t.Errorf("Cursor = %q, want %q", loaded.Cursor, s.Cursor)
	}
	if loaded.PututuCounterSeen != 7 {
		t.Errorf("PututuCounterSeen = %d, want 7", loaded.PututuCounterSeen)
	}
	if !loaded.hasSeen("l-a") || !loaded.hasSeen("l-b") {
		t.Errorf("dedup ring not restored: %v", loaded.SeenLetterIDs)
	}
	// nextLocalID must continue from where it left off, not restart at 1 —
	// otherwise a restarted device could mint a local_id it already used.
	next := loaded.nextLocalID()
	if next == localID {
		t.Errorf("nextLocalID after reload = %q, collides with pre-reload id %q", next, localID)
	}
}

func TestLoadState_MissingFileIsFreshDevice(t *testing.T) {
	s, err := loadState(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("loadState of a missing file: %v", err)
	}
	if s.Cursor != "" || len(s.SeenLetterIDs) != 0 {
		t.Errorf("fresh state = %+v, want zero-valued", s)
	}
}

// TestV21_DedupRingSurvivesRestart_ButFactoryResetClearsIt is this
// package's half of V-21: "UIDVALIDITY reset mid-life -> next sync behaves
// as window resync; device dedup leaves no duplicates." The mailbox/cursor
// half is server-side; what belongs here is proving the dedup ring itself
// is durable across a process restart (an ordinary Open on the same path)
// and is the thing that makes re-delivery harmless — while also proving
// Reset (factory reset) is a deliberately different, stronger operation that
// clears it, so the two are never confused with each other.
func TestV21_DedupRingSurvivesRestart_ButFactoryResetClearsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d1.mu.Lock()
	d1.state.markSeen("l-already-delivered")
	d1.state.Cursor = "stale-cursor"
	d1.mu.Unlock()
	if err := d1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate a restart: a fresh Device over the same path.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	after := d2.State()
	if !after.hasSeen("l-already-delivered") {
		t.Fatalf("dedup ring did not survive a restart")
	}
	if after.Cursor != "stale-cursor" {
		t.Fatalf("cursor did not survive a restart")
	}

	// A factory reset, in contrast, wipes everything — including the ring —
	// which is the correct, different behaviour: a real factory reset
	// blanks flash, and the device is expected to legitimately re-render
	// its window-resync span with no memory of having seen any of it.
	d2.Reset()
	afterReset := d2.State()
	if afterReset.hasSeen("l-already-delivered") {
		t.Errorf("dedup ring survived Reset, want cleared")
	}
	if afterReset.Cursor != "" {
		t.Errorf("cursor survived Reset, want cleared")
	}
}
