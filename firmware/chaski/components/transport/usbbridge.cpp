// Implementation of the dev transport. See include/chaski/usbbridge.h.
#include "chaski/usbbridge.h"

namespace chaski::transport {
namespace {

// One poll is short enough that a give-up is prompt and long enough that the
// device is not spinning on an idle link for the whole exchange window.
constexpr int kPollMs = 100;
constexpr std::size_t kReadChunk = 512;

Result Failed(Outcome o) {
  Result r;
  r.outcome = o;
  return r;
}

}  // namespace

UsbBridgeTransport::UsbBridgeTransport(SerialLink* link, std::string authorization, int timeout_ms)
    : link_(link), authorization_(std::move(authorization)), timeout_ms_(timeout_ms) {}

Result UsbBridgeTransport::Sync(const std::string& request_json) {
  if (link_ == nullptr) return Failed(Outcome::kTransportFail);

  // Sequence numbers wrap; they only ever need to distinguish this exchange
  // from the one before it (see frame.h on why the correlation exists).
  const std::uint16_t seq = ++seq_;

  frame::RequestPayload req;
  req.seq = seq;
  req.authorization = authorization_;
  req.body = request_json;

  std::string payload;
  std::string framed;
  if (!frame::EncodeRequest(req, &payload) ||
      !frame::Encode(frame::Type::kRequest, payload, &framed)) {
    // Only reachable when the request exceeds the 64 KB cap the server also
    // enforces (server §4.1). Nothing left the device, so nothing was acked.
    return Failed(Outcome::kTransportFail);
  }

  if (!link_->Write(reinterpret_cast<const std::uint8_t*>(framed.data()), framed.size())) {
    return Failed(Outcome::kTransportFail);
  }
  ++stats_.requests;

  const std::int64_t deadline = link_->MonotonicMs() + timeout_ms_;
  std::uint8_t chunk[kReadChunk];

  for (;;) {
    frame::Type type = frame::Type::kRequest;
    std::string got;
    while (decoder_.Next(&type, &got) == frame::DecodeStatus::kFrame) {
      stats_.resyncs = decoder_.resyncs();
      // Frames that are not ours belong to the bench control channel. Ignoring
      // unknown types is what lets one end of the link grow a frame kind
      // without the other being reflashed.
      if (type != frame::Type::kResponse) continue;

      frame::ResponsePayload resp;
      if (!frame::DecodeResponse(got, &resp)) {
        ++stats_.malformed;
        continue;
      }
      if (resp.seq != seq) {
        // The answer to an exchange this device already gave up on. Applying
        // it would apply a response computed against a different cursor.
        ++stats_.mismatched;
        continue;
      }

      ++stats_.responses;
      Result out;
      switch (resp.outcome) {
        case frame::WireOutcome::kOk:
          out.outcome = Outcome::kOk;
          out.http_status = resp.http_status;
          out.body = std::move(resp.body);
          // §5.3: the header is advice for 503. It is parsed here rather than
          // on the host so both transports read it with the same code.
          out.retry_after_s = frame::ParseRetryAfterSeconds(resp.retry_after);
          break;
        case frame::WireOutcome::kTlsTrustFail:
          // D-6: the bridge's own TLS to Wasi did not verify. Distinct from
          // "no signal" all the way up to the screen (§11.6).
          out.outcome = Outcome::kTlsTrustFail;
          break;
        case frame::WireOutcome::kTransportFail:
          out.outcome = Outcome::kTransportFail;
          break;
      }
      return out;
    }
    stats_.resyncs = decoder_.resyncs();

    const std::int64_t remaining = deadline - link_->MonotonicMs();
    if (remaining <= 0) {
      ++stats_.timeouts;
      return Failed(Outcome::kTransportFail);
    }

    const int wait = remaining < kPollMs ? static_cast<int>(remaining) : kPollMs;
    const std::size_t n = link_->Read(chunk, sizeof(chunk), wait);
    if (n > 0) decoder_.Append(chunk, n);
  }
}

}  // namespace chaski::transport
