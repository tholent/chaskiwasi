package filing

import (
	"context"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/mailbox"
)

func TestArrival_ActiveContact_KeptAndRingsDoorbell(t *testing.T) {
	store := openTestAyllu(t)
	mustAddContact(t, store, "Rosa", "rosa@example.com")

	mb := newFakeMailbox()
	uids := mb.seed(inboxFolder, rawSeedMessage("rosa@example.com", "<1@x>"))

	bell := &doorbellSpy{}
	svc := newTestService(t, mb, store, bell)

	if err := svc.HandleNotify(context.Background()); err != nil {
		t.Fatalf("HandleNotify: %v", err)
	}

	if !containsUID(mb.uidsIn(inboxFolder), uids[0]) {
		t.Errorf("active contact's letter was moved out of INBOX, want kept")
	}
	if got := mb.uidsIn("Held"); len(got) != 0 {
		t.Errorf("Held = %v, want empty", got)
	}
	if bell.count() != 1 {
		t.Errorf("doorbell rang %d times, want 1", bell.count())
	}
}

func TestArrival_Stranger_QuarantinedNoRing(t *testing.T) {
	store := openTestAyllu(t)

	mb := newFakeMailbox()
	mb.seed(inboxFolder, rawSeedMessage("stranger@example.com", "<2@x>"))

	bell := &doorbellSpy{}
	svc := newTestService(t, mb, store, bell)

	if err := svc.HandleNotify(context.Background()); err != nil {
		t.Fatalf("HandleNotify: %v", err)
	}

	if got := mb.uidsIn(inboxFolder); len(got) != 0 {
		t.Errorf("INBOX = %v, want empty (stranger should be quarantined)", got)
	}
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Errorf("Held = %v, want exactly one message", got)
	}
	if bell.count() != 0 {
		t.Errorf("doorbell rang %d times for a stranger, want 0", bell.count())
	}
}

func TestArrival_DeactivatedContact_QuarantinedNoRing(t *testing.T) {
	store := openTestAyllu(t)
	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")
	mustDeactivate(t, store, rosa.ID)

	mb := newFakeMailbox()
	mb.seed(inboxFolder, rawSeedMessage("rosa@example.com", "<3@x>"))

	bell := &doorbellSpy{}
	svc := newTestService(t, mb, store, bell)

	if err := svc.HandleNotify(context.Background()); err != nil {
		t.Fatalf("HandleNotify: %v", err)
	}

	if got := mb.uidsIn(inboxFolder); len(got) != 0 {
		t.Errorf("INBOX = %v, want empty (deactivated contact's new mail should be quarantined)", got)
	}
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Errorf("Held = %v, want exactly one message", got)
	}
	if bell.count() != 0 {
		t.Errorf("doorbell rang %d times for a deactivated contact, want 0", bell.count())
	}
}

func TestArrival_UnparseableSender_Quarantined(t *testing.T) {
	store := openTestAyllu(t)

	mb := newFakeMailbox()
	// No From header at all.
	mb.seed(inboxFolder, rawSeedMessage("", "<4@x>"))

	svc := newTestService(t, mb, store, nil)
	if err := svc.HandleNotify(context.Background()); err != nil {
		t.Fatalf("HandleNotify: %v", err)
	}
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Errorf("Held = %v, want exactly one message (default-deny on unparseable sender)", got)
	}
}

// TestV15_ReconciliationCatchesStrangerBeforeAnySync mirrors V-15: a
// stranger's mail arrives while Wasi is down (simulated by seeding directly
// into INBOX, bypassing HandleNotify entirely — nothing has "decided" this
// message yet), and Start's initial Reconcile must quarantine it before
// anything else runs.
func TestV15_ReconciliationCatchesStrangerBeforeAnySync(t *testing.T) {
	store := openTestAyllu(t)

	mb := newFakeMailbox()
	mb.seed(inboxFolder, rawSeedMessage("stranger@example.com", "<5@x>"))

	svc := newTestService(t, mb, store, nil)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := mb.uidsIn(inboxFolder); len(got) != 0 {
		t.Errorf("INBOX = %v, want empty after startup reconciliation", got)
	}
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Errorf("Held = %v, want the stranger's message", got)
	}
}

