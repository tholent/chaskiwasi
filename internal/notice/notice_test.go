package notice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
	"github.com/tholent/chaskiwasi/internal/state"
)

// --- fakes -----------------------------------------------------------------

// fakeMailbox is an in-memory Mailbox: just INBOX, keyed by UID. It mimics
// exactly the two calls this package makes and nothing else, unlike
// internal/filing's richer fixture, because notice never MOVEs or FETCHes.
type fakeMailbox struct {
	mu          sync.Mutex
	inbox       []mailbox.Raw
	nextUID     uint32
	appendErr   error
	listErr     error
	appendCalls int
	listCalls   int
}

func newFakeMailbox() *fakeMailbox { return &fakeMailbox{nextUID: 1} }

func (f *fakeMailbox) Append(ctx context.Context, folder string, msg []byte, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls++
	if f.appendErr != nil {
		return f.appendErr
	}
	if folder != inboxFolder {
		return errors.New("fakeMailbox: unexpected folder " + folder)
	}
	f.inbox = append(f.inbox, mailbox.Raw{UID: f.nextUID, Data: append([]byte(nil), msg...), InternalDate: at})
	f.nextUID++
	return nil
}

func (f *fakeMailbox) List(ctx context.Context, folder string) ([]mailbox.Raw, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if folder != inboxFolder {
		return nil, nil
	}
	return append([]mailbox.Raw(nil), f.inbox...), nil
}

func (f *fakeMailbox) messages() []mailbox.Raw {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mailbox.Raw(nil), f.inbox...)
}

// fakeSubmitter is an in-memory Submitter for the §7.5 guardian-copy path.
type fakeSubmitter struct {
	mu   sync.Mutex
	sent []sentMessage
	err  error
}

type sentMessage struct {
	from string
	to   []string
	data []byte
}

func (f *fakeSubmitter) Send(ctx context.Context, from string, to []string, msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentMessage{from: from, to: append([]string(nil), to...), data: append([]byte(nil), msg...)})
	return nil
}

func (f *fakeSubmitter) messages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

// --- test helpers ------------------------------------------------------------

const testMailboxAddr = "rosa-device@example.test"

func newTestService(t *testing.T, mb Mailbox, sub Submitter, copyAddrs []string) (*Service, *state.FileStore) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	svc, err := New(Config{
		State:          st,
		Mailbox:        mb,
		Submitter:      sub,
		MailboxAddress: testMailboxAddr,
		CopyAddresses:  copyAddrs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, st
}

// decodeMessage parses raw RFC 5322 bytes back into (subject, body) the way
// a human reading the mailbox would see them: RFC 2047-decoded subject,
// quoted-printable-decoded body. Assertions run against this, never against
// the raw wire bytes, so a passing test means the actual rendered letter is
// clean, not just its still-encoded form.
func decodeMessage(t *testing.T, raw []byte) (subject, from, body string) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing generated message: %v", err)
	}
	dec := new(mime.WordDecoder)
	subj, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		subj = msg.Header.Get("Subject")
	}
	bodyBytes, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatalf("decoding quoted-printable body: %v", err)
	}
	return subj, msg.Header.Get("From"), string(bodyBytes)
}

// forbiddenVocabulary is V-14's boundary, applied here because a notice
// letter is squarely inside "any outgoing-mail rendering path."
var forbiddenVocabulary = []string{"pututu", "ayllu", "kipu"}

func assertCleanNoticeText(t *testing.T, label, subject, body string) {
	t.Helper()
	combined := strings.ToLower(subject + " " + body)
	for _, word := range forbiddenVocabulary {
		if strings.Contains(combined, word) {
			t.Errorf("%s: notice text contains forbidden vocabulary word %q\nsubject=%q\nbody=%q", label, word, subject, body)
		}
	}
	if strings.Contains(subject+body, "@") {
		t.Errorf("%s: notice text contains %q, looks like it leaked an address\nsubject=%q\nbody=%q", label, "@", subject, body)
	}
}

// --- V-7 ---------------------------------------------------------------------

