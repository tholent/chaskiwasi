package ayllu

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
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
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// SystemAddress is the sentinel "address" that resolves the messages Wasi
// itself APPENDs to INBOX as notice letters (§7.4). It is never a real
// mailbox and never crosses the wire — protocol.SysContactID (the contact id)
// does that job. It is exported so the notice package (a later wave) can use
// the identical value as the From header on generated notice messages: the
// two must agree, or filing would mis-file Wasi's own notices as strangers.
const SystemAddress = "system@wasi.internal"

// SystemName is the display name behind protocol.SysContactID: the sender a
// child sees on every announcement about their contact list, and — since
// notice letters graduate (§7.4) — the sender on those records for as long as
// the archive lasts.
//
// "Wasi" and not "Home". This device exists for a young person who moves
// often, and "home" is a loaded word for a reader who may not have a settled
// one, or who has more than one, or who is being asked to call somewhere home
// that they don't. A letter arriving "from Home" quietly asserts something
// about the reader's life; "from Wasi" asserts nothing.
//
// It is also legal here in a way the internal vocabulary is not: Wasi is one
// of the three public names of design-spec §0, chosen to survive cold reading
// (WAH-see), not one of the §0.1 identifiers — pututu, ayllu, kipu — that must
// never reach a user-facing string (test V-14).
const SystemName = "Wasi"

// fileHeader is prepended to every ayllu.toml write. Comments and formatting
// are not preserved across a save (§3), so the file says so in its own words
// rather than surprising a guardian mid-edit.
const fileHeader = `# ayllu.toml — the contact list. Hand-editable while Wasi is stopped.
#
# This file is rewritten atomically by the guardian UI. Comments and
# formatting are NOT preserved across a save: anything you add here by hand
# survives only until the next write. Edit safely only while Wasi is stopped;
# it re-reads this file on the next start.

`

// fileFormat is the on-disk shape of ayllu.toml.
type fileFormat struct {
	Version  int       `toml:"version"`
	Contacts []Contact `toml:"contacts"`
}

// FileStore is the file-backed Store over /data/ayllu.toml and
// /data/ayllu-log.jsonl (§7, §3).
//
// c_sys is deliberately not a row in the map or the file: it is a reserved,
// synthetic identity (§7.4) that ByID, Resolve, and ResolveActive answer for
// directly, so "cannot be written to" is true by construction rather than by
// convention — there is no code path that stores a mutable row under that id.
// It is absent from List (the guardian UI manages real contacts only) but
// present in DeviceView, so the device can name the sender of a notice letter
// — see DeviceView for why it ships as a tombstone.
type FileStore struct {
	tomlPath    string
	logPath     string
	maxContacts int

	// mu serializes every Mutate call and guards the fields below. It also
	// gives ayllu-log.jsonl appends a total order relative to ayllu.toml
	// writes, which is what makes "one line per event" true under concurrent
	// guardian requests.
	mu       sync.Mutex
	version  int
	contacts map[string]Contact // keyed by Contact.ID; tombstones included
}

var _ Store = (*FileStore)(nil)

// Open loads dir/ayllu.toml, creating an empty, valid table if none exists
// yet, and ensures dir/ayllu-log.jsonl is present for append. maxContacts is
// ayllu.max_contacts from wasi.toml (§13) and includes tombstones (A.3).
func Open(dir string, maxContacts int) (*FileStore, error) {
	if maxContacts <= 0 {
		return nil, fmt.Errorf("ayllu: max_contacts must be positive, got %d", maxContacts)
	}

	s := &FileStore{
		tomlPath:    filepath.Join(dir, "ayllu.toml"),
		logPath:     filepath.Join(dir, "ayllu-log.jsonl"),
		maxContacts: maxContacts,
		contacts:    make(map[string]Contact),
	}

	data, err := os.ReadFile(s.tomlPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("ayllu: initializing %s: %w", s.tomlPath, err)
		}
	case err != nil:
		return nil, fmt.Errorf("ayllu: reading %s: %w", s.tomlPath, err)
	default:
		var ff fileFormat
		if err := toml.Unmarshal(data, &ff); err != nil {
			return nil, fmt.Errorf("ayllu: parsing %s: %w", s.tomlPath, err)
		}
		s.version = ff.Version
		for _, c := range ff.Contacts {
			if c.ID == protocol.SysContactID {
				// c_sys cannot be written to (§7.4); a hand-edited row
				// claiming that id is dropped rather than trusted.
				continue
			}
			s.contacts[c.ID] = c
		}
	}

	f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ayllu: opening %s: %w", s.logPath, err)
	}
	f.Close()

	return s, nil
}

