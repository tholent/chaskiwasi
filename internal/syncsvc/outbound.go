package syncsvc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"strings"
	"time"

	"github.com/tholent/chaskiwasi/internal/config"
	"github.com/tholent/chaskiwasi/internal/graphemes"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
	"github.com/tholent/chaskiwasi/internal/state"
	"github.com/tholent/chaskiwasi/internal/subject"
)

// maxLocalIDBytes is the wire cap on a device-assigned local_id (§4.2). It is
// enforced rather than trusted because every accepted id is written into the
// ack ring in state.json: an unbounded id would be an unbounded state file
// authored by whoever holds the bearer token.
const maxLocalIDBytes = 32

// outboundItem is one decoded outbound letter plus whatever went wrong while
// decoding it. A decode problem is carried rather than raised so the letter can
// be acked "invalid" — terminal, so the device stops resending it (§4.7) —
// instead of wedging the whole outbox behind one bad entry.
type outboundItem struct {
	letter protocol.Outbound
	// unknownFields reports a field this server does not know. §4.7 step 2
	// counts that as a validation failure for that letter alone.
	unknownFields bool
}

// decodeOutbound decodes one outbound element. It decodes twice on purpose:
// leniently to recover local_id, which is the only way to ack the letter at
// all, and then strictly to detect unknown fields (§4.7 step 2).
//
// An element that is not even a JSON object, or that carries no local_id, is
// the one outbound failure this function refuses to absorb: with no local_id
// there is no ack channel, so silently dropping it would leave the letter on
// the device forever with nothing in the logs to explain it. The caller turns
// that into a request-level error instead.
func decodeOutbound(raw json.RawMessage) (outboundItem, error) {
	var lenient protocol.Outbound
	if err := json.Unmarshal(raw, &lenient); err != nil {
		return outboundItem{}, fmt.Errorf("syncsvc: outbound letter does not decode: %w", err)
	}
	if lenient.LocalID == "" {
		return outboundItem{}, fmt.Errorf("syncsvc: outbound letter has no local_id")
	}

	item := outboundItem{letter: lenient}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var strict protocol.Outbound
	if err := dec.Decode(&strict); err != nil {
		item.unknownFields = true
	}
	return item, nil
}

// processOutbound runs §4.7 for every letter in the request and returns the
// acks to ship.
//
// The ack ring is consulted first, before any work: a replayed local_id gets
// back the SAME terminal ack — including a replayed rejection — and is never
// reprocessed (§4.7, test V-9).
func (h *Handler) processOutbound(ctx context.Context, cfg *config.Config, items []outboundItem, now time.Time) ([]protocol.Ack, error) {
	if len(items) == 0 {
		return nil, nil
	}

	acks := make([]protocol.Ack, 0, len(items))
	ring := h.deps.State.Snapshot().Acks

	for _, item := range items {
		localID := item.letter.LocalID

		if entry, replayed := ring.Lookup(localID); replayed {
			acks = append(acks, protocol.Ack{LocalID: localID, Status: protocol.AckStatus(entry.Status)})
			continue
		}

		status, err := h.sendOne(ctx, cfg, item, now)
		if err != nil {
			// SMTP failed. There is no wire status for that — every ack is
			// terminal and makes the device drop the letter (§4.7) — so the
			// letter is deliberately left unacked: it stays in the device's
			// outbox, visible to the child as "still on the road", and is
			// re-sent next sync. That is the failure §4.7 chooses, since a
			// letter the kid watched leave that never arrives is the one
			// outcome this system will not buy.
			h.deps.Logger.Error("sync: outbound send failed, leaving letter unacked",
				"local_id", localID, "error", err)
			if errors.Is(err, mailbox.ErrUnreachable) {
				// A required upstream is unreachable: stop here and let the
				// device retry the identical request (§4.1). Acks already
				// recorded are durable and replay from the ring.
				return nil, unreachableError(err)
			}
			continue
		}

		// Record (local_id, status), fsync, THEN ack (§4.7 step 5).
		//
		// Do not reorder this against the SMTP send above to close the
		// duplicate-send window. A crash between the two costs a duplicate
		// send on replay, which is the correct failure: a relative seeing a
		// letter twice is recoverable, a letter that silently never arrives is
		// not. Recording the ack first would convert every crash-in-flight
		// into exactly that loss (§4.7, test V-9).
		if err := h.recordAck(localID, status, now); err != nil {
			// Durability failed, so the ack is not owed to the device yet.
			// Same reasoning as an SMTP failure: no ack, the letter is resent,
			// and the send is repeated. A duplicate, not a loss.
			h.deps.Logger.Error("sync: persisting ack failed, leaving letter unacked",
				"local_id", localID, "status", status, "error", err)
			continue
		}
		ring.Record(localID, string(status), now)
		acks = append(acks, protocol.Ack{LocalID: localID, Status: status})
		h.deps.Logger.Info("sync: outbound acked", "local_id", localID, "status", status)
	}

	return acks, nil
}

// recordAck persists one terminal ack and does not return until it is durable.
func (h *Handler) recordAck(localID string, status protocol.AckStatus, at time.Time) error {
	return h.deps.State.Update(func(s *state.State) error {
		s.Acks.Record(localID, string(status), at)
		return nil
	})
}

