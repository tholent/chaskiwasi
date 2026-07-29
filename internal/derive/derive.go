// Package derive turns a raw mailbox message into the device's view of a letter,
// at read time, persisting nothing (invariant I-1, wasi-server-plan §5.2).
//
// The pipeline, in order:
//
//  1. FETCH -> enmime parse -> text/plain part only. HTML is never rendered and
//     remote resources are never fetched — free, because nothing renders them.
//     Attachments are ignored in v1.
//  2. Resolve From against the full ayllu, tombstones included (§7.2). After
//     reconciliation everything left in INBOX resolves.
//  3. POST /strip, honouring format=flowed soft breaks (§11.1); on failure use
//     the in-process fallback and flag the letter degraded (§5.3).
//  4. Normalise the subject (§5.4).
//  5. Truncate the body at max_letter_chars graphemes, setting Truncated if cut.
//     The full text stays untouched in the mailbox and graduates intact.
//
// Derivation is deterministic: the same UIDs under the same config yield
// byte-identical output, which is what makes replays and window resyncs safe
// (test V-9). Determinism is scoped to unchanged config — a changed
// max_letter_chars legitimately changes body bytes on the next resync, and the
// device's dedup by letter id rather than body bytes is what makes that a
// non-event (§5.2).
package derive

import (
	"context"

	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// Deriver renders one raw message as a wire letter.
type Deriver interface {
	Derive(ctx context.Context, r mailbox.Raw) (protocol.Letter, error)
}
