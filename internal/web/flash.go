package web

import "net/http"

// Outcome messages travel through the POST/redirect/GET cycle as short opaque
// codes in the query string, and the text lives here on the server.
//
// A stateless-session design has nowhere to keep a conventional flash store,
// and the obvious alternative — putting the message text in the URL — would
// eventually put a contact's name or address into browser history, a bookmark,
// or a reverse-proxy access log. Codes cost one map and keep I-2's blast
// radius at zero.
type flashCode string

const (
	flashSignedOut          flashCode = "signed-out"
	flashContactAdded       flashCode = "contact-added"
	flashContactDeactivated flashCode = "contact-off"
	flashContactReactivated flashCode = "contact-on"
	flashContactReaddressed flashCode = "contact-address"
	flashContactsFull       flashCode = "contacts-full"
	flashContactInvalid     flashCode = "contact-invalid"
	flashContactFailed      flashCode = "contact-failed"
	flashNoticeLate         flashCode = "notice-late"
	flashReleased           flashCode = "released"
	flashReleaseFailed      flashCode = "release-failed"
	flashPasswordChanged    flashCode = "password-changed"
	flashPasswordWrong      flashCode = "password-wrong"
	flashPasswordWeak       flashCode = "password-weak"
	flashPasswordMismatch   flashCode = "password-mismatch"
	flashGuardianAdded      flashCode = "guardian-added"
	flashGuardianExists     flashCode = "guardian-exists"
	flashGuardianInvalid    flashCode = "guardian-invalid"
)

// flash is what a page renders.
type flash struct {
	// Level is "ok", "warn", or "error" — it selects the styling and nothing
	// else.
	Level string
	Text  string
}

var flashes = map[flashCode]flash{
	flashSignedOut: {"ok", "You are signed out."},

	flashContactAdded:       {"ok", "Contact added. A letter announcing the change is on its way to the inbox."},
	flashContactDeactivated: {"ok", "Contact removed from the list. Their past letters can still be read; new ones will be held for review."},
	flashContactReactivated: {"ok", "Contact restored. They can send and receive letters again."},
	flashContactReaddressed: {"ok", "Address updated. The old address is kept so past letters still show the right name."},
	flashContactsFull:       {"error", "The contact list is full. Remove someone first — removed contacts still count, because their letters have to keep working."},
	flashContactInvalid:     {"error", "That contact could not be saved. Check the name and address and try again."},
	flashContactFailed:      {"error", "The change could not be saved. Nothing was altered."},

	flashNoticeLate: {"warn", "The change was saved, but the letter announcing it could not be delivered yet. It will be sent automatically."},

	flashReleased:      {"ok", "Message delivered to the device."},
	flashReleaseFailed: {"error", "The message could not be delivered. It is still held — nothing was lost."},

	flashPasswordChanged:  {"ok", "Password changed. Every other sign-in for that account has been ended."},
	flashPasswordWrong:    {"error", "That current password is not correct."},
	flashPasswordWeak:     {"error", "That password is too short."},
	flashPasswordMismatch: {"error", "The two new passwords did not match."},

	flashGuardianAdded:   {"ok", "Guardian account created."},
	flashGuardianExists:  {"error", "There is already an account with that name."},
	flashGuardianInvalid: {"error", "That name cannot be used. Use letters, digits, dots, dashes or underscores."},
}

// flashFor resolves the ?m= code on r, if any. An unknown code renders
// nothing: a stale or hand-typed URL should not produce a blank grey box.
func flashFor(r *http.Request) *flash {
	code := flashCode(r.URL.Query().Get("m"))
	if code == "" {
		return nil
	}
	f, ok := flashes[code]
	if !ok {
		return nil
	}
	return &f
}
