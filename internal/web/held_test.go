package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
)

func heldUIDs(h *harness) []uint32 {
	h.t.Helper()
	msgs, err := h.mailbox.List(h.t.Context(), heldFolder)
	if err != nil {
		h.t.Fatalf("listing held: %v", err)
	}
	uids := make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		uids = append(uids, m.UID)
	}
	return uids
}

func TestHeldList_ClassifiesEverySender(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	// A deactivated contact: added, then removed.
	if _, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionAdd, Name: "Rosa", Address: "rosa@example.test",
	}); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	_, contacts := h.ayllu.List()
	rosaID := contacts[0].ID
	if _, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionDeactivate, ContactID: rosaID,
	}); err != nil {
		t.Fatalf("seed deactivate: %v", err)
	}
	// An active contact whose older letter is still sitting in Held.
	if _, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionAdd, Name: "Tio", Address: "tio@example.test",
	}); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	at := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	h.mailbox.put(heldFolder, letter("Someone New <new@example.test>", "hello", "hi"), at)
	h.mailbox.put(heldFolder, letter("rosa@example.test", "later", "hi"), at.Add(time.Hour))
	h.mailbox.put(heldFolder, letter("tio@example.test", "earlier", "hi"), at.Add(2*time.Hour))
	h.mailbox.put(heldFolder, []byte("this is not a message at all\r\n"), at.Add(3*time.Hour))

	list := h.server.heldList(t.Context(), "token")
	if len(list.Messages) != 4 {
		t.Fatalf("held messages = %d, want 4", len(list.Messages))
	}

	byFrom := map[string]heldView{}
	for _, m := range list.Messages {
		byFrom[m.From] = m
	}
	tests := []struct {
		from     string
		wantKind heldKind
	}{
		{"new@example.test", heldStranger},
		{"rosa@example.test", heldDeactivated},
		{"tio@example.test", heldKnown},
		{"", heldUnreadable},
	}
	for _, tc := range tests {
		got, ok := byFrom[tc.from]
		if !ok {
			t.Fatalf("no held row for sender %q", tc.from)
		}
		if got.Kind != tc.wantKind {
			t.Errorf("sender %q classified %q, want %q", tc.from, got.Kind, tc.wantKind)
		}
	}
	if byFrom["rosa@example.test"].ContactID != rosaID {
		t.Errorf("deactivated row carries contact id %q, want %q", byFrom["rosa@example.test"].ContactID, rosaID)
	}
	if byFrom["new@example.test"].ContactName != "Someone New" {
		t.Errorf("stranger row suggests name %q, want the sender's display name",
			byFrom["new@example.test"].ContactName)
	}

	// Newest first.
	for i := 1; i < len(list.Messages); i++ {
		if list.Messages[i-1].Received.Before(list.Messages[i].Received) {
			t.Fatal("held messages are not newest-first")
		}
	}

	body := h.get("/held", cookie).Body.String()
	for _, want := range []string{
		"Add as contact, then deliver",
		"Restore contact, then deliver",
		"cannot be read",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the held page is missing the control for %q", want)
		}
	}
}

// TestV18_StrangerReleaseGoesThroughFiling is the UI half of V-18: the
// add-then-release flow must run through filing, and the message must end up
// delivered — never silently dropped.
func TestV18_StrangerReleaseGoesThroughFiling(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	h.mailbox.put(heldFolder, letter("new@example.test", "hello", "hi"),
		time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC))
	uid := heldUIDs(h)[0]

	rec := h.post("/held/release", "/held", url.Values{
		"uid":  {strconv.FormatUint(uint64(uid), 10)},
		"name": {"Someone New"},
	}, cookie)
	mustRedirect(t, rec, "/held?m=released")

	if len(h.releaser.calls) != 1 || !strings.HasPrefix(h.releaser.calls[0], "stranger ") {
		t.Fatalf("release calls = %v, want one ReleaseStranger", h.releaser.calls)
	}
	if h.mailbox.count(heldFolder) != 0 {
		t.Error("the message is still held after a successful release")
	}
	if h.mailbox.count("INBOX") != 1 {
		t.Fatal("the released message did not reach INBOX — a release must never make mail vanish")
	}

	c, ok := h.ayllu.ResolveActive("new@example.test")
	if !ok {
		t.Fatal("the sender was not added as an active contact")
	}
	if c.Name != "Someone New" {
		t.Errorf("contact name = %q, want the guardian-supplied name", c.Name)
	}
	if len(h.announcer.events) != 1 || h.announcer.events[0].Action != ayllu.ActionAdd {
		t.Fatalf("notice events = %+v, want one add (§7.4)", h.announcer.events)
	}
}