// TestV7_AddRemoveRepoint_ThreeNoticeLettersFromCSys drives the exact V-7
// shape: add, remove, and re-point (readdress) a contact produce three
// notice letters in INBOX, each from c_sys, each free of any address and of
// the three internal-vocabulary words. Events carry real addresses (as a
// real ayllu.Store.Mutate call would hand back) specifically so the test can
// prove those addresses never survive into the rendered letter.
func TestV7_AddRemoveRepoint_ThreeNoticeLettersFromCSys(t *testing.T) {
	mb := newFakeMailbox()
	svc, _ := newTestService(t, mb, nil, nil)
	ctx := context.Background()

	events := []ayllu.Event{
		{At: time.Now(), Actor: "dad", Action: ayllu.ActionAdd, ContactID: "c_01", Name: "Rosa", NewAddress: "rosa@aunt.example"},
		{At: time.Now(), Actor: "mom", Action: ayllu.ActionDeactivate, ContactID: "c_01", Name: "Rosa"},
		{At: time.Now(), Actor: "dad", Action: ayllu.ActionReaddress, ContactID: "c_02", Name: "Grandma", OldAddress: "grandma@old.example", NewAddress: "grandma@new.example"},
	}
	for _, ev := range events {
		if err := svc.Announce(ctx, ev); err != nil {
			t.Fatalf("Announce(%s): %v", ev.Action, err)
		}
	}

	msgs := mb.messages()
	if len(msgs) != 3 {
		t.Fatalf("len(INBOX) = %d, want 3", len(msgs))
	}

	wantSystemFrom := ayllu.SystemName + " <" + ayllu.SystemAddress + ">"
	leakedAddresses := []string{"rosa@aunt.example", "grandma@old.example", "grandma@new.example"}

	for i, raw := range msgs {
		subj, from, body := decodeMessage(t, raw.Data)
		if from != wantSystemFrom {
			t.Errorf("message %d: From = %q, want %q (must come from c_sys, §7.4)", i, from, wantSystemFrom)
		}
		assertCleanNoticeText(t, "message "+string(rune('0'+i)), subj, body)
		for _, addr := range leakedAddresses {
			if strings.Contains(subj+body, addr) {
				t.Errorf("message %d: notice text contains real address %q (I-2 violation)", i, addr)
			}
		}
	}

	// Content check, not just shape: each letter says what happened and
	// names the person.
	if _, _, body := decodeMessage(t, msgs[0].Data); !strings.Contains(body, "Rosa") {
		t.Errorf("add notice does not mention the contact's name: %q", body)
	}
	if _, _, body := decodeMessage(t, msgs[1].Data); !strings.Contains(body, "removed") {
		t.Errorf("deactivate notice does not read as a removal: %q", body)
	}
	if _, _, body := decodeMessage(t, msgs[2].Data); !strings.Contains(body, "updated") {
		t.Errorf("readdress notice does not read as an update: %q", body)
	}
}

// TestV7_SystemContactIsUnwritable backs V-7's "c_sys unwritable" clause at
// the ayllu boundary this package relies on: Announce trusts that any letter
// really from ayllu.SystemAddress genuinely came from the system precisely
// because ayllu.Store refuses to let anything else claim that identity.
func TestV7_SystemContactIsUnwritable(t *testing.T) {
	store, err := ayllu.Open(t.TempDir(), 24)
	if err != nil {
		t.Fatalf("ayllu.Open: %v", err)
	}
	_, err = store.Mutate("dad", ayllu.Mutation{
		Action:    ayllu.ActionDeactivate,
		ContactID: protocol.SysContactID,
	})
	if !errors.Is(err, ayllu.ErrSystemContact) {
		t.Fatalf("Mutate(c_sys) error = %v, want ErrSystemContact", err)
	}
}

// --- crash ordering (§7.6, V-17) ---------------------------------------------