// List implements Store.
func (s *FileStore) List() (int, []Contact) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := s.sortedIDsLocked()
	out := make([]Contact, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.contacts[id])
	}
	return s.version, out
}

// Resolve implements Store: full table, tombstones included (§7.2).
func (s *FileStore) Resolve(addr string) (Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(addr, false)
}

// ResolveActive implements Store: active rows only (§7.2).
func (s *FileStore) ResolveActive(addr string) (Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(addr, true)
}

func (s *FileStore) resolveLocked(addr string, activeOnly bool) (Contact, bool) {
	norm := normalizeAddress(addr)
	if norm == normalizeAddress(SystemAddress) {
		// c_sys always resolves, on both paths (§7.4): filing must leave
		// Wasi's own notice APPENDs in INBOX, and derivation must render them.
		return s.sysContact(), true
	}
	for _, id := range s.sortedIDsLocked() { // deterministic tie-break
		c := s.contacts[id]
		if activeOnly && !c.Active {
			continue
		}
		if normalizeAddress(c.Address) == norm {
			return c, true
		}
		// Past addresses resolve at read time only. Filing and sending see the
		// current address alone, so a readdress renders history without
		// reopening the channel (see Contact.PastAddresses).
		if activeOnly {
			continue
		}
		for _, past := range c.PastAddresses {
			if normalizeAddress(past) == norm {
				return c, true
			}
		}
	}
	return Contact{}, false
}

// ByID implements Store, against the full table.
func (s *FileStore) ByID(id string) (Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == protocol.SysContactID {
		return s.sysContact(), true
	}
	c, ok := s.contacts[id]
	return c, ok
}

func (s *FileStore) sysContact() Contact {
	return Contact{
		ID:      protocol.SysContactID,
		Name:    SystemName,
		Address: SystemAddress,
		Active:  true,
	}
}

// DeviceView returns the device's Ayllu block (§4.3) if requestVersion
// differs from the store's current version, or nil if the device is already
// current — mirroring the "present only on version change" wire rule.
// Tombstones are included so the device can show a name on an old letter
// while hiding the person from the compose picker (§7.2); addresses are never
// included (I-2). A test asserts no contact address appears in the marshalled
// result.
func (s *FileStore) DeviceView(requestVersion int) *protocol.Ayllu {
	s.mu.Lock()
	defer s.mu.Unlock()

	if requestVersion == s.version {
		return nil
	}

	ids := s.sortedIDsLocked()
	contacts := make([]protocol.AylluContact, 0, len(ids)+1)

	// c_sys ships to the device so a notice letter has a sender the device can
	// name. It is sent with Active false, which is not a fiction: the device's
	// rule for an inactive contact is exactly the rule c_sys needs — render the
	// name on letters they sent, hide them from the compose picker — and §7.4
	// says c_sys cannot be written to. Reusing the tombstone semantics gets
	// both halves right with no new wire field and no firmware special case.
	contacts = append(contacts, protocol.AylluContact{
		ID:     protocol.SysContactID,
		Name:   SystemName,
		Active: false,
	})

	for _, id := range ids {
		c := s.contacts[id]
		contacts = append(contacts, protocol.AylluContact{
			ID:       c.ID,
			Name:     c.Name,
			Active:   c.Active,
			Pinned:   c.Pinned,
			Order:    c.Order,
			Portrait: c.Portrait,
		})
	}
	return &protocol.Ayllu{Version: s.version, Contacts: contacts}
}

