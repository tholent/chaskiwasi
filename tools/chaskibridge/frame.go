package chaskibridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// The host half of the §14 framing codec. The device half is
// firmware/chaski/components/transport/frame.{h,cpp}, and the two are held
// together by the vectors under test/firmware/host/testdata/frames/ — which
// this package generates and the C++ host tests parse. Two implementations
// that merely look alike drift silently; the same argument tools/graphvectors
// makes for graphemes (B.7).
//
// Layout, all integers big-endian:
//
//	u32 magic | u8 type | u32 length | length bytes payload | u32 crc32
//
// The CRC covers type, length, and payload — not the magic, because a receiver
// that guessed wrong about where a frame starts must not have its own guess
// confirmed by its own constant.

// Frame geometry. FrameMagic and MaxFrameBytes live in bridge.go with the rest
// of the constants shared with the firmware.
const (
	frameHeaderBytes  = 9 // magic + type + length
	frameTrailerBytes = 4 // crc32
	// FrameOverheadBytes is what a frame costs beyond its payload.
	FrameOverheadBytes = frameHeaderBytes + frameTrailerBytes
)

// FrameType says what a payload is. Unknown types are framed and delivered
// like any other, so one end can grow a frame kind without the other being
// reflashed first.
type FrameType uint8

const (
	FrameRequest  FrameType = 0x01 // device -> host: authorization + sync body
	FrameResponse FrameType = 0x02 // host -> device: outcome + status + body

	// The bench control channel: the only way a harness can make a device with
	// no keyboard compose and sync (C-1, C-2, C-4, C-7). Payloads are opaque
	// UTF-8 JSON to this codec; the vocabulary is in test/firmware/bench.
	FrameCommand FrameType = 0x03 // host -> device
	FrameEvent   FrameType = 0x04 // device -> host
)

func (t FrameType) String() string {
	switch t {
	case FrameRequest:
		return "request"
	case FrameResponse:
		return "response"
	case FrameCommand:
		return "command"
	case FrameEvent:
		return "event"
	default:
		return fmt.Sprintf("type(0x%02x)", uint8(t))
	}
}

// ErrFrameTooLarge reports a payload over the cap the server also enforces
// (server §4.1). The bridge is no more permissive than the real endpoint.
var ErrFrameTooLarge = errors.New("chaskibridge: frame payload exceeds the cap")

// EncodeFrame returns one framed message.
func EncodeFrame(t FrameType, payload []byte) ([]byte, error) {
	if len(payload) > MaxFrameBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(payload), MaxFrameBytes)
	}
	out := make([]byte, 0, FrameOverheadBytes+len(payload))
	out = binary.BigEndian.AppendUint32(out, FrameMagic)
	out = append(out, byte(t))
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:])), nil
}

// FrameReader turns a byte stream into frames, resynchronising past anything
// that is not one.
//
// Resynchronising is not a nicety here. The dev console shares the USB-CDC
// peripheral with the wire (sdkconfig.dev), so log lines land between frames;
// the device reboots without warning, which is what C-4 does on purpose; and a
// developer may attach a terminal mid-stream. Every one of those leaves the
// reader between frames with no other way back in.
type FrameReader struct {
	src io.Reader
	buf []byte

	// Console receives every byte discarded on the way to a frame boundary —
	// which, on this link, is the device's serial log. Routing it out rather
	// than dropping it is what lets a bench run capture the evidence C-19 greps
	// (D-7). Nil discards.
	//
	// One caveat the C-19 assertion has to know about: a frame that is torn on
	// the wire is not distinguishable from console text after the fact, so its
	// bytes are discarded to Console like any other non-frame run. A device ->
	// host frame carries the child's outbound letter, so a torn one puts letter
	// bytes in the very capture C-19 greps. Torn counts that case; a run with
	// Torn() > 0 makes the grep inconclusive rather than a leak. It is not fixed
	// by swallowing the failed candidate silently: the discard granularity here
	// is byte-for-byte the firmware decoder's (frame.cpp), and the shared stream
	// vectors assert both sides reach the same resync count.
	Console io.Writer

	resyncs int
	torn    int
}