// TestF2_ReconcileDoesNotSweepDeactivatedContactHistory is V-6's shape
// applied specifically to reconciliation, guarding the exact regression F-2
// exists to document: two letters arrive from an active contact, the
// contact is deactivated, and a reconciliation pass must NOT retroactively
// move the first two letters to Held. Only a third, genuinely new letter
// (which never got an arrival-time decision) is quarantined, and only
// because HandleNotify's active-only check — not Reconcile — puts it there.
func TestF2_ReconcileDoesNotSweepDeactivatedContactHistory(t *testing.T) {
	store := openTestAyllu(t)
	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")

	mb := newFakeMailbox()
	svc := newTestService(t, mb, store, nil)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Two letters arrive while Rosa is active: HandleNotify keeps both.
	mb.seed(inboxFolder, rawSeedMessage("rosa@example.com", "<letter-1@x>"))
	mb.seed(inboxFolder, rawSeedMessage("rosa@example.com", "<letter-2@x>"))
	if err := svc.HandleNotify(ctx); err != nil {
		t.Fatalf("HandleNotify (while active): %v", err)
	}
	kept := mb.uidsIn(inboxFolder)
	if len(kept) != 2 {
		t.Fatalf("INBOX after two active-contact letters = %v, want 2 messages", kept)
	}

	mustDeactivate(t, store, rosa.ID)

	// A reconciliation pass (as run at the top of every sync) must leave
	// Rosa's already-delivered letters exactly where they are.
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after deactivation: %v", err)
	}
	if got := mb.uidsIn(inboxFolder); len(got) != 2 {
		t.Fatalf("INBOX after Reconcile post-deactivation = %v, want the original 2 letters still present (history must be immutable, §7.2)", got)
	}
	if got := mb.uidsIn("Held"); len(got) != 0 {
		t.Fatalf("Held after Reconcile post-deactivation = %v, want empty — reconciliation must never quarantine a deactivated contact's old mail", got)
	}

	// A third, genuinely new letter from her IS quarantined — but by
	// HandleNotify's active-only check, not by Reconcile.
	mb.seed(inboxFolder, rawSeedMessage("rosa@example.com", "<letter-3@x>"))
	if err := svc.HandleNotify(ctx); err != nil {
		t.Fatalf("HandleNotify (after deactivation): %v", err)
	}
	if got := mb.uidsIn(inboxFolder); len(got) != 2 {
		t.Fatalf("INBOX after third letter = %v, want still just the original 2 (third must be Held)", got)
	}
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Fatalf("Held after third letter = %v, want exactly the third letter", got)
	}
}

// TestF2_ReconcileNeverCallsResolveActive is the direct regression guard the
// task description asks for: it fails loudly, by call count rather than by
// behaviour, the moment Reconcile is "simplified" to share a resolution path
// with HandleNotify.
func TestF2_ReconcileNeverCallsResolveActive(t *testing.T) {
	store := openTestAyllu(t)
	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")
	mustDeactivate(t, store, rosa.ID)
	spy := &resolveSpy{Store: store}

	mb := newFakeMailbox()
	mb.seed(inboxFolder,
		rawSeedMessage("rosa@example.com", "<a@x>"),
		rawSeedMessage("stranger@example.com", "<b@x>"),
	)

	svc := newTestService(t, mb, spy, nil)
	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	resolveCalls, resolveActiveCalls := spy.counts()
	if resolveActiveCalls != 0 {
		t.Errorf("Reconcile called ResolveActive %d times, want 0 — reconciliation must resolve against the FULL table only (F-2)", resolveActiveCalls)
	}
	if resolveCalls == 0 {
		t.Errorf("Reconcile never called Resolve — test fixture is not exercising the resolution path it means to guard")
	}
}

