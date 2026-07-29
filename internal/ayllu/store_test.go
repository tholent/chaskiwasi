package ayllu

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

func openTestStore(t *testing.T, maxContacts int) *FileStore {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, maxContacts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustAdd(t *testing.T, s *FileStore, actor, name, addr string) Event {
	t.Helper()
	e, err := s.Mutate(actor, Mutation{Action: ActionAdd, Name: name, Address: addr})
	if err != nil {
		t.Fatalf("add %s <%s>: %v", name, addr, err)
	}
	return e
}

func TestOpen_CreatesEmptyValidFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	version, contacts := s.List()
	if version != 0 || len(contacts) != 0 {
		t.Fatalf("fresh store = (version %d, %d contacts), want (0, 0)", version, len(contacts))
	}

	data, err := os.ReadFile(filepath.Join(dir, "ayllu.toml"))
	if err != nil {
		t.Fatalf("reading ayllu.toml: %v", err)
	}
	if !strings.Contains(string(data), "NOT preserved") {
		t.Errorf("ayllu.toml header does not warn that formatting is not preserved:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "ayllu-log.jsonl")); err != nil {
		t.Errorf("ayllu-log.jsonl not created: %v", err)
	}
}

func TestOpen_ReloadsExistingFile(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAdd(t, s1, "dad", "Rosa", "rosa@example.com")

	s2, err := Open(dir, 24)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	version, contacts := s2.List()
	if version != 1 || len(contacts) != 1 || contacts[0].Name != "Rosa" {
		t.Fatalf("reloaded store = (version %d, %+v), want version 1 with Rosa", version, contacts)
	}
}

// V-6's shape: two letters from an active contact, deactivate them, a third
// letter arrives, then a full resync: the first two must still resolve (by
// name) via Resolve, the third must not resolve as active, outbound must be
// rejected, and the tombstone must report active:false. Re-adding the same
// address must reuse the same contact id.
func TestV6_DeactivationAndReadd(t *testing.T) {
	s := openTestStore(t, 24)
	addEvent := mustAdd(t, s, "dad", "Rosa", "rosa@example.com")
	id := addEvent.ContactID

	// Two letters arrive while active: both resolve for filing and derivation.
	if c, ok := s.ResolveActive("rosa@example.com"); !ok || c.ID != id {
		t.Fatalf("ResolveActive while active = (%+v, %v), want (%s, true)", c, ok, id)
	}
	if c, ok := s.Resolve("rosa@example.com"); !ok || c.ID != id {
		t.Fatalf("Resolve while active = (%+v, %v), want (%s, true)", c, ok, id)
	}

	if _, err := s.Mutate("dad", Mutation{Action: ActionDeactivate, ContactID: id}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Old letters (full-table resolution) still render with the correct name.
	c, ok := s.Resolve("rosa@example.com")
	if !ok || c.ID != id || c.Name != "Rosa" {
		t.Fatalf("Resolve after deactivation = (%+v, %v), want id %s, name Rosa, ok true", c, ok, id)
	}
	if c.Active {
		t.Fatalf("Resolve after deactivation reports Active=true, want false (tombstone)")
	}

	// New mail from her does not resolve as active: filing must Held it.
	if _, ok := s.ResolveActive("rosa@example.com"); ok {
		t.Fatalf("ResolveActive after deactivation unexpectedly resolved")
	}

	// Outbound to c_07-equivalent is rejected: ResolveActive is what syncsvc
	// uses to decide rejected_inactive, and it correctly fails above. ByID
	// still finds the tombstone (derivation needs it for contact_id lookups).
	byID, ok := s.ByID(id)
	if !ok || byID.Active {
		t.Fatalf("ByID after deactivation = (%+v, %v), want inactive contact, ok true", byID, ok)
	}

	// Tombstone appears in List/DeviceView with active:false.
	_, contacts := s.List()
	if len(contacts) != 1 || contacts[0].Active {
		t.Fatalf("List after deactivation = %+v, want one inactive contact", contacts)
	}

	// Re-add the same address: same id, still one person in the archive.
	readd := mustAdd(t, s, "dad", "Rosa", "rosa@example.com")
	if readd.ContactID != id {
		t.Fatalf("re-add contact id = %s, want reused id %s", readd.ContactID, id)
	}
	_, contacts = s.List()
	if len(contacts) != 1 {
		t.Fatalf("List after re-add = %d contacts, want 1 (same person, not a duplicate)", len(contacts))
	}
	if c, ok := s.ResolveActive("rosa@example.com"); !ok || c.ID != id {
		t.Fatalf("ResolveActive after re-add = (%+v, %v), want (%s, true)", c, ok, id)
	}
}

func TestMaxContacts_IncludesTombstones(t *testing.T) {
	s := openTestStore(t, 2)
	mustAdd(t, s, "dad", "A", "a@example.com")
	mustAdd(t, s, "dad", "B", "b@example.com")

	// Both slots used; a third distinct address must be rejected even though
	// the cap counts tombstones, not just active rows.
	if _, err := s.Mutate("dad", Mutation{Action: ActionAdd, Name: "C", Address: "c@example.com"}); err != ErrMaxContacts {
		t.Fatalf("add beyond cap: err = %v, want ErrMaxContacts", err)
	}

	_, contacts := s.List()
	var bID string
	for _, c := range contacts {
		if c.Address == "b@example.com" {
			bID = c.ID
		}
	}
	if _, err := s.Mutate("dad", Mutation{Action: ActionDeactivate, ContactID: bID}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// B is now a tombstone, not a free slot: still full.
	if _, err := s.Mutate("dad", Mutation{Action: ActionAdd, Name: "C", Address: "c@example.com"}); err != ErrMaxContacts {
		t.Fatalf("add after deactivate (tombstone still counts): err = %v, want ErrMaxContacts", err)
	}

	// Re-adding B's own address reuses her tombstone, not a new slot.
	readd, err := s.Mutate("dad", Mutation{Action: ActionAdd, Name: "B", Address: "b@example.com"})
	if err != nil {
		t.Fatalf("re-add B: %v", err)
	}
	if readd.ContactID != bID {
		t.Fatalf("re-add B got id %s, want reused %s", readd.ContactID, bID)
	}
}

func TestSysContact_AlwaysResolves(t *testing.T) {
	s := openTestStore(t, 24)

	c, ok := s.ByID(protocol.SysContactID)
	if !ok || c.ID != protocol.SysContactID || !c.Active {
		t.Fatalf("ByID(c_sys) = (%+v, %v), want an active contact", c, ok)
	}

	c, ok = s.Resolve(SystemAddress)
	if !ok || c.ID != protocol.SysContactID {
		t.Fatalf("Resolve(SystemAddress) = (%+v, %v), want c_sys", c, ok)
	}

	c, ok = s.ResolveActive(SystemAddress)
	if !ok || c.ID != protocol.SysContactID {
		t.Fatalf("ResolveActive(SystemAddress) = (%+v, %v), want c_sys", c, ok)
	}
}

func TestSysContact_Immutable(t *testing.T) {
	s := openTestStore(t, 24)

	cases := []Mutation{
		{Action: ActionDeactivate, ContactID: protocol.SysContactID},
		{Action: ActionReactivate, ContactID: protocol.SysContactID},
		{Action: ActionReaddress, ContactID: protocol.SysContactID, Address: "new@example.com"},
		{Action: ActionCosmetic, ContactID: protocol.SysContactID, Name: "Not Home"},
		{Action: ActionAdd, ContactID: protocol.SysContactID, Name: "x", Address: "x@example.com"},
		{Action: ActionAdd, Name: "Shadow", Address: SystemAddress},
		{Action: ActionReaddress, ContactID: "", Address: SystemAddress},
	}
	for _, m := range cases {
		if _, err := s.Mutate("dad", m); err != ErrSystemContact {
			t.Errorf("Mutate(%+v) = %v, want ErrSystemContact", m, err)
		}
	}

	// None of the attempts left a row behind.
	_, contacts := s.List()
	if len(contacts) != 0 {
		t.Fatalf("List after failed c_sys mutations = %+v, want empty", contacts)
	}
}

func TestReaddress_AnnouncesOldAndNewAddress(t *testing.T) {
	s := openTestStore(t, 24)
	add := mustAdd(t, s, "dad", "Rosa", "rosa-old@example.com")

	event, err := s.Mutate("dad", Mutation{
		Action:    ActionReaddress,
		ContactID: add.ContactID,
		Address:   "rosa-new@example.com",
	})
	if err != nil {
		t.Fatalf("readdress: %v", err)
	}
	if event.OldAddress != "rosa-old@example.com" || event.NewAddress != "rosa-new@example.com" {
		t.Fatalf("readdress event = %+v, want old/new addresses recorded", event)
	}

	if _, ok := s.ResolveActive("rosa-old@example.com"); ok {
		t.Fatalf("old address still resolves active after readdress")
	}
	if c, ok := s.ResolveActive("rosa-new@example.com"); !ok || c.ID != add.ContactID {
		t.Fatalf("new address does not resolve active after readdress: (%+v, %v)", c, ok)
	}
}

func TestUnknownContact(t *testing.T) {
	s := openTestStore(t, 24)
	for _, action := range []Action{ActionDeactivate, ActionReactivate, ActionCosmetic} {
		if _, err := s.Mutate("dad", Mutation{Action: action, ContactID: "c_99"}); err != ErrUnknownContact {
			t.Errorf("Mutate(%s, unknown id) = %v, want ErrUnknownContact", action, err)
		}
	}
	if _, _, err := (&FileStore{}).applyReaddressLocked(Mutation{ContactID: "c_99"}); err != ErrUnknownContact {
		t.Errorf("applyReaddressLocked(unknown) = %v, want ErrUnknownContact", err)
	}
}

// Cosmetic changes bump the version (the device needs the new overlay) but
// produce no change-log line and no address fields on the event.
func TestCosmetic_BumpsVersionButSkipsLog(t *testing.T) {
	s := openTestStore(t, 24)
	add := mustAdd(t, s, "dad", "Rosa", "rosa@example.com")

	logPath := s.logPath
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	beforeLines := countLines(before)

	versionBefore, _ := s.List()

	event, err := s.Mutate("kid", Mutation{
		Action:    ActionCosmetic,
		ContactID: add.ContactID,
		Name:      "Auntie R",
		Pinned:    true,
		Order:     1,
		Portrait:  "p09",
	})
	if err != nil {
		t.Fatalf("cosmetic mutate: %v", err)
	}

	versionAfter, contacts := s.List()
	if versionAfter != versionBefore+1 {
		t.Fatalf("version after cosmetic change = %d, want %d", versionAfter, versionBefore+1)
	}
	if contacts[0].Name != "Auntie R" || !contacts[0].Pinned || contacts[0].Order != 1 || contacts[0].Portrait != "p09" {
		t.Fatalf("cosmetic overlay not applied: %+v", contacts[0])
	}
	if event.Version != versionAfter {
		t.Fatalf("event.Version = %d, want %d", event.Version, versionAfter)
	}

	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading log after cosmetic change: %v", err)
	}
	if countLines(after) != beforeLines {
		t.Fatalf("cosmetic change appended to the log: before %d lines, after %d", beforeLines, countLines(after))
	}
}

// Non-cosmetic mutations append exactly one line, written only after
// ayllu.toml itself is durable (§7.6 ordering).
func TestChangeLog_AppendOrdering(t *testing.T) {
	s := openTestStore(t, 24)

	add := mustAdd(t, s, "dad", "Rosa", "rosa@example.com")
	if _, err := s.Mutate("mom", Mutation{Action: ActionDeactivate, ContactID: add.ContactID}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := s.Mutate("mom", Mutation{Action: ActionReactivate, ContactID: add.ContactID}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	data, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("log has %d lines, want 3 (add, deactivate, reactivate): %q", len(lines), data)
	}

	wantActions := []Action{ActionAdd, ActionDeactivate, ActionReactivate}
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON: %v (%q)", i, err, line)
		}
		if e.Action != wantActions[i] {
			t.Errorf("line %d action = %s, want %s", i, e.Action, wantActions[i])
		}
		if e.Actor == "" || e.ContactID == "" {
			t.Errorf("line %d missing actor/contact_id: %+v", i, e)
		}
	}

	// Addresses are permitted in the log (I-2 exempts this file) — the add
	// line must carry the address that never reaches the device.
	var addEvent Event
	if err := json.Unmarshal([]byte(lines[0]), &addEvent); err != nil {
		t.Fatalf("parsing add line: %v", err)
	}
	if addEvent.NewAddress != "rosa@example.com" {
		t.Fatalf("add log line NewAddress = %q, want rosa@example.com", addEvent.NewAddress)
	}
}

func TestPersist_AtomicRewriteLeavesParseableFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i, addr := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		mustAdd(t, s, "dad", string(rune('A'+i)), addr)
	}

	path := filepath.Join(dir, "ayllu.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading ayllu.toml: %v", err)
	}

	var ff fileFormat
	if err := toml.Unmarshal(data, &ff); err != nil {
		t.Fatalf("ayllu.toml did not parse after atomic rewrite: %v\n%s", err, data)
	}
	if ff.Version != 3 || len(ff.Contacts) != 3 {
		t.Fatalf("parsed file = %+v, want version 3 with 3 contacts", ff)
	}

	// No stray temp file left behind in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("stray temp file left behind: %s", e.Name())
		}
	}
}

