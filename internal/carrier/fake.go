package carrier

import (
	"context"
	"errors"
	"sync"
)

// errFakeTransport is the default error FailNext injects when the caller
// doesn't supply one of its own.
var errFakeTransport = errors.New("carrier: fake: simulated transport failure")

// Call records one Pututu invocation against Fake, success or failure.
type Call struct {
	Payload string
	Err     error // nil if this attempt succeeded
}

// Fake is an in-memory Carrier (§10.4): it removes any live-account
// dependency from the e2e stack by recording what would have been sent
// rather than sending it. Every Pututu attempt — successful or not — is
// appended to the call log, and failures are injectable, so a test can
// assert both "exactly one SMS went out" (V-13) and "a transport error was
// retried."
type Fake struct {
	mu sync.Mutex

	calls []Call

	failNext int
	failErr  error

	balance    Balance
	balanceErr error
}

var _ Carrier = (*Fake)(nil)

// NewFake returns a Fake with no injected failures and Balance reporting
// ErrUnsupported until SetBalance is called — an unconfigured Fake behaves
// like a provider with no balance concept, matching the "optional
// capabilities degrade" default (§10.4).
func NewFake() *Fake {
	return &Fake{balanceErr: ErrUnsupported}
}

// Name implements Carrier.
func (f *Fake) Name() string { return "fake" }

// Pututu implements Carrier. It honours context cancellation like any real
// transport would (§10.4's conformance requirement), then either returns the
// next injected failure or records the call as sent.
func (f *Fake) Pututu(ctx context.Context, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	if f.failNext > 0 {
		f.failNext--
		err = f.failErr
		if err == nil {
			err = errFakeTransport
		}
	}
	f.calls = append(f.calls, Call{Payload: payload, Err: err})
	return err
}

// Balance implements Carrier.
func (f *Fake) Balance(ctx context.Context) (Balance, error) {
	if err := ctx.Err(); err != nil {
		return Balance{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.balanceErr != nil {
		return Balance{}, f.balanceErr
	}
	return f.balance, nil
}

// FailNext arranges for the next n calls to Pututu to fail with err. A nil
// err falls back to a generic transport-shaped error. Passing n=0 clears any
// pending failures.
func (f *Fake) FailNext(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
	f.failErr = err
}

// SetBalance makes Balance report b successfully, as if this provider
// supported the capability.
func (f *Fake) SetBalance(b Balance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balance = b
	f.balanceErr = nil
}

// SetBalanceUnsupported restores the default ErrUnsupported behaviour after
// a SetBalance call.
func (f *Fake) SetBalanceUnsupported() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balance = Balance{}
	f.balanceErr = ErrUnsupported
}

// Calls returns every Pututu attempt so far, in order — the inspectable
// call log (§10.4).
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Sent returns the payloads of only the successful Pututu attempts, in
// order — what a test asserting "exactly one SMS went out" (V-13) usually
// wants instead of the full attempt log.
func (f *Fake) Sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Err == nil {
			out = append(out, c.Payload)
		}
	}
	return out
}
