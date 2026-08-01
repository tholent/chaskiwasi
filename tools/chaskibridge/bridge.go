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
// suite would stop meaning what it claims. Concretely, it never: reads or
// rewrites a request or response body, invents or repairs an Authorization
// header, retries, or turns a status code into anything but that status code.
//
// Framing (host side of client §14): each direction is a length-prefixed frame
// over USB-CDC, with a magic and a CRC so a receiver can resynchronise. See
// frame.go for the layout and firmware/chaski/components/transport for the
// device half.
//
// Logging is content-free at every level (I-1, D-7, C-19). The bridge sits
// directly on the letter path — every letter the child writes passes through
// this process — so it logs frame types, byte counts, and status codes, and
// never a body, a subject, or the bearer token.
package chaskibridge

import (
	"io"
	"log/slog"
	"time"
)

// Framing constants, shared with the firmware's usbbridge transport. Any
// change here is a change on both sides of the cable.
const (
	// FrameMagic starts every frame, so a resync after line noise is possible
	// without guessing.
	FrameMagic uint32 = 0x43484B31 // "CHK1"

	// MaxFrameBytes bounds a frame. The server caps a request at 64 KB
	// (server §4.1); the bridge is no more permissive than the real endpoint.
	// The bound is on the payload: a frame occupies at most MaxFrameBytes +
	// FrameOverheadBytes on the wire.
	MaxFrameBytes = 64 << 10

	// DefaultTimeout is generous: a bench sync crossing the compose stack is
	// slower than LTE, and a timeout here would look like a device bug.
	DefaultTimeout = 30 * time.Second

	// DefaultBaudRate is a formality. USB-CDC ignores line rate; the value
	// exists because the serial API requires one.
	DefaultBaudRate = 115200
)

// contentType is fixed by server §4.1 for both directions.
//
// It is the one header the bridge supplies rather than forwards, because on
// this path the bridge is the HTTP speaker — the device's frame carries a body
// and a bearer, not a request line. The Authorization header is the device's
// and crosses untouched; everything else about the exchange is the protocol's
// constant, not a decision made here.
const contentType = "application/json; charset=utf-8"

// Config is how a bench run points the bridge at a server.
type Config struct {
	// SerialPort is the device's USB-CDC device node.
	SerialPort string

	// BaudRate is passed to the serial API; USB-CDC does not use it. Zero
	// means DefaultBaudRate.
	BaudRate int

	// WasiURL is the sync endpoint, normally the compose stack's.
	WasiURL string

	// CAFile is a PEM bundle to verify Wasi with. The compose stack mints its
	// own CA, so pointing at it is how a bench run gets a *verified* TLS
	// connection rather than a skipped one — and leaving both this and
	// InsecureSkipVerify unset is how C-7's trust-failure case is produced
	// deliberately instead of by accident.
	CAFile string

	// InsecureSkipVerify allows the bench stack's self-signed certificate.
	// It applies ONLY to the bridge's own connection to Wasi on a developer
	// machine; it says nothing about the device, whose TLS is terminated in
	// the modem against two pinned private CAs and is never relaxed (D-6).
	InsecureSkipVerify bool

	Timeout time.Duration

	// Console receives the bytes on the link that are not frames — which, on
	// a dev build, is the device's serial log, since the console shares the
	// USB-CDC peripheral. The bench captures it as C-19's evidence. Nil
	// discards it.
	Console io.Writer

	// OnEvent receives FrameEvent payloads from the device: the bench control
	// channel's return path. Nil ignores them. It is called on the Serve
	// goroutine, so handlers must not block on anything Serve drives.
	OnEvent func(payload []byte)

	// Logger is where the content-free diagnostics go. Nil uses the default.
	Logger *slog.Logger
}

// Stats are content-free by construction: counts and byte totals, never
// bodies. The bridge sits on the letter path, so it observes I-1/D-7 as
// strictly as the firmware does.
type Stats struct {
	Frames    int
	Requests  int
	Responses int
	Commands  int
	Events    int
	BytesIn   int
	BytesOut  int

	// Resyncs counts frame-boundary recoveries: console text between frames, a
	// device reset mid-frame, or line noise. A number, never the bytes.
	Resyncs int

	// TransportFails and TLSTrustFails count what the bridge could not deliver
	// to Wasi, split the way §5.3 splits it.
	TransportFails int
	TLSTrustFails  int
}
