// Framing for the dev transport's USB-CDC link (client §14).
//
// §14 asks for "a framed USB-CDC protocol (length-prefixed request out, status
// + length-prefixed response in)". A bare length prefix is not enough on this
// link: the host end is a serial port that a developer may attach to
// mid-stream, the device reboots without warning (that is what C-4 does on
// purpose), and either event leaves the reader mid-frame with no way to tell
// where the next one starts. Hence the magic — a receiver that has lost the
// stream scans for it and resumes — and the CRC, which is what makes a
// *candidate* magic inside a payload distinguishable from a real frame start.
//
// Wire layout, all integers big-endian:
//
//   offset 0   u32  magic   0x43484B31 "CHK1"
//   offset 4   u8   type
//   offset 5   u32  length  payload bytes, <= kMaxPayloadBytes
//   offset 9   ...  payload
//   offset 9+n u32  crc32   IEEE CRC-32 over bytes [4, 9+n) — type, length,
//                           and payload; the magic is excluded because a
//                           receiver that guessed wrong about the magic must
//                           not have that guess confirmed by its own constant
//
// A frame whose length exceeds the cap, or whose CRC does not match, is
// rejected and the decoder resynchronises rather than trusting it. A truncated
// frame is never delivered: the payload and trailer must both be present
// before anything is handed up.
//
// The identical codec lives in tools/chaskibridge (Go). The two are held
// together by generated vectors under test/firmware/host/testdata/frames/,
// parsed by both sides — the same argument tools/graphvectors makes for
// graphemes (B.7): two implementations that merely look alike drift, and the
// drift is silent.
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

namespace chaski::transport::frame {

// kMagic starts every frame, so a resync after line noise or a device reset is
// possible without guessing.
inline constexpr std::uint32_t kMagic = 0x43484B31u;  // "CHK1"

// kMaxPayloadBytes bounds the declared length. The server caps a request at
// 64 KB (server §4.1) and the dev link is no more permissive than the real
// endpoint. The bound is on the payload, so a frame occupies at most
// kMaxPayloadBytes + kOverheadBytes on the wire.
inline constexpr std::size_t kMaxPayloadBytes = 64u * 1024u;

inline constexpr std::size_t kHeaderBytes = 9;
inline constexpr std::size_t kTrailerBytes = 4;
inline constexpr std::size_t kOverheadBytes = kHeaderBytes + kTrailerBytes;

// Type says what a payload is. Unknown types are framed and delivered like any
// other: a receiver ignores what it does not know, so one end can grow a frame
// kind without the other being reflashed first.
enum class Type : std::uint8_t {
  kRequest = 0x01,   // device -> host: authorization + sync request body
  kResponse = 0x02,  // host -> device: outcome + status + Retry-After + body

  // The bench control channel. It exists because the bench tier (C-1, C-2,
  // C-4, C-7) has to make the device compose and sync with no keyboard and no
  // UI, and the USB link is the only wire there is. Payloads are opaque UTF-8
  // JSON to this codec; the vocabulary is documented in
  // test/firmware/bench/README.md and handled in the dev build only.
  kCommand = 0x03,  // host -> device
  kEvent = 0x04,    // device -> host
};

// Encode appends one framed message to out. It fails only when payload exceeds
// the cap, which is a caller bug rather than a wire condition.
bool Encode(Type type, const std::string& payload, std::string* out);

// DecodeStatus reports what the decoder could do with the bytes it holds.
enum class DecodeStatus {
  kNeedMoreBytes,  // nothing wrong, the frame is not all here yet
  kFrame,          // a whole, CRC-checked frame is in the out params
};

// Decoder turns a byte stream into frames, resynchronising past anything that
// is not one. It buffers at most one maximum-size frame plus the bytes it is
// scanning through, so a corrupted length cannot make it allocate without
// bound.
class Decoder {
 public:
  // Append adds received bytes. Bytes ahead of a frame boundary are discarded
  // as the decoder advances, so the buffer does not grow with the session.
  void Append(const std::uint8_t* data, std::size_t n);