// NewFrameReader reads frames from src.
func NewFrameReader(src io.Reader) *FrameReader {
	return &FrameReader{src: src, buf: make([]byte, 0, 4096)}
}

// Resyncs counts how many times bytes were discarded to find a frame boundary.
// Content-free by construction: a count, never the bytes.
func (fr *FrameReader) Resyncs() int { return fr.resyncs }

// Torn counts candidate frames that were wholly buffered and failed their CRC:
// a frame damaged in transit, or — vanishingly rarely — four bytes of console
// text that read as the magic and declared a plausible length. Either way the
// bytes end up in the Console capture, so this is the number that says whether
// that capture is pure device log. See the Console field.
func (fr *FrameReader) Torn() int { return fr.torn }

// Next returns the next whole, CRC-checked frame, blocking on src until one
// arrives. It returns the underlying read error (io.EOF included) when the
// stream ends.
func (fr *FrameReader) Next() (FrameType, []byte, error) {
	chunk := make([]byte, 4096)
	for {
		if t, payload, ok := fr.scan(); ok {
			return t, payload, nil
		}
		n, err := fr.src.Read(chunk)
		if n > 0 {
			fr.buf = append(fr.buf, chunk[:n]...)
			continue
		}
		if err != nil {
			return 0, nil, err
		}
		// A reader that returns neither bytes nor an error would spin this
		// loop hot against a serial port; say so instead.
		return 0, nil, io.ErrNoProgress
	}
}

// scan tries to pull one frame out of the buffer, discarding whatever precedes
// it. It reports ok=false when more bytes are needed.
func (fr *FrameReader) scan() (FrameType, []byte, bool) {
	for {
		start := indexMagic(fr.buf)
		if start < 0 {
			// Keep only what could still be the head of a magic.
			const partialMagic = 3
			if len(fr.buf) > partialMagic {
				fr.discard(len(fr.buf) - partialMagic)
			}
			return 0, nil, false
		}
		if start > 0 {
			fr.discard(start)
		}
		if len(fr.buf) < frameHeaderBytes {
			return 0, nil, false
		}

		// Compared as u32 before it becomes an int: on a 32-bit host a declared
		// 0xFFFFFFFF would otherwise convert to a negative length and slip
		// past the cap.
		declaredU := binary.BigEndian.Uint32(fr.buf[5:9])
		declared := int(declaredU)
		if declaredU > uint32(MaxFrameBytes) {
			// A corrupted header, or a magic that was never a frame start.
			// Drop one byte so the scan resumes inside this candidate: skipping
			// the whole header could step over a real frame beginning in it.
			fr.discard(1)
			continue
		}

		total := frameHeaderBytes + declared + frameTrailerBytes
		if len(fr.buf) < total {
			return 0, nil, false
		}
		want := binary.BigEndian.Uint32(fr.buf[frameHeaderBytes+declared : total])
		if crc32.ChecksumIEEE(fr.buf[4:frameHeaderBytes+declared]) != want {
			fr.torn++
			fr.discard(1)
			continue
		}

		t := FrameType(fr.buf[4])
		payload := make([]byte, declared)
		copy(payload, fr.buf[frameHeaderBytes:frameHeaderBytes+declared])
		fr.buf = fr.buf[total:]
		return t, payload, true
	}
}

// discard drops n leading bytes, forwarding them to Console.
func (fr *FrameReader) discard(n int) {
	if n <= 0 {
		return
	}
	if fr.Console != nil {
		// A short write here loses console text, never a frame; the wire is
		// unaffected and the bench's own capture would show the gap.
		_, _ = fr.Console.Write(fr.buf[:n])
	}
	fr.buf = fr.buf[n:]
	fr.resyncs++
}

func indexMagic(b []byte) int {
	for i := 0; i+4 <= len(b); i++ {
		if binary.BigEndian.Uint32(b[i:]) == FrameMagic {
			return i
		}
	}
	return -1
}

// WireOutcome mirrors the firmware's transport::Outcome. It is on the wire
// because the host can fail in ways an HTTP status cannot express — it never
// reached Wasi, or Wasi's certificate did not verify — and §5.3 renders those
// three cases differently (D-6). Carrying the byte is also what lets the bench
// drive the TLS-trust path, which otherwise exists only on the modem.
type WireOutcome uint8

