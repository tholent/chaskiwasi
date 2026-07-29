package filing

import "context"

// Doorbell is the pututu hook (§10.1). Filing calls Ring whenever an event
// should trigger the SMS doorbell: new mail resolving to an active contact
// (§5.1), and a release from Held (§8) — "a released letter is an arriving
// letter from the child's point of view."
//
// This package deliberately does not implement coalescing, retry, or the
// carrier call itself: §10.1's coalescing window, the signed counter token
// (§10.2), and the actual SMS send belong to internal/pututu and
// internal/carrier, which land in a later wave. Ring is only ever the raw
// "something arrived" signal; it carries no payload, matching the doorbell's
// own wire contract of carrying no information beyond "sync now" (§10.2) —
// what to do with repeated or rapid signals is entirely the receiver's
// decision.
type Doorbell interface {
	Ring(ctx context.Context)
}

// DoorbellFunc adapts a plain function to Doorbell.
type DoorbellFunc func(ctx context.Context)

// Ring implements Doorbell.
func (f DoorbellFunc) Ring(ctx context.Context) { f(ctx) }

// NopDoorbell rings nowhere. It is the default when Config.Doorbell is nil,
// and it is what every caller gets until wave 3 wires internal/pututu in.
var NopDoorbell Doorbell = DoorbellFunc(func(context.Context) {})