// I-2: the device's view of the contact list must never carry an address.
func TestDeviceView_NoAddresses(t *testing.T) {
	s := openTestStore(t, 24)
	mustAdd(t, s, "dad", "Rosa", "rosa@example.com")
	mustAdd(t, s, "dad", "Tio Carlos", "carlos@example.com")
	add3 := mustAdd(t, s, "dad", "Old Friend", "friend@example.com")
	if _, err := s.Mutate("dad", Mutation{Action: ActionDeactivate, ContactID: add3.ContactID}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	view := s.DeviceView(0)
	if view == nil {
		t.Fatalf("DeviceView(0) = nil, want a block (version changed from 0)")
	}
	// Three real contacts plus the synthetic c_sys entry (see
	// TestF6_DeviceViewCarriesTheSystemContact).
	if len(view.Contacts) != 4 {
		t.Fatalf("DeviceView has %d contacts, want 4 (3 incl. tombstones, plus c_sys)", len(view.Contacts))
	}

	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshalling device view: %v", err)
	}
	if strings.Contains(string(data), "@") {
		t.Fatalf("device view contains an '@': %s", data)
	}

	// Version already current: no block needed.
	if got := s.DeviceView(view.Version); got != nil {
		t.Fatalf("DeviceView(current version) = %+v, want nil", got)
	}
}

