package filing

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/mailbox"
)

// rawMessage builds a minimal RFC 5322 message for tests. messageID may be
// empty to exercise the "no parseable letter id" logging path; from empty
// exercises the "no parseable sender" path.
func rawMessage(from, messageID, subject, body string) []byte {
	var b strings.Builder
	if from != "" {
		fmt.Fprintf(&b, "From: %s\r\n", from)
	}
	b.WriteString("To: kid@chaski.test\r\n")
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	if messageID != "" {
		fmt.Fprintf(&b, "Message-Id: %s\r\n", messageID)
	}
	b.WriteString("Date: Wed, 29 Jul 2026 12:00:00 +0000\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// rawSeedMessage builds a mailbox.Raw ready to hand to fakeMailbox.seed,
// with messageID left blank meaning "no Message-Id header" (exercising the
// letter-id-unavailable logging path).
func rawSeedMessage(from, messageID string) mailbox.Raw {
	return mailbox.Raw{Data: rawMessage(from, messageID, "hi", "hello")}
}

func openTestAyllu(t *testing.T) *ayllu.FileStore {
	t.Helper()
	s, err := ayllu.Open(t.TempDir(), 24)
	if err != nil {
		t.Fatalf("ayllu.Open: %v", err)
	}
	return s
}

func mustAddContact(t *testing.T, s ayllu.Store, name, addr string) ayllu.Contact {
	t.Helper()
	event, err := s.Mutate("dad", ayllu.Mutation{Action: ayllu.ActionAdd, Name: name, Address: addr})
	if err != nil {
		t.Fatalf("add %s <%s>: %v", name, addr, err)
	}
	c, ok := s.ByID(event.ContactID)
	if !ok {
		t.Fatalf("ByID(%s) after add: not found", event.ContactID)
	}
	return c
}

func mustDeactivate(t *testing.T, s ayllu.Store, contactID string) {
	t.Helper()
	if _, err := s.Mutate("dad", ayllu.Mutation{Action: ayllu.ActionDeactivate, ContactID: contactID}); err != nil {
		t.Fatalf("deactivate %s: %v", contactID, err)
	}
}

// doorbellSpy counts Ring calls, standing in for wave 3's internal/pututu.
type doorbellSpy struct {
	mu    sync.Mutex
	rings int
}

func (d *doorbellSpy) Ring(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rings++
}

func (d *doorbellSpy) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rings
}

// resolveSpy wraps a real ayllu.Store and counts calls to Resolve versus
// ResolveActive. It exists for exactly one purpose: TestF2_* below use it to
// assert, by call count rather than just by outcome, that Reconcile never
// calls ResolveActive and HandleNotify never calls Resolve. A refactor that
// "simplifies" the two paths into one shared resolution call would make one
// of those counts nonzero even if the surrounding behavioural tests happened
// to still pass on their particular fixtures — that is the failure mode this
// spy is here to catch loudly (F-2, specs/implementation-plan.md §4a).
type resolveSpy struct {
	ayllu.Store
	mu                 sync.Mutex
	resolveCalls       int
	resolveActiveCalls int
}

func (s *resolveSpy) Resolve(addr string) (ayllu.Contact, bool) {
	s.mu.Lock()
	s.resolveCalls++
	s.mu.Unlock()
	return s.Store.Resolve(addr)
}

func (s *resolveSpy) ResolveActive(addr string) (ayllu.Contact, bool) {
	s.mu.Lock()
	s.resolveActiveCalls++
	s.mu.Unlock()
	return s.Store.ResolveActive(addr)
}

func (s *resolveSpy) counts() (resolve, resolveActive int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveCalls, s.resolveActiveCalls
}

func newTestService(t *testing.T, mb *fakeMailbox, store ayllu.Store, doorbell Doorbell) *Service {
	t.Helper()
	if mb == nil {
		mb = newFakeMailbox()
	}
	if doorbell == nil {
		doorbell = NopDoorbell
	}
	return NewService(Config{
		Mailbox:    mb,
		Ayllu:      store,
		HeldFolder: "Held",
		SpamFolder: "Junk",
		Doorbell:   doorbell,
	})
}
