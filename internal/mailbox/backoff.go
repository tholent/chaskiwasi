package mailbox

import (
	"context"
	"math/rand"
	"time"
)

// backoff is exponential-with-jitter, shared by IMAP reconnection. Jitter
// matters here specifically because a real deployment is many Wasi
// containers pointed at one Fastmail account family: without it, a
// provider-side blip would reconnect every device on the same clock tick.
type backoff struct {
	base, max time.Duration
	attempt   int
}

func newBackoff(base, max time.Duration) *backoff {
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	if max <= 0 {
		max = 10 * time.Second
	}
	return &backoff{base: base, max: max}
}

// wait blocks for the next backoff interval, or returns false immediately if
// ctx is done first. Callers use the false return to give up and report
// ErrUnreachable rather than retrying past the caller's own deadline.
func (b *backoff) wait(ctx context.Context) bool {
	d := b.base
	// Cap the shift so this can never overflow into a negative duration.
	if b.attempt < 32 {
		d = b.base << b.attempt
		b.attempt++
	}
	if d <= 0 || d > b.max {
		d = b.max
	}
	// Full jitter: a random point in [0, d], not just d +/- a bit.
	d = time.Duration(rand.Int63n(int64(d) + 1))

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
