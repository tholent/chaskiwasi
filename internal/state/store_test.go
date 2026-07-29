package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOpen_MissingFileIsColdStartNotError(t *testing.T) {
	dir := t.TempDir()
	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open on empty dir: %v", err)
	}
	got := fs.Snapshot()
	want := State{}
	if got.PututuCounter != want.PututuCounter || len(got.Acks.Entries) != 0 || len(got.PendingNotices) != 0 {
		t.Fatalf("Snapshot() = %+v, want a zero State", got)
	}
}

func TestOpen_CorruptFileFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := Open(dir)
	if err == nil {
		t.Fatal("expected Open to fail loudly on a corrupt state.json, not silently reset")
	}
}

func TestOpen_LoadsExistingState(t *testing.T) {
	dir := t.TempDir()
	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fs.Update(func(s *State) error {
		s.NextPututuCounter()
		s.NextPututuCounter()
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if got := reopened.Snapshot().PututuCounter; got != 2 {
		t.Fatalf("reopened PututuCounter = %d, want 2", got)
	}
}

func TestUpdate_PersistsAndPublishes(t *testing.T) {
	dir := t.TempDir()
	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := fs.Update(func(s *State) error {
		s.LastUID = 42
		s.UIDValidity = 7
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	snap := fs.Snapshot()
	if snap.LastUID != 42 || snap.UIDValidity != 7 {
		t.Fatalf("Snapshot() = %+v, want LastUID=42 UIDValidity=7", snap)
	}

	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("state.json is empty after Update")
	}
}

func TestUpdate_ErrorFromFnWritesNothing(t *testing.T) {
	dir := t.TempDir()
	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Establish a known-good baseline on disk first.
	if err := fs.Update(func(s *State) error {
		s.LastUID = 1
		return nil
	}); err != nil {
		t.Fatalf("baseline Update: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}

	sentinel := errors.New("boom")
	err = fs.Update(func(s *State) error {
		s.LastUID = 999 // must not survive
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update error = %v, want sentinel", err)
	}

	if got := fs.Snapshot().LastUID; got != 1 {
		t.Fatalf("Snapshot().LastUID = %d after failed Update, want unchanged 1", got)
	}
	after, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("read after failed Update: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("state.json changed despite fn returning an error:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestUpdate_ConcurrentCallsDoNotLoseWrites(t *testing.T) {
	dir := t.TempDir()
	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fs.Update(func(s *State) error {
				s.NextPututuCounter()
				return nil
			}); err != nil {
				t.Errorf("Update: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := fs.Snapshot().PututuCounter; got != n {
		t.Fatalf("PututuCounter = %d after %d concurrent increments, want %d (lost update)", got, n, n)
	}
}

func TestSnapshot_IsIndependentOfSubsequentUpdates(t *testing.T) {
	dir := t.TempDir()
	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fs.Update(func(s *State) error {
		s.Acks.Record("o-1", "sent", time.Unix(1000, 0))
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	snap := fs.Snapshot()

	if err := fs.Update(func(s *State) error {
		s.Acks.Record("o-2", "sent", time.Unix(2000, 0))
		return nil
	}); err != nil {
		t.Fatalf("second Update: %v", err)
	}

	if len(snap.Acks.Entries) != 1 {
		t.Fatalf("earlier Snapshot() mutated by a later Update: got %d entries, want 1", len(snap.Acks.Entries))
	}
}