// TestV18_DeactivatedReleaseGoesThroughFiling is the other half: reactivate,
// release, and — the guardian's own third step — deactivate again, all three
// announced (§8).
func TestV18_DeactivatedReleaseGoesThroughFiling(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	if _, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionAdd, Name: "Rosa", Address: "rosa@example.test",
	}); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	_, contacts := h.ayllu.List()
	rosaID := contacts[0].ID
	if _, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionDeactivate, ContactID: rosaID,
	}); err != nil {
		t.Fatalf("seed deactivate: %v", err)
	}

	h.mailbox.put(heldFolder, letter("rosa@example.test", "one last note", "hi"),
		time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC))
	uid := heldUIDs(h)[0]

	rec := h.post("/held/release", "/held", url.Values{
		"uid": {strconv.FormatUint(uint64(uid), 10)},
	}, cookie)
	mustRedirect(t, rec, "/held?m=released")

	if len(h.releaser.calls) != 1 || !strings.Contains(h.releaser.calls[0], "deactivated ") {
		t.Fatalf("release calls = %v, want one ReleaseDeactivated", h.releaser.calls)
	}
	if h.mailbox.count("INBOX") != 1 || h.mailbox.count(heldFolder) != 0 {
		t.Fatal("the released message did not move from Held to INBOX")
	}
	if c, _ := h.ayllu.ByID(rosaID); !c.Active {
		t.Fatal("the contact was not reactivated")
	}
	if len(h.announcer.events) != 1 || h.announcer.events[0].Action != ayllu.ActionReactivate {
		t.Fatalf("notice events = %+v, want one reactivate", h.announcer.events)
	}

	// Step three of §8's documented sequence, performed by the guardian: the
	// channel closes again, and it is announced like everything else.
	mustRedirect(t, h.post("/contacts/deactivate", "/contacts", url.Values{
		"contact_id": {rosaID},
	}, cookie), "/contacts?m=contact-off")

	if c, _ := h.ayllu.ByID(rosaID); c.Active {
		t.Fatal("the contact is still active after the third step")
	}
	if len(h.announcer.events) != 2 {
		t.Fatalf("notice events = %d, want 2 — every step of the sequence is announced", len(h.announcer.events))
	}
	// The delivered letter still renders: read-time resolution consults
	// tombstones (§7.2).
	if _, ok := h.ayllu.Resolve("rosa@example.test"); !ok {
		t.Error("the delivered letter's sender stopped resolving after re-deactivation")
	}
	// And new mail from her is held again.
	if _, ok := h.ayllu.ResolveActive("rosa@example.test"); ok {
		t.Error("the channel stayed open after the third step")
	}
}

