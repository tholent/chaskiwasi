// Package modem is the ONLY caller of the walter-modem library.
//
// Design rule from the hardware architecture: expensive state lives in the
// modem, cheap state gets rebuilt (design §6.4). The GM02SP is separately
// powered and holds network registration, PDP context, and TLS sessions across
// ESP32 deep sleep; the ESP32 reboots through app_main on every wake. So this
// component never power-cycles the modem in normal operation and never
// re-attaches per sync.
//
// TLS trusts exactly the two pinned private CAs and nothing else (D-6, server
// §12.2, A.7). A trust failure must surface as its own visible state, not as a
// generic network error.
#pragma once

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

namespace chaski::modem {

struct Signal {
  std::string rat;   // "ltem" | "nbiot"
  int rssi = 0;
  bool registered = false;
};

class Modem {
 public:
  virtual ~Modem() = default;

  virtual bool Begin() = 0;

  // EnsureAttached brings up registration and the PDP context if they are not
  // already held across sleep. Cheap when the modem kept them.
  virtual bool EnsureAttached(int timeout_ms) = 0;

  // SyncTrustStore writes the compiled-in CA roots to the modem's TLS profile
  // when they differ from what it holds. This is what makes the trust store
  // updatable through the USB firmware path, as D-6 requires.
  virtual bool SyncTrustStore(const std::vector<std::string>& ca_pems) = 0;

  // HttpsPost performs the sync POST. Distinguishing a TLS trust failure from
  // a transport failure is REQUIRED, not optional (client §5.3).
  virtual bool HttpsPost(const std::string& url, const std::string& bearer,
                         const std::string& body, int& out_status,
                         std::string& out_body, bool& out_tls_trust_fail) = 0;

  // DrainSms returns buffered doorbell messages. The modem buffers SMS while
  // the ESP32 sleeps, which is what dissolves the SMS-wake problem: no
  // interrupt line is required, just a local serial transaction on timer wake
  // (design §6.4).
  virtual std::vector<std::string> DrainSms() = 0;

  virtual bool SetPsm(bool enable, int tau_s, int active_s) = 0;
  virtual bool SetRat(const std::string& rat) = 0;  // pushed config (§5.5)
  virtual Signal ReadSignal() = 0;
  virtual void PowerDownRadio() = 0;  // used by the emergency wipe (§9.1)
};

}  // namespace chaski::modem