// TestF2_HandleNotifyNeverCallsResolve is the mirror image: arrival filing
// must decide exclusively via ResolveActive.
func TestF2_HandleNotifyNeverCallsResolve(t *testing.T) {
	store := openTestAyllu(t)
	mustAddContact(t, store, "Rosa", "rosa@example.com")
	spy := &resolveSpy{Store: store}

	mb := newFakeMailbox()
	mb.seed(inboxFolder,
		rawSeedMessage("rosa@example.com", "<a@x>"),
		rawSeedMessage("stranger@example.com", "<b@x>"),
	)

	svc := newTestService(t, mb, spy, nil)
	if err := svc.HandleNotify(context.Background()); err != nil {
		t.Fatalf("HandleNotify: %v", err)
	}

	resolveCalls, resolveActiveCalls := spy.counts()
	if resolveCalls != 0 {
		t.Errorf("HandleNotify called Resolve %d times, want 0 — arrival filing must resolve against ACTIVE contacts only (F-2)", resolveCalls)
	}
	if resolveActiveCalls == 0 {
		t.Errorf("HandleNotify never called ResolveActive — test fixture is not exercising the resolution path it means to guard")
	}
}

func TestHandleNotify_MultipleBatches(t *testing.T) {
	old := arrivalBatchSize
	arrivalBatchSize = 2
	t.Cleanup(func() { arrivalBatchSize = old })

	store := openTestAyllu(t)
	mustAddContact(t, store, "Rosa", "rosa@example.com")

	mb := newFakeMailbox()
	for i := 0; i < 5; i++ {
		mb.seed(inboxFolder, rawSeedMessage("rosa@example.com", ""))
	}

	svc := newTestService(t, mb, store, nil)
	if err := svc.HandleNotify(context.Background()); err != nil {
		t.Fatalf("HandleNotify: %v", err)
	}
	if got := mb.uidsIn(inboxFolder); len(got) != 5 {
		t.Errorf("INBOX = %v, want all 5 messages kept across multiple batches", got)
	}
}

func TestCheckSpam_MovesEverythingToHeldUnconditionally(t *testing.T) {
	store := openTestAyllu(t)
	mustAddContact(t, store, "Rosa", "rosa@example.com")

	mb := newFakeMailbox()
	// Even mail from a real, active contact gets swept: the backstop does
	// not consult the ayllu at all (§5.1).
	mb.seed("Junk",
		rawSeedMessage("rosa@example.com", "<spam-1@x>"),
		rawSeedMessage("stranger@example.com", "<spam-2@x>"),
	)

	svc := newTestService(t, mb, store, nil)
	moved, err := svc.CheckSpam(context.Background())
	if err != nil {
		t.Fatalf("CheckSpam: %v", err)
	}
	if moved != 2 {
		t.Errorf("CheckSpam moved %d messages, want 2", moved)
	}
	if got := mb.uidsIn("Junk"); len(got) != 0 {
		t.Errorf("Junk = %v, want empty", got)
	}
	if got := mb.uidsIn("Held"); len(got) != 2 {
		t.Errorf("Held = %v, want both spam messages", got)
	}
}

func TestRunSpamBackstop_TicksPeriodically(t *testing.T) {
	store := openTestAyllu(t)
	mb := newFakeMailbox()

	svc := NewService(Config{
		Mailbox:      mb,
		Ayllu:        store,
		HeldFolder:   "Held",
		SpamFolder:   "Junk",
		SpamInterval: 5 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		svc.RunSpamBackstop(ctx)
		close(done)
	}()

	// Seed spam mid-run so we know a tick found and moved it, not just that
	// the loop started.
	time.Sleep(10 * time.Millisecond)
	mb.seed("Junk", rawSeedMessage("x@example.com", "<late@x>"))

	<-done
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Errorf("Held after periodic backstop = %v, want the late-seeded message swept in", got)
	}
}