func TestHeldRelease_ChoosesTheFlowFromTheMailboxNotTheForm(t *testing.T) {
	// A posted claim that a sender is a stranger must not drive the
	// add-a-contact path for a message whose sender is really a tombstone.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	if _, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionAdd, Name: "Rosa", Address: "rosa@example.test",
	}); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	_, contacts := h.ayllu.List()
	if _, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionDeactivate, ContactID: contacts[0].ID,
	}); err != nil {
		t.Fatalf("seed deactivate: %v", err)
	}

	h.mailbox.put(heldFolder, letter("rosa@example.test", "note", "hi"), time.Now())
	uid := heldUIDs(h)[0]

	// The form claims a fresh contact under a different name.
	mustRedirect(t, h.post("/held/release", "/held", url.Values{
		"uid":  {strconv.FormatUint(uint64(uid), 10)},
		"name": {"Not Rosa At All"},
	}, cookie), "/held?m=released")

	if !strings.Contains(h.releaser.calls[0], "deactivated ") {
		t.Fatalf("release calls = %v, want the reactivate path chosen server-side", h.releaser.calls)
	}
	if _, count := h.ayllu.List(); len(count) != 1 {
		t.Fatalf("contacts = %d, want 1 — the form created a second row", len(count))
	}
	if c, _ := h.ayllu.ByID(contacts[0].ID); c.Name != "Rosa" {
		t.Errorf("contact name = %q, want the name unchanged by the release form", c.Name)
	}
}

func TestHeldRelease_Failures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*harness) url.Values
	}{
		{
			"unknown uid",
			func(h *harness) url.Values { return url.Values{"uid": {"9999"}} },
		},
		{
			"uid is not a number",
			func(h *harness) url.Values { return url.Values{"uid": {"not-a-uid"}} },
		},
		{
			"stranger with no name given",
			func(h *harness) url.Values {
				h.mailbox.put(heldFolder, letter("new@example.test", "hello", "hi"), time.Now())
				return url.Values{"uid": {strconv.FormatUint(uint64(heldUIDs(h)[0]), 10)}, "name": {"  "}}
			},
		},
		{
			"sender cannot be read",
			func(h *harness) url.Values {
				h.mailbox.put(heldFolder, []byte("not a message\r\n"), time.Now())
				return url.Values{"uid": {strconv.FormatUint(uint64(heldUIDs(h)[0]), 10)}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addGuardian("dad")
			cookie := h.login("dad", testPassword)

			form := tc.setup(h)
			before := h.mailbox.count(heldFolder)

			mustRedirect(t, h.post("/held/release", "/held", form, cookie), "/held?m=release-failed")

			// V-18's core promise: a failed release leaves the message exactly
			// where it was. Nothing ever silently vanishes.
			if got := h.mailbox.count(heldFolder); got != before {
				t.Fatalf("held messages = %d, want %d — a failed release moved mail", got, before)
			}
			if h.mailbox.count("INBOX") != 0 {
				t.Fatal("a failed release delivered the message anyway")
			}
			if len(h.announcer.events) != 0 {
				t.Errorf("a failed release announced %d changes", len(h.announcer.events))
			}
		})
	}
}

func TestHeldRelease_MailboxFailureLeavesTheMessageHeld(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	h.mailbox.put(heldFolder, letter("new@example.test", "hello", "hi"), time.Now())
	uid := heldUIDs(h)[0]

	form := url.Values{"uid": {strconv.FormatUint(uint64(uid), 10)}, "name": {"Someone New"}}
	form.Set(csrfField, h.csrfFrom("/held", cookie))

	// The MOVE fails after the contact change is already durable. §8: the
	// contact change stands and the message is still in Held, available for a
	// retried release.
	h.mailbox.moveErr = errors.New("imap: move refused")
	mustRedirect(t, h.do(h.request(http.MethodPost, "/held/release", form, cookie)), "/held?m=release-failed")

	if h.mailbox.count(heldFolder) != 1 {
		t.Fatal("the message left Held even though the move failed")
	}
	if _, ok := h.ayllu.ResolveActive("new@example.test"); !ok {
		t.Error("the durable contact change was rolled back by a failed move")
	}

	// Retrying once the mailbox recovers delivers it.
	h.mailbox.moveErr = nil
	form.Set(csrfField, h.csrfFrom("/held", cookie))
	mustRedirect(t, h.do(h.request(http.MethodPost, "/held/release", form, cookie)), "/held?m=released")
	if h.mailbox.count("INBOX") != 1 || h.mailbox.count(heldFolder) != 0 {
		t.Fatal("the retried release did not deliver the message")
	}
}

