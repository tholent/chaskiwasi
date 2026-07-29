package guardians

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/tholent/chaskiwasi/internal/atomicfile"
)

// fileHeader is prepended to every guardians.toml write, for the same reason
// ayllu.toml carries one: the file is machine-rewritten, so it says so in its
// own words rather than eating a hand-edit silently.
const fileHeader = `# guardians.toml — guardian accounts for the web UI.
#
# Written by ` + "`wasi useradd`" + ` and by the password-change form. Comments and
# formatting are NOT preserved across a write. Passwords are argon2id hashes;
# there is no way to recover one, only to set a new one.
#
# session_epoch is incremented on every password change and invalidates every
# session cookie previously issued for that account. Lowering it by hand would
# hand old cookies back their access.

`

// fileFormat is the on-disk shape of guardians.toml.
type fileFormat struct {
	Guardians []Guardian `toml:"guardians"`
}

// FileStore is the file-backed Store over /data/guardians.toml (§9.2).
type FileStore struct {
	path string

	// mu serializes writes and guards byName. Reads take it too: Verify and
	// SetPassword can run concurrently from independent request goroutines,
	// and an epoch bump must not be observed half-applied.
	mu     sync.Mutex
	byName map[string]Guardian
	// loadedAt/loadedSize fingerprint the file as last read, so an edit made
	// by another process is picked up. `wasi useradd` is a separate process
	// writing this same file while the server runs (§9.2, §14): without this
	// the new account exists on disk and does not exist to the server until a
	// restart, and the CLI's own "sign in now" message is a lie. Worse, the
	// next password change would persist the server's stale in-memory table
	// and delete the account outright.
	loadedAt   time.Time
	loadedSize int64
}

var _ Store = (*FileStore)(nil)

// Open loads dir/guardians.toml, creating an empty, valid table if none
// exists. An empty table is a legitimate state: it is what a fresh deployment
// has before the operator runs `wasi useradd`.
func Open(dir string) (*FileStore, error) {
	s := &FileStore{
		path:   filepath.Join(dir, "guardians.toml"),
		byName: make(map[string]Guardian),
	}

	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("guardians: reading %s: %w", s.path, err)
	}

	if err := s.parse(data); err != nil {
		return nil, err
	}
	s.stampLocked()
	return s, nil
}

// parse replaces the in-memory table from raw TOML. Caller holds mu (or owns
// the store outright, as Open does).
func (s *FileStore) parse(data []byte) error {
	var ff fileFormat
	if err := toml.Unmarshal(data, &ff); err != nil {
		return fmt.Errorf("guardians: parsing %s: %w", s.path, err)
	}
	byName := make(map[string]Guardian, len(ff.Guardians))
	for _, g := range ff.Guardians {
		name, err := normalizeName(g.Name)
		if err != nil {
			return fmt.Errorf("guardians: %s: %w", s.path, err)
		}
		if g.SessionEpoch < 1 {
			// A hand-edited or truncated row must not be readable as epoch 0,
			// which no issued cookie carries but which would compare equal to
			// another mangled row.
			return fmt.Errorf("guardians: %s: %q has session_epoch %d, want >= 1",
				s.path, name, g.SessionEpoch)
		}
		g.Name = name
		byName[name] = g
	}
	s.byName = byName
	return nil
}

