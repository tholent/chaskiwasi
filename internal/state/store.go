package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/tholent/chaskiwasi/internal/atomicfile"
)

// fileName is state.json's fixed name within the data directory (§3). There
// is exactly one, because there is exactly one device per container (§2).
const fileName = "state.json"

// FileStore is the on-disk Store: /data/state.json, written through
// internal/atomicfile so every persisted version is complete or absent, never
// torn (§3, test V-12).
type FileStore struct {
	path string

	// writeMu serializes the whole read-modify-write cycle of Update: load the
	// published state, copy it, run fn, persist, publish the result. This is
	// distinct from atomicfile's own internal mutex, which only serializes the
	// disk write step across every atomicfile caller in the process; writeMu is
	// what stops two concurrent Update calls on this store from racing each
	// other and losing one's changes (the classic lost-update problem a real
	// database's transactions would have prevented — see A.9).
	writeMu sync.Mutex

	// current is the last successfully persisted state, published only after
	// Update's write is durable. Once published, a *State is never mutated in
	// place — Update always works on a fresh copy — so concurrent readers via
	// Snapshot need no lock of their own.
	current atomic.Pointer[State]
}

// Open loads dir/state.json, or initialises a fresh, empty State if the file
// does not exist yet: a missing file is a normal cold start (a brand-new
// device or a fresh backup restore), not an error. A file that exists but
// fails to parse is a different situation entirely — silently resetting to
// empty would quietly roll the pututu counter back to zero and drop the ack
// ring, both of which are supposed to be durable — so that case fails loudly
// instead.
func Open(dir string) (*FileStore, error) {
	path := filepath.Join(dir, fileName)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var st State
		if uerr := json.Unmarshal(data, &st); uerr != nil {
			return nil, fmt.Errorf("state: %s exists but will not parse, refusing to start: %w", path, uerr)
		}
		fs := &FileStore{path: path}
		fs.current.Store(&st)
		return fs, nil

	case os.IsNotExist(err):
		fs := &FileStore{path: path}
		fs.current.Store(&State{})
		return fs, nil

	default:
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	}
}

// Snapshot returns a copy of the current state (§ interface doc in state.go).
// Because current is only ever replaced, never mutated in place, dereferencing
// the published pointer is race-free without holding writeMu.
func (s *FileStore) Snapshot() State {
	return *s.current.Load()
}

// Update applies fn to a private copy of the current state under writeMu,
// persists the result atomically, and only then publishes it for Snapshot and
// future Updates to see. If fn returns an error, the copy is discarded and
// nothing is written or published — the state on disk and in memory is
// exactly as if Update had never been called.
func (s *FileStore) Update(fn func(*State) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	working := s.current.Load().clone()
	if err := fn(&working); err != nil {
		return err
	}

	data, err := json.Marshal(working)
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	if err := atomicfile.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("state: write %s: %w", s.path, err)
	}

	s.current.Store(&working)
	return nil
}

// clone deep-copies the slice fields so a working copy can be freely mutated
// by an Update callback without any chance of that mutation leaking into the
// currently published state before the write succeeds.
func (s *State) clone() State {
	out := *s
	out.Acks.Entries = append([]AckEntry(nil), s.Acks.Entries...)
	out.PendingNotices = append([]PendingNotice(nil), s.PendingNotices...)
	return out
}