// sendOne runs steps 1-4 of §4.7 for one letter. A returned error means the
// SMTP submission itself failed and the letter must stay unacked; a returned
// status — including a rejection — is terminal.
func (h *Handler) sendOne(ctx context.Context, cfg *config.Config, item outboundItem, now time.Time) (protocol.AckStatus, error) {
	letter := item.letter

	// Step 1: resolve against ACTIVE contacts only (§7.2). "You can still read
	// Rosa's old letters, you just can't write to her" falls out of this.
	contact, known := h.deps.Ayllu.ByID(letter.ContactID)
	switch {
	case !known:
		return protocol.AckRejectedUnknownContact, nil
	case letter.ContactID == protocol.SysContactID:
		// c_sys always resolves and is always active, because notice letters
		// must render (§7.4) — but it cannot be written to, and it is never in
		// the device's contact list, so a letter addressed to it is a device
		// bug rather than a contact-state problem. Unknown-contact is the
		// closest terminal status the wire has.
		return protocol.AckRejectedUnknownContact, nil
	case !contact.Active:
		return protocol.AckRejectedInactive, nil
	}

	// Step 2: validate (§4.7).
	if !validOutbound(letter, item.unknownFields, cfg.Sync.MaxLetterChars) {
		return protocol.AckInvalid, nil
	}

	// Steps 3 and 4: sanitise or generate the subject, mint a Message-ID,
	// submit.
	msg, err := buildMessage(cfg, contact.Address, letter, now)
	if err != nil {
		return "", err
	}
	if err := h.deps.Submitter.Send(ctx, cfg.Mail.Address, []string{contact.Address}, msg); err != nil {
		// A permanent 5xx rejection (a dead recipient address) is terminal:
		// retrying it every sync forever would never deliver the letter and
		// would leave the child watching "on the road" for something that can
		// never land. Hand back a terminal reject so the device stops and shows
		// "couldn't send — ask your guardians" (§4.7, A.11). A transient failure
		// (4xx) or an unreachable server returns an error instead, and the
		// caller leaves the letter unacked to be retried.
		if mailbox.IsPermanentReject(err) {
			return protocol.AckRejectedUndeliverable, nil
		}
		return "", err
	}
	return protocol.AckSent, nil
}

// validOutbound implements §4.7 step 2: non-empty body, within the grapheme
// cap, known fields, and a local_id inside its wire cap.
func validOutbound(letter protocol.Outbound, unknownFields bool, maxLetterChars int) bool {
	switch {
	case unknownFields:
		return false
	case len(letter.LocalID) > maxLocalIDBytes:
		return false
	case strings.TrimSpace(letter.Body) == "":
		return false
	case graphemes.Count(letter.Body) > maxLetterChars:
		// Outbound is rejected past the cap rather than truncated: inbound
		// truncation shapes a view of a letter the mailbox still holds whole,
		// but truncating a child's own letter would send words they did not
		// choose to send (§4.6).
		return false
	}
	return true
}

// buildMessage renders one outbound letter as RFC 5322 bytes (§6.1).
//
// Message-ID and nothing else: no In-Reply-To, no References, no Re: prefix,
// no reply concept anywhere on the wire. The product is passing notes, not
// conducting email (A.1). Test V-8 fails loudly if anyone "fixes" this.
func buildMessage(cfg *config.Config, to string, letter protocol.Outbound, now time.Time) ([]byte, error) {
	messageID, err := newMessageID(cfg.Mail.Address)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	header := func(name, value string) {
		fmt.Fprintf(&buf, "%s: %s\r\n", name, value)
	}
	header("Message-ID", messageID)
	header("From", cfg.Mail.Address)
	header("To", to)
	header("Date", now.Format(time.RFC1123Z))
	// The one place child-supplied text enters an email header, sanitised and
	// RFC 2047-encoded server-side and never trusted from the device (§6.2,
	// test V-3).
	header("Subject", subject.Outbound(letter.Subject, letter.Body, cfg.Owner.Name))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	// Quoted-printable rather than 8bit: a letter with an emoji or an accent
	// must survive a submission path that never promised 8BITMIME, and
	// quoted-printable also folds the long single line an e-ink composer
	// produces without needing the transport to.
	header("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write([]byte(letter.Body)); err != nil {
		return nil, fmt.Errorf("syncsvc: encoding body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("syncsvc: encoding body: %w", err)
	}
	return buf.Bytes(), nil
}

// newMessageID mints the outbound Message-ID (§6.1). The left-hand side is 16
// random bytes: it must be globally unique, and it must not encode anything
// about the letter, the child, or the clock, since it is the one header that
// travels to a relative's mail client and back into the archive forever.
func newMessageID(fromAddress string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("syncsvc: generating message-id: %w", err)
	}

	domain := "localhost"
	if at := strings.LastIndexByte(fromAddress, '@'); at >= 0 && at+1 < len(fromAddress) {
		domain = fromAddress[at+1:]
	}
	return fmt.Sprintf("<%s@%s>", hex.EncodeToString(b[:]), domain), nil
}
