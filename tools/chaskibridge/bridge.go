// Package chaskibridge proxies the device's dev-build sync transport to a real
// Wasi.
//
// It exists so firmware can be iterated against the actual server — the
// compose stack with maddy, running the V-table fixtures — without a SIM, a
// carrier, or a public endpoint (client §14, decision B.2).
//
// The bridge is a WIRE, not a mock. It adds nothing, interprets nothing, and
// forwards the device's bearer header untouched, so the firmware exercises
// real authentication against a real server. Anything it "helpfully" did here
// would be behaviour the production modem path does not have, and the bench
// suite would stop meaning what it claims.
//
// Framing (host side of client §14): each direction is a length-prefixed
// frame over USB-CDC. Request frames carry the sync body; response frames
// carry status plus body.
//
// Wave 0 scaffold: types and framing constants only. Wave 2A implements the
// serial loop and the HTTP forwarder (chaski-implementation-plan §4).
package chaskibridge

import "time"

// Framing constants, shared with the firmware's usbbridge transport. Any
// change here is a change on both sides of the cable.
const (
	// FrameMagic starts every frame, so a resync after line noise is possible
	// without guessing.
	FrameMagic uint32 = 0x43484B31 // "CHK1"

	// MaxFrameBytes bounds a frame. The server caps a request at 64 KB
	// (server §4.1); the bridge is no more permissive than the real endpoint.
	MaxFrameBytes = 64 << 10

	// DefaultTimeout is generous: a bench sync crossing the compose stack is
	// slower than LTE, and a timeout here would look like a device bug.
	DefaultTimeout = 30 * time.Second
)

// Config is how a bench run points the bridge at a server.
type Config struct {
	// SerialPort is the device's USB-CDC device node.
	SerialPort string

	// WasiURL is the sync endpoint, normally the compose stack's.
	WasiURL string

	// InsecureSkipVerify allows the bench stack's self-signed certificate.
	// It applies ONLY to the bridge's own connection to Wasi on a developer
	// machine; it says nothing about the device, whose TLS is terminated in
	// the modem against two pinned private CAs and is never relaxed (D-6).
	InsecureSkipVerify bool

	Timeout time.Duration
}

// Stats are content-free by construction: counts and byte totals, never
// bodies. The bridge sits on the letter path, so it observes I-1/D-7 as
// strictly as the firmware does.
type Stats struct {
	Frames    int
	Requests  int
	Responses int
	BytesIn   int
	BytesOut  int
}