  // Next returns the oldest complete frame, or kNeedMoreBytes. Call it in a
  // loop until it reports kNeedMoreBytes: one Append can complete several
  // frames.
  DecodeStatus Next(Type* type, std::string* payload);

  // resyncs counts how many times the decoder discarded bytes to find a frame
  // boundary: a corrupted or truncated frame, or a false magic inside a
  // payload. Content-free by construction and worth logging (D-7).
  std::size_t resyncs() const { return resyncs_; }

  void Reset();

 private:
  std::string buf_;
  std::size_t resyncs_ = 0;
};

// Crc32 is the IEEE CRC-32 the frame trailer carries. Exposed because both
// sides' tests check it against the shared vectors.
std::uint32_t Crc32(const std::uint8_t* data, std::size_t n);

// ---- Payload codecs -------------------------------------------------------
//
// The framing above says nothing about what a payload means; these say it.
// They are deliberately positional binary rather than JSON: the device already
// parses one JSON document per sync and there is no reason to make it parse
// two, and a fixed layout cannot be "helpfully" extended by the wire in the
// middle.

// RequestPayload is what the device sends. The authorization value travels
// verbatim and the bridge forwards it untouched (§14): the firmware exercises
// real bearer auth against a real Wasi, and the bridge never becomes a place
// where auth is invented.
//
//   u16 seq | u16 auth_len | auth_len bytes authorization | rest: request body
//
// `seq` is echoed by the host and exists because the link outlives any one
// exchange. A request that timed out is still in flight on the host's HTTP
// connection; if its response arrived during the *next* attempt, the device
// would apply a response computed for a different cursor — letters skipped by
// a §5.2 step that looked entirely healthy. Matching the sequence is what
// makes "one request, one response" true of a shared byte pipe.
struct RequestPayload {
  std::uint16_t seq = 0;
  std::string authorization;  // e.g. "Bearer <device-token>"
  std::string body;           // the sync request JSON, opaque here
};

bool EncodeRequest(const RequestPayload& in, std::string* out);
bool DecodeRequest(const std::string& in, RequestPayload* out);

// WireOutcome mirrors transport::Outcome on the wire. It is carried explicitly
// because the host end can fail in ways an HTTP status cannot express — it
// never reached Wasi, or Wasi's certificate did not verify — and §5.3 renders
// those three cases differently (D-6). Keeping the byte in the protocol is
// also what lets the bench drive the TLS-trust path, which otherwise exists
// only on the production modem.
enum class WireOutcome : std::uint8_t {
  kOk = 0,
  kTransportFail = 1,
  kTlsTrustFail = 2,
};

// ResponsePayload is what the host sends back.
//
//   u16 seq | u8 outcome | u16 http_status | u16 ra_len |
//   ra_len bytes Retry-After | rest: response body
//
// Retry-After crosses as the header's verbatim text, not as a number. The
// bridge is a wire and does not interpret headers, and §5.3's parsing then
// lives in one place for both transports instead of two that can disagree.
struct ResponsePayload {
  std::uint16_t seq = 0;  // echoed from the request it answers
  WireOutcome outcome = WireOutcome::kTransportFail;
  std::uint16_t http_status = 0;
  std::string retry_after;  // empty when the header was absent
  std::string body;
};

bool EncodeResponse(const ResponsePayload& in, std::string* out);
bool DecodeResponse(const std::string& in, ResponsePayload* out);

// ParseRetryAfterSeconds reads the delta-seconds form of Retry-After and
// returns 0 for anything else, including the HTTP-date form. The date form is
// deliberately unsupported: the device's clock is invalid until the first sync
// of a power cycle (§5.6), so a date would be interpreted against a wall clock
// that may not exist. 0 means "no advice", and syncengine falls back to the
// §5.3 schedule — which is the correct behaviour, not a degradation.
int ParseRetryAfterSeconds(const std::string& header_value);

}  // namespace chaski::transport::frame