// TestF9_ReconcileSyncHoldsRemovedContactUndeliveredMail is the core of the
// fix: a removed contact's letter that the child has not yet received (UID
// above the device's delivery cursor) is quarantined by the per-sync pass,
// before that same sync could deliver it as history. This is the gap where a
// contact removed for a reason reaches the child by sending during a brief
// outage the IDLE path never saw.
func TestF9_ReconcileSyncHoldsRemovedContactUndeliveredMail(t *testing.T) {
	mb := newFakeMailbox()
	store := openTestAyllu(t)
	svc := newTestService(t, mb, store, nil)

	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")
	mustDeactivate(t, store, rosa.ID)

	// Her new letter arrives at UID 5; the device's cursor is 3, so this is
	// mail she sent after removal that the child has not received.
	uids := mb.seed(inboxFolder, mailbox.Raw{UID: 5, Data: rawMessage("rosa@example.com", "<new@x>", "s", "b")})

	if err := svc.ReconcileSync(context.Background(), 3); err != nil {
		t.Fatalf("ReconcileSync: %v", err)
	}
	if containsUID(mb.uidsIn(inboxFolder), uids[0]) {
		t.Error("a removed contact's undelivered mail was left in INBOX; it would be delivered as history (F-9)")
	}
	// IMAP MOVE reassigns the UID, so the message is in Held under a new one;
	// assert by count, as the arrival tests do.
	if got := mb.uidsIn("Held"); len(got) != 1 {
		t.Errorf("Held = %v, want the removed contact's one quarantined letter", got)
	}
}

// TestF9_ReconcileSyncLeavesDeliveredHistory is the guardrail: a removed
// contact's ALREADY-delivered history (at or below the delivery cursor) must
// never be swept into Held — that is the §7.2 / V-6 promise the fix must not
// break.
func TestF9_ReconcileSyncLeavesDeliveredHistory(t *testing.T) {
	mb := newFakeMailbox()
	store := openTestAyllu(t)
	svc := newTestService(t, mb, store, nil)

	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")
	mustDeactivate(t, store, rosa.ID)

	// Old letters at UID 1 and 2, already delivered (cursor is 3).
	old := mb.seed(inboxFolder,
		mailbox.Raw{UID: 1, Data: rawMessage("rosa@example.com", "<o1@x>", "s", "b")},
		mailbox.Raw{UID: 2, Data: rawMessage("rosa@example.com", "<o2@x>", "s", "b")},
	)

	if err := svc.ReconcileSync(context.Background(), 3); err != nil {
		t.Fatalf("ReconcileSync: %v", err)
	}
	for _, uid := range old {
		if !containsUID(mb.uidsIn(inboxFolder), uid) {
			t.Errorf("delivered history uid %d was swept into Held (§7.2, V-6)", uid)
		}
	}
}

// TestF9_ReconcileSyncLeavesActiveContactMail confirms the hold is scoped to
// *inactive* contacts: an active contact's new mail above the cursor is normal
// undelivered mail and must be delivered, not held.
func TestF9_ReconcileSyncLeavesActiveContactMail(t *testing.T) {
	mb := newFakeMailbox()
	store := openTestAyllu(t)
	svc := newTestService(t, mb, store, nil)

	mustAddContact(t, store, "Rosa", "rosa@example.com") // stays active
	uids := mb.seed(inboxFolder, mailbox.Raw{UID: 5, Data: rawMessage("rosa@example.com", "<a@x>", "s", "b")})

	if err := svc.ReconcileSync(context.Background(), 3); err != nil {
		t.Fatalf("ReconcileSync: %v", err)
	}
	if !containsUID(mb.uidsIn(inboxFolder), uids[0]) {
		t.Error("an active contact's undelivered mail was quarantined; only removed contacts are held")
	}
}

// TestF9_PlainReconcileNeverHoldsInactive guards the window-resync fallback: a
// device with no trustworthy cursor (empty or UIDVALIDITY-stale) takes the
// strangers-only pass, so a removed contact's mail is NOT held on that path —
// holding "everything above zero" would sweep history on a factory reset.
func TestF9_PlainReconcileNeverHoldsInactive(t *testing.T) {
	mb := newFakeMailbox()
	store := openTestAyllu(t)
	svc := newTestService(t, mb, store, nil)

	rosa := mustAddContact(t, store, "Rosa", "rosa@example.com")
	mustDeactivate(t, store, rosa.ID)
	uids := mb.seed(inboxFolder, mailbox.Raw{UID: 5, Data: rawMessage("rosa@example.com", "<n@x>", "s", "b")})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !containsUID(mb.uidsIn(inboxFolder), uids[0]) {
		t.Error("plain Reconcile quarantined an inactive contact's mail; only ReconcileSync may (F-9 window-resync fallback)")
	}
}