func TestNormalizeAddress(t *testing.T) {
	cases := []struct{ a, b string }{
		{"rosa@Example.com", "rosa@example.com"},
		{"Name <rosa@example.com>", "rosa@example.com"},
		{"  rosa@example.com  ", "rosa@example.com"},
	}
	for _, c := range cases {
		if got := normalizeAddress(c.a); got != c.b {
			t.Errorf("normalizeAddress(%q) = %q, want %q", c.a, got, c.b)
		}
	}

	// Local part stays case-sensitive: only the domain is folded (§7.2).
	if a, b := normalizeAddress("Rosa@example.com"), normalizeAddress("rosa@example.com"); a == b {
		t.Errorf("local part was folded: %q == %q, want distinct", a, b)
	}
}

func countLines(b []byte) int {
	n := 0
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}

// TestReaddress_HistoryStillResolves pins the rule that makes a readdress safe
// in a read-time architecture: after Rosa moves to a new address, letters she
// sent from the old one must still resolve for derivation, or reconciliation
// sweeps her entire history into Held and a window resync loses it.
//
// This is the §7.1 failure arriving through the address rather than the row.
// The spec announces address changes (§7.4) but does not say what happens to
// the old address; retaining it for read-time resolution only is what keeps
// I-5 ("removal never deletes") true of addresses as well as contacts.
func TestReaddress_HistoryStillResolves(t *testing.T) {
	s := openTestStore(t, 24)
	id := mustAdd(t, s, "dad", "Rosa", "rosa@old.example").ContactID

	ev, err := s.Mutate("dad", Mutation{
		Action:    ActionReaddress,
		ContactID: id,
		Address:   "rosa@new.example",
	})
	if err != nil {
		t.Fatalf("readdress: %v", err)
	}
	if ev.OldAddress != "rosa@old.example" || ev.NewAddress != "rosa@new.example" {
		t.Fatalf("event addresses = %q -> %q", ev.OldAddress, ev.NewAddress)
	}

	// Read time: her old letters still render, with the right name.
	c, ok := s.Resolve("rosa@old.example")
	if !ok {
		t.Fatal("Resolve(old address) = false; her history would be quarantined")
	}
	if c.ID != id || c.Name != "Rosa" {
		t.Fatalf("Resolve(old) = %+v, want id %s named Rosa", c, id)
	}

	// Arrival: new mail from the old address is NOT auto-accepted. A readdress
	// usually means she lost that account; mail from it afterwards is exactly
	// what a guardian should review.
	if _, ok := s.ResolveActive("rosa@old.example"); ok {
		t.Error("ResolveActive(old address) = true; the channel silently reopened")
	}
	if _, ok := s.ResolveActive("rosa@new.example"); !ok {
		t.Error("ResolveActive(new address) = false; she cannot be written to")
	}

	// Re-adding her at the address she used before is still one person.
	reAdd := mustAdd(t, s, "dad", "Rosa", "rosa@old.example")
	if reAdd.ContactID != id {
		t.Fatalf("re-add at a past address made a new row %s, want %s", reAdd.ContactID, id)
	}
	if _, ok := s.Resolve("rosa@new.example"); !ok {
		t.Error("the address she just moved away from stopped resolving")
	}
}

