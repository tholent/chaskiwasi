package filing

import (
	"context"
	"fmt"

	"github.com/tholent/chaskiwasi/internal/ayllu"
)

// This file implements the release mechanism of §8, not the guardian UI that
// drives it: wave 3 builds the Held-review pages on top of these two
// functions. "Release requires the sender to resolve to an active contact,"
// so there are exactly two flows, matching the two ways a Held sender can
// become active:
//
//   - ReleaseStranger: "add as contact, then release."
//   - ReleaseDeactivated: "reactivate, then release."
//
// Both functions call ayllu.Store.Mutate but do not themselves send the
// resulting notice letter (§7.4) — this package has no internal/notice
// dependency, by design; the returned ayllu.Event is what a caller feeds
// into the notice pipeline, exactly as any other Mutate call site would.
// What both functions DO guarantee directly, because it is specific to
// release and V-18 tests it: nothing vanishes. If the ayllu mutation fails,
// the Held message is untouched. If the move to INBOX fails after a
// successful mutation, the contact change stands (nothing to roll back —
// Mutate is already durable) and the message is still sitting in Held,
// available for a retried release; reconciliation never touches Held, so it
// cannot be swept away in the meantime.

// ReleaseStranger implements "add as contact, then release" (§8) for a Held
// message whose sender is not in the ayllu at all. It adds them as a new
// contact (subject to max_contacts) at the address read from the message
// itself — never accepted as a caller-supplied parameter, so a release can
// never be used to add a contact at an address that didn't actually send
// this letter — then MOVEs the message from Held to INBOX.
//
// name is the guardian-supplied display name; actor is the guardian username
// recorded in the ayllu change log (same meaning as ayllu.Store.Mutate's
// actor). The returned Event is the add event for the caller's notice
// pipeline.
//
// The released message is routed through the same arrival path as any new
// mail (see handleNotifyLocked below), so it rings the doorbell exactly the
// way §8 requires: "a released letter is an arriving letter from the
// child's point of view."
func (s *Service) ReleaseStranger(ctx context.Context, uid uint32, actor, name string) (ayllu.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.findInHeldLocked(ctx, uid)
	if err != nil {
		return ayllu.Event{}, err
	}
	h := parseHeaders(raw.Data)
	if !h.fromOK {
		return ayllu.Event{}, fmt.Errorf("filing: release uid %d: no parseable sender address", uid)
	}

	event, err := s.ayllu.Mutate(actor, ayllu.Mutation{
		Action:  ayllu.ActionAdd,
		Name:    name,
		Address: h.from,
	})
	if err != nil {
		return ayllu.Event{}, fmt.Errorf("filing: release uid %d: adding contact: %w", uid, err)
	}

	if err := s.mailbox.Move(ctx, s.heldFolder, uid, inboxFolder); err != nil {
		return event, fmt.Errorf("filing: release uid %d: moving to INBOX: %w", uid, err)
	}
	s.log.Info("filing: released stranger as new contact",
		"uid", uid, "contact_id", event.ContactID, "letter_id", h.letterID)

	return event, s.arrivalPassAfterReleaseLocked(ctx, uid)
}

