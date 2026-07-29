package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tholent/chaskiwasi/internal/ayllu"
)

func TestContactAdd(t *testing.T) {
	tests := []struct {
		name    string
		form    url.Values
		wantTo  string
		wantAdd bool
	}{
		{"plain", url.Values{"name": {"Rosa"}, "address": {"rosa@example.test"}}, "/contacts?m=contact-added", true},
		{"display-name form", url.Values{"name": {"Rosa"}, "address": {"Rosa T <rosa@example.test>"}}, "/contacts?m=contact-added", true},
		{"no name", url.Values{"name": {"  "}, "address": {"rosa@example.test"}}, "/contacts?m=contact-invalid", false},
		{"no address", url.Values{"name": {"Rosa"}, "address": {""}}, "/contacts?m=contact-invalid", false},
		{"not an address", url.Values{"name": {"Rosa"}, "address": {"rosa at example"}}, "/contacts?m=contact-invalid", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addGuardian("dad")
			cookie := h.login("dad", testPassword)

			mustRedirect(t, h.post("/contacts/add", "/contacts", tc.form, cookie), tc.wantTo)

			_, contacts := h.ayllu.List()
			if got := len(contacts) == 1; got != tc.wantAdd {
				t.Fatalf("contact stored = %v, want %v", got, tc.wantAdd)
			}
			if !tc.wantAdd {
				if len(h.announcer.events) != 0 {
					t.Error("a refused change produced a notice event")
				}
				return
			}
			if contacts[0].Address != "rosa@example.test" {
				t.Errorf("stored address = %q, want the bare addr-spec", contacts[0].Address)
			}
			// I-4: every mutation reaches the notice pipeline.
			if len(h.announcer.events) != 1 || h.announcer.events[0].Action != ayllu.ActionAdd {
				t.Fatalf("notice events = %+v, want one add", h.announcer.events)
			}
			if h.announcer.events[0].Actor != "dad" {
				t.Errorf("event actor = %q, want the signed-in guardian", h.announcer.events[0].Actor)
			}
		})
	}
}

// TestContactAdd_MaxContactsIsAClearError covers A.3: the cap is surfaced, not
// swallowed. The harness config sets max_contacts = 4.
func TestContactAdd_MaxContactsIsAClearError(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	for _, addr := range []string{"a@example.test", "b@example.test", "c@example.test", "d@example.test"} {
		rec := h.post("/contacts/add", "/contacts", url.Values{
			"name": {"Contact"}, "address": {addr},
		}, cookie)
		mustRedirect(t, rec, "/contacts?m=contact-added")
	}

	rec := h.post("/contacts/add", "/contacts", url.Values{
		"name": {"One too many"}, "address": {"e@example.test"},
	}, cookie)
	mustRedirect(t, rec, "/contacts?m=contacts-full")

	page := h.get("/contacts?m=contacts-full", cookie).Body.String()
	if !strings.Contains(page, flashes[flashContactsFull].Text) {
		t.Fatal("the full-list error is not shown to the guardian")
	}
	if strings.Contains(page, "e@example.test") {
		t.Fatal("the refused contact was stored anyway")
	}
	// A refused add must not produce a notice: nothing changed.
	if len(h.announcer.events) != 4 {
		t.Errorf("notice events = %d, want 4 (the refused add must not announce)", len(h.announcer.events))
	}
}

