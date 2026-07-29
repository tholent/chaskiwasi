package filing

import (
	"context"
	"testing"

	"github.com/tholent/chaskiwasi/internal/ayllu"
)

// TestV18_ReleaseStranger_AddsContactMovesAndRings mirrors V-18's stranger
// flow: "add-then-release delivers it, add notice fires, pututu rings."
// This package doesn't send the notice letter itself (that's wave 3's
// internal/notice, fed by the returned ayllu.Event), but everything else in
// that sentence is this package's job.
func TestV18_ReleaseStranger_AddsContactMovesAndRings(t *testing.T) {
	store := openTestAyllu(t)
	mb := newFakeMailbox()
	held := mb.seed("Held", rawSeedMessage("stranger@example.com", "<held-1@x>"))[0]

	bell := &doorbellSpy{}
	svc := newTestService(t, mb, store, bell)

	event, err := svc.ReleaseStranger(context.Background(), held, "dad", "Stranger")
	if err != nil {
		t.Fatalf("ReleaseStranger: %v", err)
	}
	if event.Action != ayllu.ActionAdd {
		t.Errorf("event.Action = %q, want %q", event.Action, ayllu.ActionAdd)
	}

	c, ok := store.ResolveActive("stranger@example.com")
	if !ok || c.ID != event.ContactID || !c.Active {
		t.Fatalf("ResolveActive after release = (%+v, %v), want active contact %s", c, ok, event.ContactID)
	}

	if got := mb.uidsIn("Held"); len(got) != 0 {
		t.Errorf("Held after release = %v, want empty", got)
	}
	if got := mb.uidsIn(inboxFolder); len(got) != 1 {
		t.Fatalf("INBOX after release = %v, want exactly one message", got)
	}
	// §8: "a released message receives a new UID above the cursor."
	if got := mb.uidsIn(inboxFolder)[0]; got == held {
		t.Errorf("released message kept its Held UID %d, want a new UID assigned by the move", held)
	}
	if bell.count() != 1 {
		t.Errorf("doorbell rang %d times on release, want 1", bell.count())
	}
}

func TestReleaseStranger_MaxContactsFull_NothingVanishes(t *testing.T) {
	dir := t.TempDir()
	store, err := ayllu.Open(dir, 1)
	if err != nil {
		t.Fatalf("ayllu.Open: %v", err)
	}
	mustAddContact(t, store, "Existing", "existing@example.com")

	mb := newFakeMailbox()
	held := mb.seed("Held", rawSeedMessage("stranger@example.com", "<held-2@x>"))[0]

	bell := &doorbellSpy{}
	svc := newTestService(t, mb, store, bell)

	_, err = svc.ReleaseStranger(context.Background(), held, "dad", "Stranger")
	if err == nil {
		t.Fatal("ReleaseStranger with a full contact list: want error, got nil")
	}

	// Nothing vanished (V-18): the message is still exactly where it was,
	// and no new contact was created.
	if got := mb.uidsIn("Held"); !containsUID(got, held) {
		t.Errorf("Held = %v, want the message still present after a failed release", got)
	}
	if got := mb.uidsIn(inboxFolder); len(got) != 0 {
		t.Errorf("INBOX = %v, want empty — a failed add must not move anything", got)
	}
	if _, ok := store.ResolveActive("stranger@example.com"); ok {
		t.Errorf("stranger unexpectedly resolves active after a failed release")
	}
	if bell.count() != 0 {
		t.Errorf("doorbell rang %d times on a failed release, want 0", bell.count())
	}
}