// Mutate implements Store (§7.6 crash ordering: write ayllu.toml atomically,
// then append the change-log line, then return the Event).
//
// Cosmetic changes (nickname/pinned/order/portrait — the youth's overlay,
// §3) still bump Version, because the device needs the new overlay pushed on
// the next sync; they deliberately skip the change-log append, because
// ayllu-log.jsonl is the guardian-facing accountability trail for who is on
// the list and what address they have (I-4), and a nickname the *child*
// chose is neither. Skipping the log line is therefore not an oversight: it
// is what keeps the log meaningful instead of full of youth-preference noise.
func (s *FileStore) Mutate(actor string, m Mutation) (Event, error) {
	if m.ContactID == protocol.SysContactID {
		return Event{}, ErrSystemContact
	}
	if (m.Action == ActionAdd || m.Action == ActionReaddress) &&
		m.Address != "" && normalizeAddress(m.Address) == normalizeAddress(SystemAddress) {
		// Blocks a contact from being pointed at the system sentinel address:
		// that would let a normal row shadow c_sys in Resolve and misattribute
		// Wasi's own notices (§7.4's redirection-attack concern, applied to
		// the one address Resolve treats as infallible).
		return Event{}, ErrSystemContact
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		contact    Contact
		oldAddress string
		err        error
	)
	switch m.Action {
	case ActionAdd:
		contact, err = s.applyAddLocked(m)
	case ActionDeactivate:
		contact, err = s.applyDeactivateLocked(m)
	case ActionReactivate:
		contact, err = s.applyReactivateLocked(m)
	case ActionReaddress:
		contact, oldAddress, err = s.applyReaddressLocked(m)
	case ActionCosmetic:
		contact, err = s.applyCosmeticLocked(m)
	default:
		err = fmt.Errorf("ayllu: unknown action %q", m.Action)
	}
	if err != nil {
		return Event{}, err
	}

	if err := s.commitLocked(contact); err != nil {
		return Event{}, fmt.Errorf("ayllu: persisting after %s: %w", m.Action, err)
	}

	event := Event{
		At:        time.Now().UTC(),
		Actor:     actor,
		Action:    m.Action,
		ContactID: contact.ID,
		Name:      contact.Name,
		Version:   s.version,
	}
	if m.Action == ActionReaddress {
		event.OldAddress = oldAddress
		event.NewAddress = contact.Address
	} else if m.Action == ActionAdd {
		event.NewAddress = contact.Address
	}

	if m.Action != ActionCosmetic {
		// ayllu.toml is already durable at this point (commitLocked
		// succeeded); a failure here means "notice arrives a little late,"
		// never "change happened silently" (§7.6) — the change itself stands.
		if err := s.appendLogLocked(event); err != nil {
			return Event{}, fmt.Errorf("ayllu: appending change log: %w", err)
		}
	}

	return event, nil
}

// commitLocked writes contact into the in-memory table, bumps the version,
// and persists — rolling both back if the write fails, so an I/O error never
// leaves the in-memory view ahead of what's on disk.
func (s *FileStore) commitLocked(contact Contact) error {
	prevContact, hadContact := s.contacts[contact.ID]
	prevVersion := s.version

	s.contacts[contact.ID] = contact
	s.version++

	if err := s.persistLocked(); err != nil {
		if hadContact {
			s.contacts[contact.ID] = prevContact
		} else {
			delete(s.contacts, contact.ID)
		}
		s.version = prevVersion
		return err
	}
	return nil
}

func (s *FileStore) applyAddLocked(m Mutation) (Contact, error) {
	addr := strings.TrimSpace(m.Address)
	if addr == "" {
		return Contact{}, fmt.Errorf("ayllu: add requires an address")
	}
	norm := normalizeAddress(addr)

	// Re-adding an address reuses the original contact id, matched against
	// the full table (tombstones included), so a person who leaves and
	// returns stays one person in the archive (§7.2). Matching against active
	// rows too makes a duplicate add idempotent instead of drifting into two
	// rows resolving the same address.
	// A match against a *past* address counts too: someone re-added at an
	// address they used years ago is the same person in the archive, and
	// splitting them into a second row would strand half their history.
	for _, id := range s.sortedIDsLocked() {
		c := s.contacts[id]
		current := normalizeAddress(c.Address) == norm
		if !current && !c.hasPastAddress(norm) {
			continue
		}
		if !current {
			c.PastAddresses = retainPast(c.PastAddresses, c.Address, addr)
			c.Address = addr
		}
		c.Active = true
		c.Name = m.Name
		c.Pinned = m.Pinned
		c.Order = m.Order
		c.Portrait = m.Portrait
		return c, nil
	}

	// max_contacts includes tombstones (A.3); this is a genuinely new row.
	if len(s.contacts) >= s.maxContacts {
		return Contact{}, ErrMaxContacts
	}
	id, err := s.nextIDLocked()
	if err != nil {
		return Contact{}, err
	}
	return Contact{
		ID:       id,
		Name:     m.Name,
		Address:  addr,
		Active:   true,
		Pinned:   m.Pinned,
		Order:    m.Order,
		Portrait: m.Portrait,
	}, nil
}