// TestV17_CrashBetweenAppendAndRemoval_NoticeSentExactlyOnce simulates the
// exact window the package doc names: the APPEND reached the mailbox, but
// the process died before pending_notices was cleared. A fresh Service
// backed by the same on-disk state.json and the same mailbox contents (as a
// real restart would see) must Flush to exactly one letter in INBOX, not
// two.
func TestV17_CrashBetweenAppendAndRemoval_NoticeSentExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	mb := newFakeMailbox()
	ctx := context.Background()

	// --- "before the crash" ---
	before, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	svcBefore, err := New(Config{State: before, Mailbox: mb, MailboxAddress: testMailboxAddr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pn := state.PendingNotice{ID: "deadbeef", At: time.Now(), Action: string(ayllu.ActionDeactivate), ContactID: "c_07", Name: "Rosa", Actor: "dad"}
	if err := svcBefore.addPending(pn); err != nil {
		t.Fatalf("addPending: %v", err)
	}
	if _, err := svcBefore.appendLetter(ctx, pn); err != nil {
		t.Fatalf("appendLetter: %v", err)
	}
	// Deliberately do NOT call removePending: this is the crash.
	if got := len(mb.messages()); got != 1 {
		t.Fatalf("before crash: len(INBOX) = %d, want 1", got)
	}

	// --- "after the restart": fresh Service, state re-loaded from disk,
	// same mailbox (as a real IMAP server would still hold the appended
	// letter across a Wasi restart).
	after, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open (reload): %v", err)
	}
	if got := len(after.Snapshot().PendingNotices); got != 1 {
		t.Fatalf("reloaded state has %d pending notices, want 1 (the un-cleared one)", got)
	}
	svcAfter, err := New(Config{State: after, Mailbox: mb, MailboxAddress: testMailboxAddr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := svcAfter.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := mb.appendCalls; got != 1 {
		t.Fatalf("Append was called %d times across the whole scenario, want 1 (no duplicate append on flush)", got)
	}
	if got := len(mb.messages()); got != 1 {
		t.Fatalf("after flush: len(INBOX) = %d, want 1 (still no duplicate)", got)
	}
	if got := len(after.Snapshot().PendingNotices); got != 0 {
		t.Fatalf("after flush: %d pending notices remain, want 0", got)
	}
}

// TestFlush_NothingPending_NeverListsInbox keeps the hot path (Announce)
// honest: Flush must not pay a List(INBOX) scan when there is nothing to
// recover, since that call runs unconditionally at every startup.
func TestFlush_NothingPending_NeverListsInbox(t *testing.T) {
	mb := newFakeMailbox()
	svc, _ := newTestService(t, mb, nil, nil)

	if err := svc.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if mb.listCalls != 0 {
		t.Errorf("List called %d times for an empty pending_notices, want 0", mb.listCalls)
	}
}

// TestFlush_PendingNeverAppended_AppendsAndClears covers the other half of
// §7.6's crash window: the process died between recording pending_notices
// and the APPEND ever reaching the mailbox. Flush must still deliver it.
func TestFlush_PendingNeverAppended_AppendsAndClears(t *testing.T) {
	dir := t.TempDir()
	mb := newFakeMailbox()

	st, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	pn := state.PendingNotice{ID: "abc123", At: time.Now(), Action: string(ayllu.ActionAdd), ContactID: "c_09", Name: "Uncle Theo"}
	if err := st.Update(func(s *state.State) error { s.AddPendingNotice(pn); return nil }); err != nil {
		t.Fatalf("seeding pending notice: %v", err)
	}

	svc, err := New(Config{State: st, Mailbox: mb, MailboxAddress: testMailboxAddr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := len(mb.messages()); got != 1 {
		t.Fatalf("len(INBOX) after flush = %d, want 1", got)
	}
	if got := len(st.Snapshot().PendingNotices); got != 0 {
		t.Fatalf("pending notices after flush = %d, want 0", got)
	}
}

// TestFlush_ContinuesPastOneFailure asserts §7.6's "late, never silent" for
// the multi-pending case: one bad entry must not block the others.
func TestFlush_ContinuesPastOneFailure(t *testing.T) {
	dir := t.TempDir()
	mb := newFakeMailbox()

	st, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	good := state.PendingNotice{ID: "good1", Action: string(ayllu.ActionAdd), ContactID: "c_01", Name: "Rosa"}
	bad := state.PendingNotice{ID: "bad1", Action: "some_future_action", ContactID: "c_02", Name: "Mystery"}
	if err := st.Update(func(s *state.State) error {
		s.AddPendingNotice(bad)
		s.AddPendingNotice(good)
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	svc, err := New(Config{State: st, Mailbox: mb, MailboxAddress: testMailboxAddr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = svc.Flush(context.Background())
	if err == nil {
		t.Fatal("Flush: want an error reporting the unrenderable action, got nil")
	}

	if got := len(mb.messages()); got != 1 {
		t.Fatalf("len(INBOX) = %d, want 1 (the good notice, despite the bad one failing)", got)
	}
	remaining := st.Snapshot().PendingNotices
	if len(remaining) != 1 || remaining[0].ID != "bad1" {
		t.Fatalf("remaining pending notices = %+v, want only bad1 still pending", remaining)
	}
}

// --- cosmetic changes: no notice, no log ------------------------------------

func TestAnnounce_CosmeticChangeIsNoOp(t *testing.T) {
	mb := newFakeMailbox()
	svc, st := newTestService(t, mb, nil, nil)

	err := svc.Announce(context.Background(), ayllu.Event{
		Action: ayllu.ActionCosmetic, ContactID: "c_01", Name: "Rosa",
	})
	if err != nil {
		t.Fatalf("Announce(cosmetic): %v", err)
	}
	if mb.appendCalls != 0 {
		t.Errorf("Append called %d times for a cosmetic change, want 0", mb.appendCalls)
	}
	if got := len(st.Snapshot().PendingNotices); got != 0 {
		t.Errorf("pending notices after a cosmetic change = %d, want 0", got)
	}
}

// --- the §7.5 guardian SMTP copy exception ----------------------------------

func TestGuardianCopy_OffUnlessConfigured(t *testing.T) {
	tests := []struct {
		name string
		sub  Submitter
		cfg  []string
	}{
		{"no submitter, no addresses", nil, nil},
		{"submitter but no addresses configured", &fakeSubmitter{}, nil},
		{"addresses configured but no submitter wired", nil, []string{"guardian@example.test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mb := newFakeMailbox()
			svc, _ := newTestService(t, mb, tt.sub, tt.cfg)
			if err := svc.Announce(context.Background(), ayllu.Event{Action: ayllu.ActionAdd, ContactID: "c_01", Name: "Rosa"}); err != nil {
				t.Fatalf("Announce: %v", err)
			}
			if fs, ok := tt.sub.(*fakeSubmitter); ok {
				if got := len(fs.messages()); got != 0 {
					t.Errorf("guardian copy sent %d times, want 0 (not fully configured)", got)
				}
			}
		})
	}
}

// TestGuardianCopy_SentWhenConfigured checks the bound the §7.5 exception is
// built to: fixed guardian addresses, system-generated text, and — because
// it carries the same rendered notice — still no address and no vocabulary
// leak.
func TestGuardianCopy_SentWhenConfigured(t *testing.T) {
	mb := newFakeMailbox()
	sub := &fakeSubmitter{}
	copyAddrs := []string{"mom@example.test", "dad@example.test"}
	svc, _ := newTestService(t, mb, sub, copyAddrs)

	ev := ayllu.Event{Action: ayllu.ActionReaddress, ContactID: "c_02", Name: "Grandma", Actor: "dad", OldAddress: "old@example.test", NewAddress: "new@example.test"}
	if err := svc.Announce(context.Background(), ev); err != nil {
		t.Fatalf("Announce: %v", err)
	}

	sent := sub.messages()
	if len(sent) != 1 {
		t.Fatalf("guardian copies sent = %d, want 1", len(sent))
	}
	got := sent[0]
	if got.from != testMailboxAddr {
		t.Errorf("guardian copy From (envelope) = %q, want %q", got.from, testMailboxAddr)
	}
	if len(got.to) != 2 || got.to[0] != copyAddrs[0] || got.to[1] != copyAddrs[1] {
		t.Errorf("guardian copy To (envelope) = %v, want %v", got.to, copyAddrs)
	}
	subj, _, body := decodeMessage(t, got.data)
	assertCleanNoticeText(t, "guardian copy", subj, body)
	if strings.Contains(subj+body, "old@example.test") || strings.Contains(subj+body, "new@example.test") {
		t.Error("guardian copy carries a real address — the §7.5 exception must stay system-generated text only")
	}
}

// TestGuardianCopy_SendFailureDoesNotFailAnnounce: the INBOX letter is the
// mandatory channel I-4 depends on; the SMTP copy is optional (§7.5) and its
// failure must not retroactively fail an already-delivered notice.
func TestGuardianCopy_SendFailureDoesNotFailAnnounce(t *testing.T) {
	mb := newFakeMailbox()
	sub := &fakeSubmitter{err: errors.New("smtp: connection refused")}
	svc, st := newTestService(t, mb, sub, []string{"mom@example.test"})

	err := svc.Announce(context.Background(), ayllu.Event{Action: ayllu.ActionAdd, ContactID: "c_01", Name: "Rosa"})
	if err != nil {
		t.Fatalf("Announce returned an error because the OPTIONAL guardian copy failed: %v", err)
	}
	if got := len(mb.messages()); got != 1 {
		t.Fatalf("len(INBOX) = %d, want 1 (the mandatory letter must still have gone out)", got)
	}
	if got := len(st.Snapshot().PendingNotices); got != 0 {
		t.Fatalf("pending notices = %d, want 0 (crash ordering must complete regardless of the copy)", got)
	}
}

// --- §12.3 certificate-expiry alarm -----------------------------------------

// TestCertExpiryCopy_NeverTouchesInbox is the clause that makes §12.3's
// alarm different from every other notice this package sends: it must never
// become a device-visible letter, only ever the optional guardian copy.
func TestCertExpiryCopy_NeverTouchesInbox(t *testing.T) {
	mb := newFakeMailbox()
	sub := &fakeSubmitter{}
	svc, _ := newTestService(t, mb, sub, []string{"mom@example.test"})

	if err := svc.CertExpiryCopy(context.Background(), 12); err != nil {
		t.Fatalf("CertExpiryCopy: %v", err)
	}
	if mb.appendCalls != 0 {
		t.Errorf("Append called %d times for a cert-expiry alarm, want 0 (§12.3: not an INBOX notice)", mb.appendCalls)
	}
	sent := sub.messages()
	if len(sent) != 1 {
		t.Fatalf("guardian copies sent = %d, want 1", len(sent))
	}
	subj, _, body := decodeMessage(t, sent[0].data)
	assertCleanNoticeText(t, "cert-expiry copy", subj, body)
	if !strings.Contains(body, "12") {
		t.Errorf("cert-expiry body does not mention the day count: %q", body)
	}
}

func TestCertExpiryCopy_OffUnlessConfigured(t *testing.T) {
	mb := newFakeMailbox()
	svc, _ := newTestService(t, mb, nil, nil)
	if err := svc.CertExpiryCopy(context.Background(), 12); err != nil {
		t.Fatalf("CertExpiryCopy: %v", err)
	}
	if mb.appendCalls != 0 {
		t.Errorf("Append called %d times, want 0", mb.appendCalls)
	}
}

// --- wording table (text.go) -------------------------------------------------

// TestNoticeText_AllActionsHaveCleanWording is table-driven over every
// action noticeText knows about, checking each in isolation: names the
// person, no vocabulary leak, no "@". This is the "reviewable as a set"
// check the package doc promises for text.go.
func TestNoticeText_AllActionsHaveCleanWording(t *testing.T) {
	tests := []struct {
		action        ayllu.Action
		wantSubstring string // something the wording must say, proving it's not boilerplate
	}{
		{ayllu.ActionAdd, "added"},
		{ayllu.ActionDeactivate, "removed"},
		{ayllu.ActionReactivate, "back"},
		{ayllu.ActionReaddress, "updated"},
	}
	for _, tt := range tests {
		t.Run(string(tt.action), func(t *testing.T) {
			pn := state.PendingNotice{Action: string(tt.action), Name: "Rosa", Actor: "dad"}
			body, err := bodyFor(pn)
			if err != nil {
				t.Fatalf("bodyFor: %v", err)
			}
			subj := subjectFor(pn)
			assertCleanNoticeText(t, string(tt.action), subj, body)
			if !strings.Contains(body, "Rosa") {
				t.Errorf("%s: body does not name the contact: %q", tt.action, body)
			}
			if !strings.Contains(body, tt.wantSubstring) {
				t.Errorf("%s: body = %q, want it to contain %q", tt.action, body, tt.wantSubstring)
			}
		})
	}
}

// TestNoticeText_ReaddressWithoutActor covers the fallback wording when no
// actor is on record — Announce never actually produces this today (ayllu
// always records an actor), but bodyFor must degrade gracefully rather than
// print an empty "by ." clause.
func TestNoticeText_ReaddressWithoutActor(t *testing.T) {
	pn := state.PendingNotice{Action: string(ayllu.ActionReaddress), Name: "Rosa", Actor: ""}
	body, err := bodyFor(pn)
	if err != nil {
		t.Fatalf("bodyFor: %v", err)
	}
	if strings.Contains(body, "by ") {
		t.Errorf("body = %q, want no dangling %q clause when Actor is empty", body, "by ")
	}
	if !strings.Contains(body, "Rosa") {
		t.Errorf("body = %q, want it to still name the contact", body)
	}
}

func TestBodyFor_UnknownActionErrors(t *testing.T) {
	_, err := bodyFor(state.PendingNotice{Action: "not_a_real_action", Name: "Rosa"})
	if err == nil {
		t.Fatal("bodyFor(unknown action): want an error, got nil")
	}
}

// --- Config validation --------------------------------------------------------

func TestNew_RequiresDependencies(t *testing.T) {
	base := Config{State: mustState(t), Mailbox: newFakeMailbox(), MailboxAddress: testMailboxAddr}

	t.Run("missing state", func(t *testing.T) {
		cfg := base
		cfg.State = nil
		if _, err := New(cfg); err == nil {
			t.Fatal("New: want an error for a nil State, got nil")
		}
	})
	t.Run("missing mailbox", func(t *testing.T) {
		cfg := base
		cfg.Mailbox = nil
		if _, err := New(cfg); err == nil {
			t.Fatal("New: want an error for a nil Mailbox, got nil")
		}
	})
	t.Run("missing mailbox address", func(t *testing.T) {
		cfg := base
		cfg.MailboxAddress = ""
		if _, err := New(cfg); err == nil {
			t.Fatal("New: want an error for an empty MailboxAddress, got nil")
		}
	})
}

func mustState(t *testing.T) state.Store {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return st
}

// TestF5_ChangeWithNoPendingRecordIsAnnouncedLate covers the crash window
// Flush structurally cannot see: §7.6 makes ayllu.toml durable before it
// records anything in pending_notices, so a crash in between leaves a changed
// contact list and no trace that a notice was ever owed. Flush finds an empty
// pending_notices and correctly does nothing; without Reconcile the notice is
// lost forever, which is exactly the I-4 failure ("neither party can alter the
// list behind the other's back") that this whole package exists to prevent.
func TestF5_ChangeWithNoPendingRecordIsAnnouncedLate(t *testing.T) {
	mb := newFakeMailbox()
	svc, _ := newTestService(t, mb, nil, nil)

	// The change is durable in the log, but the process died before Announce
	// recorded anything — so state.json holds nothing at all.
	ev := ayllu.Event{
		At:        time.Now().UTC(),
		Actor:     "dad",
		Action:    ayllu.ActionDeactivate,
		ContactID: "c_07",
		Name:      "Rosa",
		Version:   8,
	}

	if err := svc.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(mb.messages()); got != 0 {
		t.Fatalf("Flush announced %d letters from an empty pending list, want 0", got)
	}

	if err := svc.Reconcile(context.Background(), []ayllu.Event{ev}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	msgs := mb.messages()
	if len(msgs) != 1 {
		t.Fatalf("after Reconcile: %d letters in INBOX, want 1 — the change was never announced", len(msgs))
	}
	if body := string(msgs[0].Data); !strings.Contains(body, "Rosa") {
		t.Errorf("late notice does not name the contact: %s", body)
	}

	// Reconciling again must not produce a second letter: the id is derived
	// from the event, so the INBOX check is exact rather than approximate.
	if err := svc.Reconcile(context.Background(), []ayllu.Event{ev}); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := len(mb.messages()); got != 1 {
		t.Fatalf("second Reconcile produced %d letters, want 1 — duplicate announcement", got)
	}
}

// TestF5_ReconcileSkipsAlreadyAnnouncedChanges is the common case: a normal
// run announced everything, so a startup reconciliation over the same log must
// be silent. If this fails, every restart spams the child's inbox with notices
// for changes they were already told about.
func TestF5_ReconcileSkipsAlreadyAnnouncedChanges(t *testing.T) {
	mb := newFakeMailbox()
	svc, _ := newTestService(t, mb, nil, nil)

	events := []ayllu.Event{
		{At: time.Now().UTC(), Actor: "dad", Action: ayllu.ActionAdd, ContactID: "c_01", Name: "Theo", Version: 1},
		{At: time.Now().UTC(), Actor: "dad", Action: ayllu.ActionReaddress, ContactID: "c_01", Name: "Theo", Version: 2},
	}
	for _, ev := range events {
		if err := svc.Announce(context.Background(), ev); err != nil {
			t.Fatalf("Announce: %v", err)
		}
	}
	before := len(mb.messages())

	if err := svc.Reconcile(context.Background(), events); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := len(mb.messages()); got != before {
		t.Fatalf("Reconcile re-announced: %d letters, want %d", got, before)
	}
}
