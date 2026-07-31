// Package transport is the entire network surface of the device (client §14).
//
// One seam, two implementations. Production is ModemTransport: HTTPS through
// the GM02SP against the two pinned private CAs (D-6). Development is
// UsbBridgeTransport: a framed USB-CDC link to tools/chaskibridge on a host,
// which forwards to a real Wasi — normally the compose stack with maddy.
//
// The dev path is USB, never a radio. WiFi and BLE are not compiled into ANY
// build (D-3, B.2), so no build flag can accidentally add one; C-16 asserts it
// by scanning the linked ELF.
//
// Everything above this seam — sync engine, stores, dedup, acks, UI — is
// identical in both variants. That is what makes the bench suite meaningful.
#pragma once

#include <memory>
#include <string>

namespace chaski::transport {

// Outcome separates the three failure kinds the device must render
// differently (client §5.3, §11.6). Collapsing them loses the distinction
// between "the road is busy" and "can't reach home", and D-6 requires a TLS
// trust failure to be visibly its own state.
enum class Outcome {
  kOk,             // an HTTP response arrived; read http_status
  kTransportFail,  // no response: no signal, no attach, timeout
  kTlsTrustFail,   // the peer's certificate did not chain to a pinned CA
};

struct Result {
  Outcome outcome = Outcome::kTransportFail;
  int http_status = 0;      // meaningful only when outcome == kOk
  std::string body;         // response body when outcome == kOk
  int retry_after_s = 0;    // parsed Retry-After on 503, 0 when absent
};

// Transport submits one sync request and returns one result. It performs no
// retries and holds no policy: backoff, status interpretation, and state
// application all live in syncengine (client §5.3), so both implementations
// stay dumb and testable.
class Transport {
 public:
  virtual ~Transport() = default;

  // Sync POSTs `request_json` with the device bearer token and returns the
  // outcome. Implementations MUST NOT log the request or response body — it
  // carries letter content (D-7, C-19).
  virtual Result Sync(const std::string& request_json) = 0;

  // Name identifies the implementation for content-free diagnostics.
  virtual const char* Name() const = 0;
};

}  // namespace chaski::transport