func (s *FileStore) applyDeactivateLocked(m Mutation) (Contact, error) {
	c, ok := s.contacts[m.ContactID]
	if !ok {
		return Contact{}, ErrUnknownContact
	}
	// I-5: the address is retained. Only the flag changes.
	c.Active = false
	return c, nil
}

func (s *FileStore) applyReactivateLocked(m Mutation) (Contact, error) {
	c, ok := s.contacts[m.ContactID]
	if !ok {
		return Contact{}, ErrUnknownContact
	}
	c.Active = true
	return c, nil
}

func (s *FileStore) applyReaddressLocked(m Mutation) (Contact, string, error) {
	c, ok := s.contacts[m.ContactID]
	if !ok {
		return Contact{}, "", ErrUnknownContact
	}
	newAddr := strings.TrimSpace(m.Address)
	if newAddr == "" {
		return Contact{}, "", fmt.Errorf("ayllu: readdress requires a new address")
	}
	old := c.Address
	if normalizeAddress(old) == normalizeAddress(newAddr) {
		return Contact{}, "", fmt.Errorf("ayllu: readdress to the same address")
	}
	c.Address = newAddr
	c.PastAddresses = retainPast(c.PastAddresses, old, newAddr)
	return c, old, nil
}

// retainPast records old in the history list and drops newAddr from it, so a
// contact moved back to an address they used before does not carry it in both
// places. Order is preserved: this is a record of what was, and it only grows.
func retainPast(past []string, old, newAddr string) []string {
	kept := make([]string, 0, len(past)+1)
	seen := map[string]bool{normalizeAddress(newAddr): true}
	for _, a := range append(past, old) {
		norm := normalizeAddress(a)
		if a == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		kept = append(kept, a)
	}
	return kept
}

func (s *FileStore) applyCosmeticLocked(m Mutation) (Contact, error) {
	c, ok := s.contacts[m.ContactID]
	if !ok {
		return Contact{}, ErrUnknownContact
	}
	// Address and Active are guardian-owned and untouched here; only the
	// youth's overlay changes (§3, design-spec §3.2).
	c.Name = m.Name
	c.Pinned = m.Pinned
	c.Order = m.Order
	c.Portrait = m.Portrait
	return c, nil
}

// nextIDLocked finds the lowest unused "c_NN" id for a genuinely new
// contact. Ids are never reused across a different address: tombstones keep
// their slot forever, which is what makes the maxContacts cap meaningful.
// idAlphabet is Crockford base32: the digits plus the lowercase letters, less
// i, l, o and u — the four that misread as 1/0 or turn into words. Contact ids
// are read out of log lines and typed into a `wasi contacts` invocation, so
// unambiguous beats dense. Exactly 32 symbols, which is what keeps the
// uniform-selection argument in randomContactID true.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// idBodyLen is the number of random symbols after the "c_" prefix. Ten symbols
// is 50 bits: with the 24-contact cap (A.3) the chance of ever drawing a
// collision is vanishing, and nextIDLocked re-draws anyway.
const idBodyLen = 10

// nextIDLocked mints an opaque contact id.
//
// Ids used to be assigned sequentially (c_01, c_02, …), which made every id a
// disclosure: an id seen in isolation revealed both when that person was added
// relative to everyone else, and a lower bound on how many contacts exist —
// c_07 says "at least seven". Contact ids travel further than the addresses
// they stand in for. They are on the wire in every letter and every ayllu
// block, they sit in flash on a device the design spec expects to be lost or
// stolen (design-spec §5.6), and they appear in log lines. The ordering is
// also the more sensitive half: "who was added most recently" and "who was
// contact number one" are facts about a family, and the point of I-2 is that
// the device carries names and nothing else about the shape of the list.
//
// Random ids still sort lexicographically — a total order over a set of
// strings always exists — so the deterministic tie-break resolution and
// DeviceView rely on is unchanged. The order simply no longer means anything,
// which is the point.
//
// Existing sequential ids are never rewritten. A contact id is the permanent
// rendering key for every letter that person ever sent (§7.1); reissuing one
// would unresolve their history exactly the way F-1 describes. Old ids keep
// working, and only new contacts get the new form.
func (s *FileStore) nextIDLocked() (string, error) {
	if len(s.contacts) >= s.maxContacts {
		return "", ErrMaxContacts
	}
	// Redraw on collision rather than probing adjacent values: a probe would
	// reintroduce a relationship between one id and another.
	for attempt := 0; attempt < 32; attempt++ {
		id, err := randomContactID()
		if err != nil {
			return "", err
		}
		if id == protocol.SysContactID {
			continue
		}
		if _, used := s.contacts[id]; used {
			continue
		}
		return id, nil
	}
	return "", errors.New("ayllu: could not mint a unique contact id")
}