// TestV18_ReleaseDeactivated_ReactivatesMovesAndRings mirrors V-18's second
// flow: "deactivated contact's letter -> reactivate-release-deactivate
// delivers it and future mail still goes to Held." This test covers
// reactivate+release; the final re-deactivate step is a second, independent
// ayllu.Store.Mutate call the guardian UI makes afterward, deliberately not
// chained by this package (see ReleaseDeactivated's doc comment).
func TestV18_ReleaseDeactivated_ReactivatesMovesAndRings(t *testing.T) {
	store := openTestAyllu(t)
	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")
	mustDeactivate(t, store, rosa.ID)

	mb := newFakeMailbox()
	held := mb.seed("Held", rawSeedMessage("rosa@example.com", "<held-3@x>"))[0]

	bell := &doorbellSpy{}
	svc := newTestService(t, mb, store, bell)

	event, err := svc.ReleaseDeactivated(context.Background(), held, "dad", rosa.ID)
	if err != nil {
		t.Fatalf("ReleaseDeactivated: %v", err)
	}
	if event.Action != ayllu.ActionReactivate {
		t.Errorf("event.Action = %q, want %q", event.Action, ayllu.ActionReactivate)
	}

	c, ok := store.ByID(rosa.ID)
	if !ok || !c.Active {
		t.Fatalf("contact after release = (%+v, %v), want active", c, ok)
	}
	if got := mb.uidsIn("Held"); len(got) != 0 {
		t.Errorf("Held after release = %v, want empty", got)
	}
	if got := mb.uidsIn(inboxFolder); len(got) != 1 {
		t.Fatalf("INBOX after release = %v, want exactly one message", got)
	}
	if bell.count() != 1 {
		t.Errorf("doorbell rang %d times on release, want 1", bell.count())
	}

	// "future mail still goes to Held" until a guardian releases again:
	// re-deactivate (the guardian UI's third step) and confirm the channel
	// is closed to new mail once more.
	mustDeactivate(t, store, rosa.ID)
	mb.seed(inboxFolder, rawSeedMessage("rosa@example.com", "<new-after-redeactivate@x>"))
	if err := svc.HandleNotify(context.Background()); err != nil {
		t.Fatalf("HandleNotify: %v", err)
	}
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Errorf("Held after re-deactivation = %v, want the new letter quarantined", got)
	}
}

func TestReleaseDeactivated_MismatchedContactRejected_NothingVanishes(t *testing.T) {
	store := openTestAyllu(t)
	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")
	mustDeactivate(t, store, rosa.ID)
	other := mustAddContact(t, store, "Other", "other@example.com")
	mustDeactivate(t, store, other.ID)

	mb := newFakeMailbox()
	held := mb.seed("Held", rawSeedMessage("rosa@example.com", "<held-4@x>"))[0]

	bell := &doorbellSpy{}
	svc := newTestService(t, mb, store, bell)

	// Claim the Held message (actually from Rosa) belongs to a different
	// deactivated contact.
	_, err := svc.ReleaseDeactivated(context.Background(), held, "dad", other.ID)
	if err == nil {
		t.Fatal("ReleaseDeactivated with a mismatched contact id: want error, got nil")
	}

	c, ok := store.ByID(other.ID)
	if !ok || c.Active {
		t.Errorf("mismatched contact %+v was reactivated, want untouched (still inactive)", c)
	}
	rosaAfter, ok := store.ByID(rosa.ID)
	if !ok || rosaAfter.Active {
		t.Errorf("Rosa %+v was reactivated by a mismatched request, want untouched", rosaAfter)
	}
	if got := mb.uidsIn("Held"); !containsUID(got, held) {
		t.Errorf("Held = %v, want the message still present after a rejected release", got)
	}
	if bell.count() != 0 {
		t.Errorf("doorbell rang %d times on a rejected release, want 0", bell.count())
	}
}

func TestReleaseStranger_UIDNotInHeld(t *testing.T) {
	store := openTestAyllu(t)
	mb := newFakeMailbox()
	svc := newTestService(t, mb, store, nil)

	if _, err := svc.ReleaseStranger(context.Background(), 999, "dad", "Nobody"); err == nil {
		t.Fatal("ReleaseStranger for a UID not in Held: want error, got nil")
	}
}