// TestReaddress_DeviceViewStillHidesAddresses guards I-2 against the new field:
// past addresses are addresses, and the device must never see one.
func TestReaddress_DeviceViewStillHidesAddresses(t *testing.T) {
	s := openTestStore(t, 24)
	id := mustAdd(t, s, "dad", "Rosa", "rosa@old.example").ContactID
	if _, err := s.Mutate("dad", Mutation{
		Action:    ActionReaddress,
		ContactID: id,
		Address:   "rosa@new.example",
	}); err != nil {
		t.Fatalf("readdress: %v", err)
	}

	view := s.DeviceView(0)
	blob, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal device view: %v", err)
	}
	for _, leak := range []string{"@", "old.example", "new.example"} {
		if strings.Contains(string(blob), leak) {
			t.Fatalf("device view leaked %q (I-2): %s", leak, blob)
		}
	}
}

// TestF6_DeviceViewCarriesTheSystemContact: notice letters arrive with
// contact_id c_sys (§7.4), so without an entry for it the device receives
// letters from an id it has never heard of and cannot name the sender of —
// which is every announcement the system makes about the contact list.
//
// It ships Active false on purpose: the device's tombstone rule (render the
// name, hide from the compose picker) is exactly what §7.4 requires of a
// contact that cannot be written to.
func TestF6_DeviceViewCarriesTheSystemContact(t *testing.T) {
	s := openTestStore(t, 24)
	mustAdd(t, s, "dad", "Rosa", "rosa@example.com")

	view := s.DeviceView(0)
	if view == nil {
		t.Fatal("DeviceView returned nil after a change")
	}

	var sys *protocol.AylluContact
	for i := range view.Contacts {
		if view.Contacts[i].ID == protocol.SysContactID {
			sys = &view.Contacts[i]
		}
	}
	if sys == nil {
		t.Fatal("the device view has no c_sys entry; notice letters would name no sender")
	}
	if sys.Name == "" {
		t.Error("c_sys has no display name")
	}
	if sys.Active {
		t.Error("c_sys is active in the device view, so it would appear in the compose picker (§7.4: it cannot be written to)")
	}

	// It must not displace a real contact or leak an address.
	if len(view.Contacts) != 2 {
		t.Fatalf("device view holds %d contacts, want c_sys plus Rosa", len(view.Contacts))
	}
	blob, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "@") {
		t.Errorf("device view leaked an address (I-2): %s", blob)
	}
}