func TestContactLifecycle(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, cookie), "/contacts?m=contact-added")

	_, contacts := h.ayllu.List()
	id := contacts[0].ID

	steps := []struct {
		path   string
		form   url.Values
		wantTo string
	}{
		{"/contacts/deactivate", url.Values{"contact_id": {id}}, "/contacts?m=contact-off"},
		{"/contacts/reactivate", url.Values{"contact_id": {id}}, "/contacts?m=contact-on"},
		{"/contacts/readdress", url.Values{"contact_id": {id}, "address": {"rosa2@example.test"}}, "/contacts?m=contact-address"},
	}
	for _, st := range steps {
		mustRedirect(t, h.post(st.path, "/contacts", st.form, cookie), st.wantTo)
	}

	c, ok := h.ayllu.ByID(id)
	if !ok {
		t.Fatal("the contact disappeared")
	}
	if c.Address != "rosa2@example.test" {
		t.Errorf("address = %q, want the new one", c.Address)
	}
	// I-5 / F-1: the old address is retained so history keeps resolving.
	if len(c.PastAddresses) != 1 || c.PastAddresses[0] != "rosa@example.test" {
		t.Errorf("past addresses = %v, want the original retained", c.PastAddresses)
	}
	if _, ok := h.ayllu.Resolve("rosa@example.test"); !ok {
		t.Error("the old address stopped resolving at read time")
	}

	if len(h.announcer.events) != 4 {
		t.Fatalf("notice events = %d, want 4 (add, deactivate, reactivate, readdress)", len(h.announcer.events))
	}
	wantActions := []ayllu.Action{ayllu.ActionAdd, ayllu.ActionDeactivate, ayllu.ActionReactivate, ayllu.ActionReaddress}
	for i, want := range wantActions {
		if h.announcer.events[i].Action != want {
			t.Errorf("event %d = %q, want %q", i, h.announcer.events[i].Action, want)
		}
	}
	last := h.announcer.events[3]
	if last.OldAddress != "rosa@example.test" || last.NewAddress != "rosa2@example.test" {
		t.Errorf("readdress event carried %q -> %q, want both addresses for the change log",
			last.OldAddress, last.NewAddress)
	}
}

func TestContactChange_SurvivesAFailedAnnouncement(t *testing.T) {
	// §7.6: the mutation is durable before the notice is attempted, so a
	// failure to announce is "the notice arrives late", never "the change
	// happened silently" — and never "the change was rolled back".
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)
	h.announcer.err = errNoticeUnavailable

	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, cookie), "/contacts?m=notice-late")

	if _, ok := h.ayllu.Resolve("rosa@example.test"); !ok {
		t.Fatal("the contact was lost because the notice could not be sent")
	}
}

func TestContactMutations_RejectAnUnknownContact(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	for _, path := range []string{"/contacts/deactivate", "/contacts/reactivate"} {
		mustRedirect(t, h.post(path, "/contacts", url.Values{"contact_id": {"c_99"}}, cookie),
			"/contacts?m=contact-invalid")
	}
	mustRedirect(t, h.post("/contacts/readdress", "/contacts", url.Values{
		"contact_id": {"c_99"}, "address": {"x@example.test"},
	}, cookie), "/contacts?m=contact-invalid")

	if len(h.announcer.events) != 0 {
		t.Errorf("refused mutations produced %d notice events, want 0", len(h.announcer.events))
	}
}

func TestContactsPage_StatesTheHonestLimitation(t *testing.T) {
	// §7.3: a contact removed for a safety reason still has their history
	// readable, and this system cannot offer retroactive removal. Guardians
	// should learn that from the interface, not during an argument — so the
	// page says it, next to the button that does it.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	body := h.get("/contacts", cookie).Body.String()
	for _, phrase := range []string{
		"does not remove the letters they already sent",
		"cannot take them back",
		"safety reason",
	} {
		if !strings.Contains(body, phrase) {
			t.Errorf("the contacts page does not state §7.3's limitation: missing %q", phrase)
		}
	}
}

func TestContactsPage_ShowsTheCapIncludingTombstones(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, cookie), "/contacts?m=contact-added")
	_, contacts := h.ayllu.List()
	mustRedirect(t, h.post("/contacts/deactivate", "/contacts", url.Values{
		"contact_id": {contacts[0].ID},
	}, cookie), "/contacts?m=contact-off")

	body := h.get("/contacts", cookie).Body.String()
	if !strings.Contains(body, "0 active, 1 of 4 places used") {
		t.Fatalf("the page does not show that a removed contact still holds a place:\n%s", body)
	}
}

func TestContacts_HandlerNeverWritesTOMLItself(t *testing.T) {
	// §9.1: contact CRUD writes ayllu.toml via the store, never by hand. The
	// proof that matters is the file the store produced parsing back as a
	// contact table with the version the store assigned.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, cookie), "/contacts?m=contact-added")

	version, contacts := h.ayllu.List()
	if version == 0 || len(contacts) != 1 {
		t.Fatalf("store version %d with %d contacts, want a bumped version and one row", version, len(contacts))
	}
}

func TestContactsPage_RequiresASessionForWrites(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")

	rec := h.do(h.request(http.MethodPost, "/contacts/add", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, nil))
	mustRedirect(t, rec, "/login?m=signed-out")

	if _, contacts := h.ayllu.List(); len(contacts) != 0 {
		t.Fatal("an unauthenticated request changed the contact list")
	}
}
