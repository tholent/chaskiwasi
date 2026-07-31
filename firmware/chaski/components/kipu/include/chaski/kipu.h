// Package kipu assembles the tier-1 health block and keeps the readable log.
//
// v1 is health ONLY: battery, charging, RAT, signal, queue depth, firmware
// version. No position, no behaviour, no engagement data (server §4.8, client
// §13). Tier-2 coarse position and its opt-out are v2 and arrive with the
// server's cell service.
//
// The readable log is a transparency MECHANISM, not a promise in a document
// (design §3.7): the child can see, in plain language, exactly what was sent —
// "Tue 15:04 - battery 64%, good signal, 1 letter waiting". It is a short ring;
// the kipu is designed to be forgotten.
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "chaski/wire.h"

namespace chaski::kipu {

struct Entry {
  std::int64_t at = 0;
  wire::Kipu block;
};

// Build assembles the block for a sync request. Serialised size must stay
// under wire::kMaxKipuBytes.
wire::Kipu Build(int battery_pct, bool charging, const std::string& rat,
                 int rssi, int queue_depth, const std::string& fw);

// Log is the on-device ring the settings screen renders (client §11.7).
class Log {
 public:
  virtual ~Log() = default;
  virtual void Record(const Entry& e) = 0;
  virtual std::vector<Entry> Recent(std::size_t n) const = 0;
};

inline constexpr std::size_t kLogCapacity = 32;

std::unique_ptr<Log> NewLog();

}  // namespace chaski::kipu