// TestContactIDsAreOpaque pins the three properties an id must have. A
// sequential scheme passes none of them, and nothing else in the suite would
// notice a regression to one: every other test looks up the id it was handed.
func TestContactIDsAreOpaque(t *testing.T) {
	s := openTestStore(t, 24)

	const n = 12
	ids := make([]string, 0, n)
	for i := range n {
		ev := mustAdd(t, s, "dad", fmt.Sprintf("Person %d", i), fmt.Sprintf("p%d@example.com", i))
		ids = append(ids, ev.ContactID)
	}

	// 1. Distinct.
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate contact id %q", id)
		}
		seen[id] = true
	}

	// 2. No ordinal, and no count. An id must not contain the position it was
	// added at, which is what made c_07 announce "at least seven contacts".
	for i, id := range ids {
		body, ok := strings.CutPrefix(id, "c_")
		if !ok {
			t.Fatalf("id %q lacks the c_ prefix", id)
		}
		if len(body) != idBodyLen {
			t.Errorf("id %q body is %d symbols, want %d", id, len(body), idBodyLen)
		}
		for _, r := range body {
			if !strings.ContainsRune(idAlphabet, r) {
				t.Errorf("id %q contains %q, which is outside the alphabet", id, r)
			}
		}
		for _, ordinal := range []string{
			fmt.Sprint(i), fmt.Sprint(i + 1), fmt.Sprintf("%02d", i+1),
		} {
			if body == ordinal || strings.HasPrefix(body, ordinal+"0") {
				t.Errorf("id %q looks like it encodes position %d", id, i)
			}
		}
	}

	// 3. Sort order carries no information about insertion order. With 12 ids
	// the chance of a random draw landing in insertion order is 1/12!, so a
	// match here means the ids are sequential, not that we got unlucky.
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if slices.Equal(sorted, ids) {
		t.Error("sorted ids match insertion order — the ids still encode when each contact was added")
	}

	// ...but sorting is still a stable total order, which resolution and the
	// device view rely on for a deterministic tie-break.
	again := append([]string(nil), ids...)
	sort.Strings(again)
	if !slices.Equal(again, sorted) {
		t.Error("sorting is not stable across calls")
	}
}