const (
	OutcomeOK            WireOutcome = 0
	OutcomeTransportFail WireOutcome = 1
	OutcomeTLSTrustFail  WireOutcome = 2
)

func (o WireOutcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeTransportFail:
		return "transport_fail"
	case OutcomeTLSTrustFail:
		return "tls_trust_fail"
	default:
		return fmt.Sprintf("outcome(%d)", uint8(o))
	}
}

// RequestPayload is what the device sends inside a FrameRequest.
//
//	u16 seq | u16 auth_len | auth_len bytes authorization | rest: body
//
// Authorization is the device's header value, carried verbatim and forwarded
// untouched (§14). Seq is echoed so a response that arrives after the device
// gave up cannot be applied to the exchange that followed it.
type RequestPayload struct {
	Seq           uint16
	Authorization string
	Body          []byte
}

var errShortPayload = errors.New("chaskibridge: truncated frame payload")

// EncodeRequestPayload is used by tests and by the vector generator; the
// bridge itself only decodes requests.
func EncodeRequestPayload(p RequestPayload) ([]byte, error) {
	if len(p.Authorization) > 0xFFFF {
		return nil, errors.New("chaskibridge: authorization header too long")
	}
	out := make([]byte, 0, 4+len(p.Authorization)+len(p.Body))
	out = binary.BigEndian.AppendUint16(out, p.Seq)
	out = binary.BigEndian.AppendUint16(out, uint16(len(p.Authorization)))
	out = append(out, p.Authorization...)
	return append(out, p.Body...), nil
}

func DecodeRequestPayload(b []byte) (RequestPayload, error) {
	const fixed = 4
	if len(b) < fixed {
		return RequestPayload{}, errShortPayload
	}
	authLen := int(binary.BigEndian.Uint16(b[2:4]))
	if fixed+authLen > len(b) {
		return RequestPayload{}, errShortPayload
	}
	return RequestPayload{
		Seq:           binary.BigEndian.Uint16(b[0:2]),
		Authorization: string(b[fixed : fixed+authLen]),
		Body:          append([]byte(nil), b[fixed+authLen:]...),
	}, nil
}

// ResponsePayload is what the host sends back inside a FrameResponse.
//
//	u16 seq | u8 outcome | u16 http_status | u16 ra_len |
//	ra_len bytes Retry-After | rest: body
//
// RetryAfter crosses as the header's verbatim text. The bridge does not
// interpret headers, and §5.3's parsing then lives in one place on the device
// for both transports rather than two that can disagree.
type ResponsePayload struct {
	Seq        uint16
	Outcome    WireOutcome
	HTTPStatus uint16
	RetryAfter string
	Body       []byte
}

func EncodeResponsePayload(p ResponsePayload) ([]byte, error) {
	if len(p.RetryAfter) > 0xFFFF {
		return nil, errors.New("chaskibridge: Retry-After header too long")
	}
	out := make([]byte, 0, 7+len(p.RetryAfter)+len(p.Body))
	out = binary.BigEndian.AppendUint16(out, p.Seq)
	out = append(out, byte(p.Outcome))
	out = binary.BigEndian.AppendUint16(out, p.HTTPStatus)
	out = binary.BigEndian.AppendUint16(out, uint16(len(p.RetryAfter)))
	out = append(out, p.RetryAfter...)
	return append(out, p.Body...), nil
}

// DecodeResponsePayload is used by tests and the bench harness; the bridge
// itself only encodes responses.
func DecodeResponsePayload(b []byte) (ResponsePayload, error) {
	const fixed = 7
	if len(b) < fixed {
		return ResponsePayload{}, errShortPayload
	}
	raLen := int(binary.BigEndian.Uint16(b[5:7]))
	if fixed+raLen > len(b) {
		return ResponsePayload{}, errShortPayload
	}
	return ResponsePayload{
		Seq:        binary.BigEndian.Uint16(b[0:2]),
		Outcome:    WireOutcome(b[2]),
		HTTPStatus: binary.BigEndian.Uint16(b[3:5]),
		RetryAfter: string(b[fixed : fixed+raLen]),
		Body:       append([]byte(nil), b[fixed+raLen:]...),
	}, nil
}