// refreshLocked re-reads guardians.toml if its mtime or size has changed since
// the last read. Caller holds mu.
//
// A stale read here is not a cache miss, it is an authentication decision made
// against a table that no longer exists — an account the operator just created
// is rejected, and an account whose password was just reset still accepts the
// old one. Both fail in the direction that matters, so this runs before every
// read and before every write. The file holds two or three rows; the stat call
// costs more than the parse.
func (s *FileStore) refreshLocked() {
	info, err := os.Stat(s.path)
	if err != nil {
		return // missing or unreadable: keep what we have
	}
	if info.ModTime().Equal(s.loadedAt) && info.Size() == s.loadedSize {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	if err := s.parse(data); err != nil {
		// A half-written or hand-mangled file must not empty the table and
		// lock every guardian out; keep the last good one.
		return
	}
	s.loadedAt, s.loadedSize = info.ModTime(), info.Size()
}

// stampLocked records the fingerprint of the file as it now stands on disk.
func (s *FileStore) stampLocked() {
	if info, err := os.Stat(s.path); err == nil {
		s.loadedAt, s.loadedSize = info.ModTime(), info.Size()
	}
}

// Path reports where this store persists, so `wasi useradd` can tell the
// operator which file it just wrote.
func (s *FileStore) Path() string { return s.path }

// List implements Store.
func (s *FileStore) List() []Guardian {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()

	out := make([]Guardian, 0, len(s.byName))
	for _, g := range s.byName {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get implements Store.
func (s *FileStore) Get(name string) (Guardian, bool) {
	norm, err := normalizeName(name)
	if err != nil {
		return Guardian{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	g, ok := s.byName[norm]
	return g, ok
}

// Add implements Store.
func (s *FileStore) Add(name, password string) (Guardian, error) {
	norm, err := normalizeName(name)
	if err != nil {
		return Guardian{}, err
	}
	if err := checkPassword(password); err != nil {
		return Guardian{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Guardian{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()

	if _, exists := s.byName[norm]; exists {
		return Guardian{}, ErrExists
	}
	now := time.Now().UTC()
	g := Guardian{
		Name:              norm,
		PasswordHash:      hash,
		SessionEpoch:      1,
		CreatedAt:         now,
		PasswordChangedAt: now,
	}
	if err := s.commitLocked(g); err != nil {
		return Guardian{}, err
	}
	return g, nil
}

// SetPassword implements Store: new hash, epoch incremented (§9.2, V-19).
func (s *FileStore) SetPassword(name, password string) (Guardian, error) {
	norm, err := normalizeName(name)
	if err != nil {
		return Guardian{}, ErrNoSuchGuardian
	}
	if err := checkPassword(password); err != nil {
		return Guardian{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Guardian{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()

	g, ok := s.byName[norm]
	if !ok {
		return Guardian{}, ErrNoSuchGuardian
	}
	g.PasswordHash = hash
	// The increment and the new hash land in the same atomic write. Splitting
	// them would leave a window in which the password had changed and old
	// cookies still worked, which is the exact failure §9.2 names.
	g.SessionEpoch++
	g.PasswordChangedAt = time.Now().UTC()

	if err := s.commitLocked(g); err != nil {
		return Guardian{}, err
	}
	return g, nil
}

// Verify implements Store. An unknown name is verified against the decoy hash
// so it costs the same work as a wrong password (see hash.go).
func (s *FileStore) Verify(name, password string) (Guardian, error) {
	norm, nameErr := normalizeName(name)

	s.mu.Lock()
	s.refreshLocked()
	g, ok := s.byName[norm]
	s.mu.Unlock()

	if nameErr != nil || !ok {
		verifyPassword(decoyHash(), password)
		return Guardian{}, ErrBadCredentials
	}
	if !verifyPassword(g.PasswordHash, password) {
		return Guardian{}, ErrBadCredentials
	}
	return g, nil
}

// commitLocked writes g into the table and persists, rolling back on I/O
// failure so the in-memory view never runs ahead of the file. Caller holds mu.
func (s *FileStore) commitLocked(g Guardian) error {
	prev, had := s.byName[g.Name]
	s.byName[g.Name] = g

	if err := s.persistLocked(); err != nil {
		if had {
			s.byName[g.Name] = prev
		} else {
			delete(s.byName, g.Name)
		}
		return fmt.Errorf("guardians: persisting %s: %w", s.path, err)
	}
	return nil
}

func (s *FileStore) persistLocked() error {
	names := make([]string, 0, len(s.byName))
	for n := range s.byName {
		names = append(names, n)
	}
	sort.Strings(names)

	ff := fileFormat{Guardians: make([]Guardian, 0, len(names))}
	for _, n := range names {
		ff.Guardians = append(ff.Guardians, s.byName[n])
	}

	body, err := toml.Marshal(ff)
	if err != nil {
		return err
	}
	// 0o600: this file holds password hashes and nothing else needs to read it.
	if err := atomicfile.WriteFile(s.path, append([]byte(fileHeader), body...), 0o600); err != nil {
		return err
	}
	s.stampLocked()
	return nil
}

// normalizeName lower-cases and trims a guardian name and rejects anything
// outside [a-z0-9._-]. The restriction is not decoration: the name is stamped
// into the session cookie payload and into the ayllu change log's actor field,
// so keeping it to a boring character set means neither has to worry about
// delimiters, encoding, or case-folding surprises.
func normalizeName(name string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(name))
	if norm == "" || len(norm) > 32 {
		return "", ErrInvalidName
	}
	for _, r := range norm {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return "", ErrInvalidName
		}
	}
	return norm, nil
}

// checkPassword enforces the one rule (see MinPasswordLen). Length is counted
// in bytes deliberately: the question here is how much material the KDF gets,
// not how many characters a human perceives.
func checkPassword(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("%w: need at least %d characters", ErrWeakPassword, MinPasswordLen)
	}
	return nil
}
