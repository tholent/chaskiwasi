package pututu

import (
	"context"
	"math/rand"
	"time"
)

// backoff is exponential-with-jitter, matching internal/mailbox's IMAP
// reconnect backoff in shape. That implementation is unexported there, so
// this is a deliberate small duplication of a ~30-line helper rather than a
// shared package two call sites don't earn.
type backoff struct {
	base, max time.Duration
	attempt   int
}

func newBackoff(base, max time.Duration) *backoff {
	if base <= 0 {
		base = defaultRetryBase
	}
	if max <= 0 {
		max = defaultRetryMax
	}
	return &backoff{base: base, max: max}
}

// wait blocks for the next backoff interval, or returns false immediately if
// ctx is done first.
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
