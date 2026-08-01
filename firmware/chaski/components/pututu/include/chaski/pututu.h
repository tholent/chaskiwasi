// Package pututu verifies the SMS doorbell.
//
// The token is opaque by contract: no sender name, no content, ever. SMS is
// plaintext and carrier-buffered, so a name in it would leak exactly what
// contact resolution protects (server §10.2). Format: CH1.<counter>.<mac>.
//
// Device rules, verbatim from server §10.2 and client §7:
//   - verify the MAC
//   - accept only a counter strictly greater than the highest accepted
//   - persist that value across power loss
//   - ignore every failure SILENTLY: no response, no wake, no UI
//   - rate-limit SMS-triggered wakes to one per 5 minutes regardless of
//     validity, so even a validation bug cannot become a battery or balance
//     drain attack
#pragma once

#include <cstdint>
#include <functional>
#include <memory>
#include <string>

namespace chaski::pututu {

enum class Verdict {
  kAccept,        // valid, fresh: schedule a sync
  kBadFormat,
  kBadMac,
  kStaleCounter,  // replay or forgery
  kRateLimited,   // valid or not, too soon since the last SMS-triggered wake
  kNotPersisted,  // valid and fresh, but the high-water write failed
};

// CounterStore persists the high-water counter. On the target this is NVS;
// in host tests, memory. It must be written BEFORE acting on an accepted
// token, so a crash cannot re-accept the same counter.
class CounterStore {
 public:
  virtual ~CounterStore() = default;
  virtual std::uint64_t HighWater() const = 0;
  virtual bool SetHighWater(std::uint64_t v) = 0;
};

class Verifier {
 public:
  virtual ~Verifier() = default;

  // Verify parses and checks one SMS body. It never logs the body and never
  // produces user-visible text: a failure is silence by design.
  virtual Verdict Verify(const std::string& sms_body) = 0;

  // CounterSeen is reported on every sync so a server restored from backup
  // heals its counter over the wire (server §10.3).
  virtual std::uint64_t CounterSeen() const = 0;
};

// One SMS-triggered wake per 5 minutes (server §10.2). After power loss the
// limiter starts conservative: the first minute after boot counts as inside
// the window (client §7).
inline constexpr int kMinWakeIntervalMs = 5 * 60 * 1000;

// The conservative start: a monotonic clock reading below this is inside the
// window by definition. Keyed on the clock rather than on construction so a
// wake from deep sleep — where the clock keeps running — is not gagged for a
// minute every time, while a power loss — where it restarts near zero — is.
inline constexpr int kBootQuietMs = 60 * 1000;

struct Deps {
  CounterStore* counters = nullptr;
  std::string hmac_key;  // provisioned; never in TOML, never in logs
  std::function<std::int64_t()> monotonic_ms;

  // Optional, and the difference between a rate limit that holds and one that
  // does not: client §7 tracks the limiter in RTC memory, because the wake it
  // limits is the one that ends a deep sleep. A Verifier constructed fresh on
  // each wake keeps nothing, so a caller that sleeps between messages must
  // hand the timestamp somewhere that survives — an RTC_DATA_ATTR variable —
  // through these. Unset keeps it in the instance, which is right for host
  // tests and for any caller that stays resident.
  //
  // The stored unit is the same monotonic_ms scale; 0 means "no wake yet",
  // which is what zeroed RTC memory after power loss already says.
  std::function<std::int64_t()> load_last_wake_ms;
  std::function<void(std::int64_t)> store_last_wake_ms;
};

std::unique_ptr<Verifier> NewVerifier(const Deps& d);

}  // namespace chaski::pututu
