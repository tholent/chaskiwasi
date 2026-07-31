package mailbox

import "errors"

// ErrUnreachable reports that the mailbox could not be reached — dial
// failure, TLS failure, or a connection that died mid-command. It is
// distinguished from an ordinary IMAP/SMTP protocol error (bad login, no
// such mailbox) so the sync handler can return 503 + Retry-After rather than
// a hard failure (wasi-server-plan §4.1). Callers should use errors.Is,
// since the concrete error is always wrapped with more context.
var ErrUnreachable = errors.New("mailbox: server unreachable")

// ErrUndeliverable reports that the server permanently refused *this message*
// — the recipient address is dead (5xx at RCPT TO) or the message itself is
// refused (5xx at DATA). No retry will ever deliver it, so §4.7 step 4 turns
// this into the terminal ack `rejected_undeliverable` and the child is told
// once, instead of watching "on the road" forever (A.11).
//
// It is deliberately narrower than "any 5xx". A 5xx at AUTH or MAIL FROM is
// the server refusing *us* — a rotated app password, a sending quota — which
// is guardian-fixable and hits every queued letter at once. Those stay plain
// errors so the letters are left unacked and retried once the config is
// repaired; see submit in smtp.go. Callers should use errors.Is, since the
// concrete error is always wrapped with more context.
var ErrUndeliverable = errors.New("mailbox: message permanently refused")

// unreachable wraps errs (any of which may be nil) together with
// ErrUnreachable so errors.Is(result, ErrUnreachable) is true.
func unreachable(errs ...error) error {
	return join(ErrUnreachable, errs...)
}

// undeliverable wraps errs together with ErrUndeliverable so
// errors.Is(result, ErrUndeliverable) is true.
func undeliverable(errs ...error) error {
	return join(ErrUndeliverable, errs...)
}

func join(sentinel error, errs ...error) error {
	present := make([]error, 0, len(errs)+1)
	for _, err := range errs {
		if err != nil {
			present = append(present, err)
		}
	}
	present = append(present, sentinel)
	return errors.Join(present...)
}