// randomContactID draws idBodyLen symbols uniformly from idAlphabet.
//
// The modulo is unbiased because 256 is an exact multiple of 32: every byte
// value maps to exactly eight symbols. If the alphabet ever stops being a
// power of two this needs rejection sampling, or the low symbols become
// slightly likelier — a small bias, but the whole purpose of these ids is that
// they carry no structure.
func randomContactID() (string, error) {
	b := make([]byte, idBodyLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ayllu: generating a contact id: %w", err)
	}
	out := make([]byte, 0, 2+idBodyLen)
	out = append(out, 'c', '_')
	for _, v := range b {
		out = append(out, idAlphabet[int(v)%len(idAlphabet)])
	}
	return string(out), nil
}

func (s *FileStore) sortedIDsLocked() []string {
	ids := make([]string, 0, len(s.contacts))
	for id := range s.contacts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// persistLocked rewrites ayllu.toml atomically from the in-memory table.
// Caller must hold mu.
func (s *FileStore) persistLocked() error {
	ids := s.sortedIDsLocked()
	ff := fileFormat{Version: s.version, Contacts: make([]Contact, 0, len(ids))}
	for _, id := range ids {
		ff.Contacts = append(ff.Contacts, s.contacts[id])
	}

	body, err := toml.Marshal(ff)
	if err != nil {
		return err
	}
	data := append([]byte(fileHeader), body...)
	return atomicfile.WriteFile(s.tomlPath, data, 0o600)
}

// appendLogLocked appends one Event to ayllu-log.jsonl. Caller must hold mu,
// which is what gives the log a total order matching the order mutations were
// applied — the log is meant to be read top-to-bottom as history (§3).
func (s *FileStore) appendLogLocked(e Event) error {
	f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return err
	}
	return f.Sync()
}

// hasPastAddress reports whether norm (already normalised) is one of this
// contact's retained former addresses.
func (c Contact) hasPastAddress(norm string) bool {
	for _, past := range c.PastAddresses {
		if normalizeAddress(past) == norm {
			return true
		}
	}
	return false
}

// normalizeAddress prepares an address for comparison: it tolerates a
// "Name <addr>" wrapper (resolution's real job is matching a bare address;
// full header parsing belongs to whichever package hands addresses to this
// one) and lower-cases only the domain, per §7.2's "case-insensitive on the
// domain."
func normalizeAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndexByte(addr, '<'); i >= 0 {
		if j := strings.IndexByte(addr[i:], '>'); j >= 0 {
			addr = addr[i+1 : i+j]
		}
	}
	addr = strings.TrimSpace(addr)

	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return addr
	}
	local, domain := addr[:at], addr[at+1:]
	return local + "@" + strings.ToLower(domain)
}

// ReadLog returns the change-log events at or after since, oldest first.
//
// ayllu-log.jsonl is append-only and is written *before* the notice path runs,
// which makes it the durable record of what changed — `pending_notices` in
// state.json is only a fast path. Startup reconciliation of announcements
// against this log is what closes the crash window in which a change lands but
// its notice never does, and I-4 ("nothing about the list changes silently")
// is only literally true once that window is closed.
//
// Malformed lines are skipped rather than fatal: a torn final line from a
// crash mid-append must not make the whole log unreadable, and losing the
// ability to reconcile would be a strictly worse outcome than skipping the one
// event that was already only half-written.
func ReadLog(dir string, since time.Time) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, "ayllu-log.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ayllu: opening change log: %w", err)
	}
	defer f.Close()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.At.Before(since) {
			continue
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ayllu: reading change log: %w", err)
	}
	return events, nil
}