// TestContactIDsNeverCollideWithTheSystemContact: c_sys is reserved and
// resolves synthetically, so a real contact drawing that id would shadow every
// notice letter's sender.
func TestContactIDsNeverCollideWithTheSystemContact(t *testing.T) {
	for range 2000 {
		id, err := randomContactID()
		if err != nil {
			t.Fatalf("randomContactID: %v", err)
		}
		if id == protocol.SysContactID {
			t.Fatalf("minted the reserved system contact id %q", id)
		}
	}
}

// TestExistingSequentialIDsStillWork: a contact id is the permanent rendering
// key for every letter that person ever sent (§7.1), so ids already issued are
// never reassigned — an ayllu.toml written before this change must keep
// resolving exactly as it did.
func TestExistingSequentialIDsStillWork(t *testing.T) {
	dir := t.TempDir()
	legacy := `version = 3

[[contacts]]
id = "c_01"
name = "Rosa"
address = "rosa@example.com"
active = true

[[contacts]]
id = "c_02"
name = "Theo"
address = "theo@example.com"
active = false
`
	if err := os.WriteFile(filepath.Join(dir, "ayllu.toml"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}
	s, err := Open(dir, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	c, ok := s.Resolve("rosa@example.com")
	if !ok || c.ID != "c_01" {
		t.Fatalf("legacy contact did not resolve to its original id: %+v", c)
	}
	if tomb, ok := s.ByID("c_02"); !ok || tomb.Active {
		t.Fatalf("legacy tombstone did not survive: %+v", tomb)
	}

	// A new contact added alongside them gets the opaque form.
	ev := mustAdd(t, s, "dad", "New", "new@example.com")
	if !strings.HasPrefix(ev.ContactID, "c_") || len(ev.ContactID) != 2+idBodyLen {
		t.Errorf("new contact id %q is not the opaque form", ev.ContactID)
	}

	// And the legacy ids are still there, unrewritten.
	if _, ok := s.ByID("c_01"); !ok {
		t.Error("adding a contact rewrote an existing id")
	}
}

// TestSystemNameIsSafeForAChildToRead guards the one string a child sees on
// every announcement about their contact list — and, because notice letters
// graduate (§7.4), keeps seeing for as long as the archive lasts.
//
// It is pinned by test rather than left to judgement because it is cheap to
// change today and expensive later: an archive that changes sender name
// halfway through reads as two different correspondents.
func TestSystemNameIsSafeForAChildToRead(t *testing.T) {
	if SystemName == "" {
		t.Fatal("the system contact has no display name; notice letters would name no sender")
	}
	// §0.1's hard boundary: the internal identifiers never reach a
	// user-facing string, and this one is as user-facing as they get (V-14).
	for _, banned := range []string{"pututu", "ayllu", "kipu"} {
		if strings.Contains(strings.ToLower(SystemName), banned) {
			t.Errorf("system contact display name %q contains the internal word %q", SystemName, banned)
		}
	}
	// design-spec §0: user-facing names survive cold reading — no digraphs
	// with no correct answer, no apostrophes, no punctuation to puzzle over.
	for _, r := range SystemName {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			t.Errorf("system contact display name %q contains %q; keep it plainly readable", SystemName, r)
		}
	}
}
