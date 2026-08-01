// UsbBridgeTransport — the development half of the transport seam (client §14,
// decision B.2).
//
// It speaks the §14 frame protocol over USB-CDC to tools/chaskibridge, which
// forwards to a real Wasi. The dev path is USB and never a radio: WiFi and BLE
// are absent from every build (D-3), so no build flag can turn the dev
// transport into one C-16 would catch.
//
// It is deliberately dumb. It performs no retries, interprets no status codes,
// and holds no policy — §5.3's backoff and the §11.6 fault states live in
// syncengine, and both transports feed it the same three-way Outcome so the
// mapping is exercised identically in dev and prod.
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

#include "chaski/frame.h"
#include "chaski/transport.h"

namespace chaski::transport {

// SerialLink is the byte pipe to the host. It exists so the transport is
// host-testable against an in-memory pipe: the target implementation is
// USB-CDC and lives in a translation unit that only the IDF build compiles
// (implementation-plan ground rule 3 — no esp_* headers reach this logic).
//
// MonotonicMs lives here rather than in a clock seam of its own because the
// only thing that needs it is the read deadline, and the object that owns the
// blocking read is the object that can honour one. Two injected seams for one
// timeout would be ceremony.
class SerialLink {
 public:
  virtual ~SerialLink() = default;

  // Write sends the whole buffer, or reports false. Partial writes are the
  // implementation's problem, not the caller's.
  virtual bool Write(const std::uint8_t* data, std::size_t n) = 0;

  // Read returns however many bytes are available, blocking at most
  // timeout_ms. 0 means the timeout elapsed with nothing to read, which is not
  // an error — the host may simply still be waiting on Wasi.
  virtual std::size_t Read(std::uint8_t* buf, std::size_t cap, int timeout_ms) = 0;

  // MonotonicMs is a clock that never goes backwards and survives nothing;
  // only differences are meaningful.
  virtual std::int64_t MonotonicMs() = 0;
};

// Stats are content-free by construction — counts only, never bytes of a body.
// The transport logs nothing itself (D-7, C-19): a component that formats a
// body into a string is one refactor away from formatting it into a log line,
// so it never holds one. Callers log these numbers instead.
struct UsbBridgeStats {
  int requests = 0;
  int responses = 0;
  int timeouts = 0;
  int malformed = 0;    // a framed payload that did not decode
  int mismatched = 0;   // a response for an exchange that already gave up
  std::size_t resyncs = 0;
};

// kDefaultTimeoutMs matches chaskibridge.DefaultTimeout on the host side; the
// device waits a little longer than the host does so a host-side timeout is
// reported as an outcome rather than racing the device's own.
inline constexpr int kDefaultTimeoutMs = 45000;

// UsbBridgeTransport is concrete in the header rather than behind a factory
// because the composition root needs its stats and this codebase builds
// without RTTI — there is no dynamic_cast to recover the type with.
class UsbBridgeTransport final : public Transport {
 public:
  // `authorization` is the full header value ("Bearer <token>"), read from
  // factory NVS by the composition root. It crosses the link verbatim and the
  // bridge forwards it untouched, so the device exercises the same §4.1 auth
  // the modem path will.
  //
  // `timeout_ms` bounds one exchange. It is generous by design: a bench sync
  // crossing the compose stack is slower than LTE, and a timeout here would be
  // read as a device fault.
  UsbBridgeTransport(SerialLink* link, std::string authorization,
                     int timeout_ms = kDefaultTimeoutMs);

  Result Sync(const std::string& request_json) override;
  const char* Name() const override { return "usbbridge"; }

  const UsbBridgeStats& stats() const { return stats_; }

 private:
  SerialLink* link_;
  std::string authorization_;
  int timeout_ms_;
  std::uint16_t seq_ = 0;
  UsbBridgeStats stats_;
  // The decoder outlives one exchange on purpose: bytes that arrive after a
  // give-up are part of this stream and must be parsed, not guessed at. The
  // sequence check is what discards them safely.
  frame::Decoder decoder_;
};

}  // namespace chaski::transport
