package mailbox

import "errors"

// ErrUnreachable reports that the mailbox could not be reached — dial
// failure, TLS failure, or a connection that died mid-command. It is
// distinguished from an ordinary IMAP/SMTP protocol error (bad login, no
// such mailbox) so the sync handler can return 503 + Retry-After rather than
// a hard failure (wasi-server-plan §4.1). Callers should use errors.Is,
// since the concrete error is always wrapped with more context.
var ErrUnreachable = errors.New("mailbox: server unreachable")

// unreachable wraps errs (any of which may be nil) together with
// ErrUnreachable so errors.Is(result, ErrUnreachable) is true.
func unreachable(errs ...error) error {
	present := make([]error, 0, len(errs)+1)
	for _, err := range errs {
		if err != nil {
			present = append(present, err)
		}
	}
	present = append(present, ErrUnreachable)
	return errors.Join(present...)
}
