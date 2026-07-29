package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/subject"
)

// heldKind classifies a Held message by what its sender resolves to, which is
// what decides the shape of the release control (§8): "release requires the
// sender to resolve to an active contact", and there are exactly as many
// release flows as there are ways for a sender to become one.
type heldKind string

const (
	// heldStranger — not in the contact list at all. Release is "add as
	// contact, then release" (§8).
	heldStranger heldKind = "stranger"
	// heldDeactivated — a tombstone. Release is "reactivate, then release" (§8).
	heldDeactivated heldKind = "deactivated"
	// heldKnown — already an active contact. §8 does not name this case, but
	// it is reachable: a guardian who adds a stranger on the Contacts page
	// leaves that stranger's earlier letter sitting in Held with a
	// now-resolving sender. Release here mutates nothing and announces
	// nothing; see releaseFor.
	heldKnown heldKind = "known"
	// heldUnreadable — the From header will not parse, so nothing can resolve
	// it and no release flow applies. Shown rather than hidden: a message this
	// UI cannot act on must still be visible, because the alternative is mail
	// that silently exists and can never leave.
	heldUnreadable heldKind = "unreadable"
)

// heldView is one row of the Held review.
//
// It carries a sender address — this is the decision point for "add as
// contact", and a review that hides who the mail is from is not a review — and
// a subject line, because the guardian already holds the canonical mailbox and
// judging a message with neither is guesswork. It never carries a body, and
// nothing on it is logged (I-1) or crosses the device boundary (I-2).
type heldView struct {
	UID         uint32
	From        string
	Subject     string
	Received    time.Time
	Kind        heldKind
	ContactID   string
	ContactName string
}

type heldPage struct {
	layout
	List heldList
}

type heldList struct {
	Messages []heldView
	// Err is set when the mailbox could not be read. The Held view is read
	// live over IMAP with no mirror (§8), so an unreachable mailbox is an
	// empty page — it must say so rather than imply there is nothing held.
	Err string
	// CSRF is repeated on the fragment because htmx swaps it into a page whose
	// forms need one and the fragment is rendered standalone.
	CSRF string
}

func (s *Server) handleHeld(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	page := heldPage{
		layout: s.newLayout(r, sess, "Held Messages", "held"),
	}
	page.List = s.heldList(r.Context(), page.CSRF)
	s.page(w, http.StatusOK, "held.html", page)
}

// handleHeldFragment serves the htmx poll that keeps the list fresh without a
// page reload.
func (s *Server) handleHeldFragment(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	s.fragment(w, "held_list.html", s.heldList(r.Context(), s.issueCSRF(sess)))
}

// heldList reads the Held folder live over IMAP (§8). There is deliberately no
// cache and no mirror: releasing acts on the canonical article, and a stale
// list would let a guardian release a message that is no longer there.
func (s *Server) heldList(ctx context.Context, csrf string) heldList {
	out := heldList{CSRF: csrf}
	if s.mailbox == nil {
		out.Err = "The mailbox is not configured."
		return out
	}

	msgs, err := s.mailbox.List(ctx, s.heldFolder())
	if err != nil {
		s.log.Warn("web: reading held messages failed", "error", err)
		out.Err = "The mailbox could not be reached. This list is not complete."
		return out
	}

	out.Messages = make([]heldView, 0, len(msgs))
	for _, raw := range msgs {
		out.Messages = append(out.Messages, s.classify(raw))
	}
	// Newest first: a guardian reviewing Held is almost always looking at what
	// just arrived.
	sort.SliceStable(out.Messages, func(i, j int) bool {
		return out.Messages[i].Received.After(out.Messages[j].Received)
	})
	return out
}

// classify resolves one Held message against the FULL contact table
// (tombstones and past addresses included, §7.2) to decide which release flow
// it needs. Resolve rather than ResolveActive: the whole question here is
// whether the sender is a stranger or a tombstone, and ResolveActive cannot
// tell those apart.
func (s *Server) classify(raw mailbox.Raw) heldView {
	v := heldView{UID: raw.UID, Received: raw.InternalDate, Kind: heldUnreadable}

	msg, err := mail.ReadMessage(bytes.NewReader(raw.Data))
	if err != nil {
		return v
	}
	if v.Received.IsZero() {
		if date, err := msg.Header.Date(); err == nil {
			v.Received = date
		}
	}
	v.Subject = subject.NormalizeInbound(msg.Header.Get("Subject"))

	addrs, err := msg.Header.AddressList("From")
	if err != nil || len(addrs) == 0 {
		return v
	}
	v.From = addrs[0].Address

	c, ok := s.ayllu.Resolve(v.From)
	switch {
	case !ok:
		v.Kind = heldStranger
		// A sensible default for the "add as contact" name field, taken from
		// the display name the sender chose. The guardian can overwrite it;
		// what it must never do is silently become the contact's identity, so
		// it is offered as a form value and not applied on its own.
		v.ContactName = strings.TrimSpace(addrs[0].Name)
	case !c.Active:
		v.Kind, v.ContactID, v.ContactName = heldDeactivated, c.ID, c.Name
	default:
		v.Kind, v.ContactID, v.ContactName = heldKnown, c.ID, c.Name
	}
	return v
}