func TestHeldList_UnreachableMailboxSaysSoRatherThanShowingNothing(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	h.mailbox.listErr = errors.New("imap: connection refused")

	body := h.get("/held", cookie).Body.String()
	if !strings.Contains(body, "could not be reached") {
		t.Fatal("an unreachable mailbox renders as an empty Held list")
	}
	if strings.Contains(body, "Nothing is waiting for review") {
		t.Fatal("an unreachable mailbox claims nothing is held")
	}
}

func TestHeldPage_DocumentsTheThreeStepSequence(t *testing.T) {
	// §8: "the UI documents that sequence rather than offering a silent
	// one-off override."
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	body := h.get("/held", cookie).Body.String()
	for _, phrase := range []string{
		"Restore the contact, deliver the message, then remove the contact again",
		"no quiet one-off delivery",
	} {
		if !strings.Contains(body, phrase) {
			t.Errorf("the held page does not document §8's sequence: missing %q", phrase)
		}
	}
}

func TestHeldFragment_CarriesAUsableToken(t *testing.T) {
	// The htmx-swapped fragment is rendered standalone, so it has to issue its
	// own token or every release form it swaps in would be dead on arrival.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	h.mailbox.put(heldFolder, letter("new@example.test", "hello", "hi"), time.Now())
	uid := heldUIDs(h)[0]

	token := h.csrfFrom("/held/list", cookie)
	mustRedirect(t, h.do(h.request(http.MethodPost, "/held/release", url.Values{
		csrfField: {token},
		"uid":     {strconv.FormatUint(uint64(uid), 10)},
		"name":    {"Someone New"},
	}, cookie)), "/held?m=released")
}

// TestF7_ReleasingAnAlreadyActiveSenderAnnouncesNothing pins the third release
// case §8 does not name but that a guardian reaches routinely: add a stranger
// on the contacts page, then release the letter of theirs that was already
// sitting in Held.
//
// Routing that through "reactivate, then release" works mechanically and puts a
// false statement into the child's inbox — "added back to your list" for a
// change that never happened. I-4 exists so both parties can trust that the
// list changed exactly when a letter says it did; a notice describing a
// non-change corrupts that record more than a missing one would.
func TestF7_ReleasingAnAlreadyActiveSenderAnnouncesNothing(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	// Theo is an active contact and a letter of his is sitting in Held —
	// exactly the state left behind by adding him after his mail arrived.
	ev, err := h.ayllu.Mutate("dad", ayllu.Mutation{
		Action: ayllu.ActionAdd, Name: "Theo", Address: "theo@example.test",
	})
	if err != nil {
		t.Fatalf("add contact: %v", err)
	}
	h.announcer.events = nil // the add itself is announced; ignore it here

	h.mailbox.put(heldFolder, letter("theo@example.test", "hello again", "hi"),
		time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC))
	uid := heldUIDs(h)[0]

	rec := h.post("/held/release", "/held", url.Values{
		"uid": {strconv.FormatUint(uint64(uid), 10)},
	}, cookie)
	mustRedirect(t, rec, "/held?m=released")

	if got := len(h.announcer.events); got != 0 {
		t.Fatalf("released an already-active sender and announced %d change(s): %+v",
			got, h.announcer.events)
	}
	for _, c := range h.releaser.calls {
		if strings.HasPrefix(c, "deactivated") {
			t.Fatalf("took the reactivate path for an active contact: %v", h.releaser.calls)
		}
	}

	// "Announce nothing" must not become "do nothing" (V-18).
	if h.mailbox.count("INBOX") != 1 {
		t.Fatal("the released message did not reach INBOX")
	}
	if h.mailbox.count(heldFolder) != 0 {
		t.Error("the message is still held after a successful release")
	}

	// And the contact is untouched.
	c, ok := h.ayllu.ByID(ev.ContactID)
	if !ok || !c.Active {
		t.Fatalf("contact %s changed during a no-op release: %+v", ev.ContactID, c)
	}
}