// ReleaseDeactivated implements "reactivate, then release" (§8) for a Held
// message whose sender resolves to an existing tombstone. contactID must
// match what the message's sender actually resolves to against the full
// table (tombstones included) — this function refuses to reactivate a
// different contact than the one that sent this specific letter, even if a
// caller passes a mismatched id by mistake, which is what keeps a release
// from ever being used to reopen the wrong channel.
//
// actor is the guardian username recorded in the ayllu change log. The
// returned Event is the reactivation event for the caller's notice pipeline.
//
// A guardian who wants to deliver one old letter without reopening the
// channel reactivates, releases, and deactivates again — three deliberate,
// announced actions (§8). This function does not chain them; that sequencing
// is the guardian UI's job, one call to this function and one separate call
// to ayllu.Store.Mutate(ActionDeactivate) afterward.
func (s *Service) ReleaseDeactivated(ctx context.Context, uid uint32, actor, contactID string) (ayllu.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.findInHeldLocked(ctx, uid)
	if err != nil {
		return ayllu.Event{}, err
	}
	h := parseHeaders(raw.Data)
	if !h.fromOK {
		return ayllu.Event{}, fmt.Errorf("filing: release uid %d: no parseable sender address", uid)
	}
	resolved, ok := s.ayllu.Resolve(h.from)
	if !ok || resolved.ID != contactID {
		return ayllu.Event{}, fmt.Errorf("filing: release uid %d: sender does not resolve to contact %s", uid, contactID)
	}

	event, err := s.ayllu.Mutate(actor, ayllu.Mutation{
		Action:    ayllu.ActionReactivate,
		ContactID: contactID,
	})
	if err != nil {
		return ayllu.Event{}, fmt.Errorf("filing: release uid %d: reactivating %s: %w", uid, contactID, err)
	}

	if err := s.mailbox.Move(ctx, s.heldFolder, uid, inboxFolder); err != nil {
		return event, fmt.Errorf("filing: release uid %d: moving to INBOX: %w", uid, err)
	}
	s.log.Info("filing: released reactivated contact",
		"uid", uid, "contact_id", contactID, "letter_id", h.letterID)

	return event, s.arrivalPassAfterReleaseLocked(ctx, uid)
}

// arrivalPassAfterReleaseLocked runs the ordinary arrival scan immediately
// after a release moves a message into INBOX. Reusing handleNotifyLocked
// here — rather than open-coding "ring the doorbell" in each release
// function — is deliberate: the released message is, at this point,
// indistinguishable from any other brand-new INBOX arrival from an active
// contact (its sender was just confirmed to resolve active by the mutation
// above), so it should be decided by the exact same code that decides every
// other arrival, not a second copy of that logic that could drift from it.
// Any other truly-new mail that arrived concurrently gets swept up too,
// which is a harmless bonus, not a side effect release depends on.
func (s *Service) arrivalPassAfterReleaseLocked(ctx context.Context, releasedUID uint32) error {
	if err := s.handleNotifyLocked(ctx); err != nil {
		return fmt.Errorf("filing: release uid %d: arrival pass after move: %w", releasedUID, err)
	}
	return nil
}

// ReleaseActive releases a Held message whose sender ALREADY resolves to an
// active contact. It moves the message and rings the doorbell; it mutates the
// contact list not at all, and therefore announces nothing.
//
// §8 names only two release flows — stranger and deactivated — but a third
// state is reachable and ordinary: a guardian adds a stranger from the contacts
// page, and that person's earlier letter is still sitting in Held with a sender
// that now resolves as active. Routing it through ReleaseDeactivated would
// satisfy the mechanics and produce a *false* notice letter — "Rosa was added
// back to your list" when nothing about Rosa changed. A notice that describes a
// change which did not happen is worse than a missing one: I-4 exists so the
// child and the guardians can trust that the list only changes when a letter
// says so, and a spurious letter corrupts exactly that record.
//
// The §8 precondition still holds and is still checked: the sender must resolve
// to an active contact. Here it simply already does, so there is nothing to
// mutate on the way.
func (s *Service) ReleaseActive(ctx context.Context, uid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.findInHeldLocked(ctx, uid)
	if err != nil {
		return err
	}
	h := parseHeaders(raw.Data)
	if !h.fromOK {
		return fmt.Errorf("filing: release uid %d: no parseable sender address", uid)
	}
	resolved, ok := s.ayllu.ResolveActive(h.from)
	if !ok {
		return fmt.Errorf("filing: release uid %d: sender does not resolve to an active contact", uid)
	}

	if err := s.mailbox.Move(ctx, s.heldFolder, uid, inboxFolder); err != nil {
		return fmt.Errorf("filing: release uid %d: moving to INBOX: %w", uid, err)
	}
	s.log.Info("filing: released message from an already-active contact",
		"uid", uid, "contact_id", resolved.ID, "letter_id", h.letterID)

	return s.arrivalPassAfterReleaseLocked(ctx, uid)
}