// handleHeldRelease performs a release (§8, V-18).
//
// The flow is chosen from a fresh server-side resolution, never from the form:
// a posted "this one is a stranger" would otherwise be able to drive the
// add-a-contact path for a message whose sender is really a tombstone. filing
// re-checks the same facts against the live article — ReleaseStranger takes the
// address from the message itself, ReleaseDeactivated refuses a contact id the
// sender does not resolve to — so this is the outer of two independent checks,
// not the only one.
func (s *Server) handleHeldRelease(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	if s.releaser == nil || s.mailbox == nil {
		s.redirect(w, r, "/held", flashReleaseFailed)
		return
	}

	uid64, err := strconv.ParseUint(r.PostFormValue("uid"), 10, 32)
	if err != nil {
		s.redirect(w, r, "/held", flashReleaseFailed)
		return
	}
	uid := uint32(uid64)

	view, err := s.findHeld(r.Context(), uid)
	if err != nil {
		s.log.Warn("web: release could not locate the message", "uid", uid, "error", err)
		s.redirect(w, r, "/held", flashReleaseFailed)
		return
	}

	event, err := s.releaseFor(r.Context(), view, sess.Guardian, strings.TrimSpace(r.PostFormValue("name")))
	if err != nil {
		// Nothing vanishes on a failed release: filing leaves the message in
		// Held, and reconciliation never touches Held, so it is still there to
		// try again (§8, V-18).
		s.log.Error("web: release failed", "uid", uid, "kind", view.Kind, "error", err)
		s.redirect(w, r, "/held", flashReleaseFailed)
		return
	}

	s.log.Info("web: released a held message",
		"uid", uid, "kind", view.Kind, "contact_id", event.ContactID, "guardian", sess.Guardian)

	// A zero Event means nothing about the contact list changed — the
	// already-active case (see releaseFor). Announcing it would state a change
	// that did not happen, which is worse than staying quiet: I-4's promise is
	// that the list changed exactly when a letter says so.
	if event.Action != "" && !s.announce(r.Context(), event) {
		s.redirect(w, r, "/held", flashNoticeLate)
		return
	}
	s.redirect(w, r, "/held", flashReleased)
}

// releaseFor dispatches to the one filing function that matches the sender's
// classification. This package must never reimplement release: filing owns
// both the ordering guarantee (the contact change is durable before the MOVE,
// and a failed MOVE leaves the message retrievable) and the re-verification
// against the live article.
//
// The heldKnown case is the one §8 does not name, and it is reached routinely:
// a guardian adds a stranger from the contacts page, and that person's earlier
// letter is still in Held with a sender that now resolves as active. It gets
// filing.ReleaseActive, which moves the message and rings the doorbell while
// mutating nothing, and returns a zero Event so the caller announces nothing.
// Routing it through ReleaseDeactivated instead would work mechanically and
// put a false statement into the child's inbox — "added back to your list" for
// a change that never happened.
func (s *Server) releaseFor(ctx context.Context, v heldView, actor, name string) (ayllu.Event, error) {
	switch v.Kind {
	case heldStranger:
		if name == "" {
			return ayllu.Event{}, fmt.Errorf("web: release uid %d: a name is required to add this sender as a contact", v.UID)
		}
		return s.releaser.ReleaseStranger(ctx, v.UID, actor, name)
	case heldDeactivated:
		return s.releaser.ReleaseDeactivated(ctx, v.UID, actor, v.ContactID)
	case heldKnown:
		// Already active: release without touching the contact list. Sending
		// this through ReleaseDeactivated would work mechanically and lie in
		// the child's inbox ("added back to your list" for a change that never
		// happened), so it gets its own path and returns no event.
		return ayllu.Event{}, s.releaser.ReleaseActive(ctx, v.UID)
	default:
		return ayllu.Event{}, fmt.Errorf("web: release uid %d: the sender of this message cannot be read, so it cannot be released", v.UID)
	}
}

// findHeld re-reads the Held folder and classifies the one message being
// acted on, so the release decision is made against the mailbox as it is now
// rather than as it was when the page was rendered.
func (s *Server) findHeld(ctx context.Context, uid uint32) (heldView, error) {
	msgs, err := s.mailbox.List(ctx, s.heldFolder())
	if err != nil {
		return heldView{}, fmt.Errorf("web: listing held messages: %w", err)
	}
	for _, raw := range msgs {
		if raw.UID == uid {
			return s.classify(raw), nil
		}
	}
	return heldView{}, fmt.Errorf("web: no held message with uid %d", uid)
}
