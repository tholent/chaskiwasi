package notice

import (
	"fmt"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/state"
)

// This file is the single reviewable set of notice wording (§7.4). A child
// reads these, so they live in one place, in plain English, on purpose —
// nowhere else in this package should assemble notice text piecemeal.
//
// Two rules shape every entry:
//
//   - No address, ever (I-2). Guardians who need the actual old/new address
//     find it in ayllu-log.jsonl, surfaced by the web UI — never here.
//   - No pronoun. ayllu.Contact carries no gender, so every sentence repeats
//     the person's name instead of guessing "his"/"her"/"their". This departs
//     from the spec's illustrative examples ("her old letters") in form only,
//     not in weight or meaning — see the notice.go package doc for the
//     reasoning.
//
// Address change is worded with the same weight as add and remove (§7.4):
// it is a full sentence about the person, not a footnote.

// noticeText maps an ayllu.Action to the sentence a device-visible notice
// letter carries for it. Deliberately NOT a map[ayllu.Action]string: the
// readdress wording depends on whether an actor was recorded, so each entry
// is a function of the pending notice.
//
// ayllu.ActionCosmetic has no entry on purpose: the youth's cosmetic overlay
// (nickname, order, pinned, portrait) is explicitly "no notice, no log" in
// ayllu's own package doc, and Service.Announce short-circuits before ever
// consulting this table for it.
var noticeText = map[ayllu.Action]func(state.PendingNotice) string{
	ayllu.ActionAdd: func(pn state.PendingNotice) string {
		return fmt.Sprintf("%s was added to your list. You can send letters to %s now.", pn.Name, pn.Name)
	},
	ayllu.ActionDeactivate: func(pn state.PendingNotice) string {
		return fmt.Sprintf("%s was removed from your list. You can still read %s's old letters, but you can't send %s new ones.", pn.Name, pn.Name, pn.Name)
	},
	ayllu.ActionReactivate: func(pn state.PendingNotice) string {
		return fmt.Sprintf("%s was added back to your list. You can send letters to %s again.", pn.Name, pn.Name)
	},
	ayllu.ActionReaddress: func(pn state.PendingNotice) string {
		if pn.Actor == "" {
			return fmt.Sprintf("%s's address was updated.", pn.Name)
		}
		return fmt.Sprintf("%s's address was updated by %s.", pn.Name, pn.Actor)
	},
}

// bodyFor renders the notice body for pn, or an error if pn.Action has no
// entry in noticeText — which should only happen for a persisted
// pending_notices row this version of the code does not know how to word
// (e.g. a downgrade after a future action type shipped forward).
func bodyFor(pn state.PendingNotice) (string, error) {
	fn, ok := noticeText[ayllu.Action(pn.Action)]
	if !ok {
		return "", fmt.Errorf("notice: no notice text for action %q", pn.Action)
	}
	return fn(pn), nil
}

// subjectFor is the Subject header for pn's notice letter (§7.4). Kept
// generic and short on purpose: the body carries the actual news, and a
// generic subject line means the device's list view doesn't spoil "removed"
// vs "added" before the letter is opened.
func subjectFor(pn state.PendingNotice) string {
	return fmt.Sprintf("About %s", pn.Name)
}

// certExpirySubject and certExpiryBody are the §12.3 guardian-copy wording.
// This never becomes an INBOX notice letter (see CertExpiryCopy's doc
// comment), so it does not go through noticeText, but it lives here for the
// same reason: it is outgoing mail and belongs in the one reviewable place.
const certExpirySubject = "Wasi certificate renewal needed"

func certExpiryBody(daysRemaining int) string {
	return fmt.Sprintf(
		"The device's certificate has %d day(s) left before it expires. "+
			"Renew it before it lapses, or the device will lose its connection to the server.",
		daysRemaining,
	)
}
