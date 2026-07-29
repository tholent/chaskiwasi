package web

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/tholent/chaskiwasi/internal/ayllu"
)

// contactView is one row of the contacts page. This is one of exactly two
// surfaces where an address is shown (the change log is the other) — and it is
// shown because this is the page on which a guardian *manages* addresses.
// Nothing rendered here is device-bound (I-2).
type contactView struct {
	ID            string
	Name          string
	Address       string
	PastAddresses []string
	Active        bool
}

type contactsPage struct {
	layout
	Contacts    []contactView
	ActiveCount int
	TotalCount  int
	MaxContacts int
	Full        bool
}

// handleContacts renders contact management (§9.1).
func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	_, contacts := s.ayllu.List()

	page := contactsPage{
		layout:      s.newLayout(r, sess, "Contacts", "contacts"),
		Contacts:    make([]contactView, 0, len(contacts)),
		MaxContacts: s.maxContacts(),
	}
	for _, c := range contacts {
		page.Contacts = append(page.Contacts, contactView{
			ID:            c.ID,
			Name:          c.Name,
			Address:       c.Address,
			PastAddresses: c.PastAddresses,
			Active:        c.Active,
		})
		if c.Active {
			page.ActiveCount++
		}
	}
	page.TotalCount = len(contacts)
	// The cap counts tombstones (A.3), which is surprising enough that the
	// page says so rather than letting a guardian discover it at the moment
	// they are trying to add someone.
	page.Full = page.TotalCount >= page.MaxContacts

	s.page(w, http.StatusOK, "contacts.html", page)
}

// handleContactAdd adds a contact. max_contacts is enforced by the store and
// surfaced here as a clear error rather than a silent no-op (A.3).
func (s *Server) handleContactAdd(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	name := strings.TrimSpace(r.PostFormValue("name"))
	address, err := parseAddress(r.PostFormValue("address"))
	if name == "" || err != nil {
		s.redirect(w, r, "/contacts", flashContactInvalid)
		return
	}

	s.mutate(w, r, "/contacts", ayllu.Mutation{
		Action:  ayllu.ActionAdd,
		Name:    name,
		Address: address,
	}, sess.Guardian, flashContactAdded)
}

// handleContactDeactivate implements §7.2's removal-is-deactivation. The page
// that posts here carries §7.3's honest limitation next to the button, so a
// guardian removing someone for a safety reason learns what this system can
// and cannot do before they click, not during an argument afterwards.
func (s *Server) handleContactDeactivate(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	s.mutate(w, r, "/contacts", ayllu.Mutation{
		Action:    ayllu.ActionDeactivate,
		ContactID: r.PostFormValue("contact_id"),
	}, sess.Guardian, flashContactDeactivated)
}

func (s *Server) handleContactReactivate(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	s.mutate(w, r, "/contacts", ayllu.Mutation{
		Action:    ayllu.ActionReactivate,
		ContactID: r.PostFormValue("contact_id"),
	}, sess.Guardian, flashContactReactivated)
}

// handleContactReaddress repoints a contact at a new address. §7.4 announces
// this with the same weight as add and remove, because silently repointing a
// contact id at a new address is precisely how this system would be turned
// into a redirection attack.
func (s *Server) handleContactReaddress(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	address, err := parseAddress(r.PostFormValue("address"))
	if err != nil {
		s.redirect(w, r, "/contacts", flashContactInvalid)
		return
	}
	s.mutate(w, r, "/contacts", ayllu.Mutation{
		Action:    ayllu.ActionReaddress,
		ContactID: r.PostFormValue("contact_id"),
		Address:   address,
	}, sess.Guardian, flashContactReaddressed)
}

// mutate is the single call site for every contact change the UI makes: one
// ayllu.Store.Mutate call, then the resulting ayllu.Event handed to the
// notice pipeline (§7.4). This package never writes ayllu.toml itself and
// never hand-rolls TOML (§9.1).
//
// The ordering is §7.6's and is not negotiable: the mutation is durable before
// the announcement is attempted, so a failure to announce degrades to "notice
// arrives a little late" and never to "change happened silently" (I-4).
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, back string, m ayllu.Mutation, actor string, ok flashCode) {
	event, err := s.ayllu.Mutate(actor, m)
	if err != nil {
		s.log.Warn("web: contact change rejected",
			"action", m.Action, "contact_id", m.ContactID, "guardian", actor, "error", err)
		s.redirect(w, r, back, mutationFlash(err))
		return
	}

	s.log.Info("web: contact changed",
		"action", event.Action, "contact_id", event.ContactID, "guardian", actor, "version", event.Version)

	if !s.announce(r.Context(), event) {
		s.redirect(w, r, back, flashNoticeLate)
		return
	}
	s.redirect(w, r, back, ok)
}

// mutationFlash maps a store error to what the guardian is told. Only the
// cases a guardian can act on are distinguished; everything else is "nothing
// was altered", which is true because Mutate rolls back its in-memory table on
// a failed write.
func mutationFlash(err error) flashCode {
	switch {
	case errors.Is(err, ayllu.ErrMaxContacts):
		return flashContactsFull
	case errors.Is(err, ayllu.ErrUnknownContact),
		errors.Is(err, ayllu.ErrSystemContact):
		return flashContactInvalid
	default:
		return flashContactFailed
	}
}

// parseAddress validates a guardian-typed address and returns it in bare
// addr-spec form. Accepting "Rosa <rosa@example.test>" and storing only the
// address keeps the display name out of the resolution key, which is the one
// thing the ayllu store compares on.
func parseAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("web: empty address")
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil {
		return "", err
	}
	return parsed.Address, nil
}
