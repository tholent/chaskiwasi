// Implementation of components/kipu — see include/chaski/kipu.h for the
// contract and the spec clauses it must satisfy.
//
// Two jobs, and the second is the one that matters to the child: assemble the
// tier-1 block, and keep the ring the settings screen reads back to her
// (design §3.7, client §11.7). This file therefore holds no user-visible text
// at all — the rendering of "battery 64%, good signal" is words on a screen and
// belongs in chaski_strings.c like every other word (C-15). What lives here is
// the numbers those words are made from, and the guarantee that nothing else
// is in the block.
#include "chaski/kipu.h"

#include <algorithm>

namespace chaski::kipu {
namespace {

// Bounds on the tier-1 fields. They exist for size, not for taste: the block
// is capped at wire::kMaxKipuBytes and an oversized one is dropped by the
// encoder rather than failing the sync (server §4.8), so a fuel gauge
// returning 32767 must not be able to cost the whole block. Every field is
// clamped to a width the worst case can be counted with.
constexpr int kMinBatteryPct = 0;
constexpr int kMaxBatteryPct = 100;

// dBm, and always negative in practice; the range is generous on both sides so
// a modem quirk clamps instead of surprising anyone.
constexpr int kMinRssiDbm = -199;
constexpr int kMaxRssiDbm = 99;

// The outbox cap is far below this; the clamp is a backstop against a caller
// passing a garbage count, not a policy about how many letters may wait.
constexpr int kMinQueueDepth = 0;
constexpr int kMaxQueueDepth = 9999;

// RAT identifiers ("ltem", "nbiot") and firmware versions are short ASCII
// machine strings. Anything longer is a bug upstream, and truncating it here
// is better than letting it push the block over the cap.
constexpr std::size_t kMaxRatBytes = 16;
constexpr std::size_t kMaxFwBytes = 64;

// Ascii keeps the block encodable: it drops anything outside printable ASCII
// before truncating, so a truncation can never split a multi-byte sequence and
// hand the JSON encoder an invalid string. These fields are identifiers; there
// is nothing here that a person wrote.
std::string Ascii(const std::string& s, std::size_t max_bytes) {
  std::string out;
  out.reserve(std::min(s.size(), max_bytes));
  for (const char c : s) {
    if (out.size() == max_bytes) break;
    if (c >= 0x20 && c <= 0x7e) out.push_back(c);
  }
  return out;
}

class RingLog final : public Log {
 public:
  // A short ring, overwriting oldest first. The kipu is designed to be
  // forgotten (client §13, A.6): what the screen can show is exactly what the
  // device keeps, so there is no history here to ask a question of later.
  void Record(const Entry& e) override {
    if (entries_.size() < kLogCapacity) {
      entries_.push_back(e);
      return;
    }
    entries_[oldest_] = e;
    oldest_ = (oldest_ + 1) % kLogCapacity;
  }

  // Newest first: the settings screen lists the most recent sync at the top,
  // and a caller asking for 5 wants the last 5, not the first 5.
  std::vector<Entry> Recent(std::size_t n) const override {
    const std::size_t have = entries_.size();
    const std::size_t count = std::min(n, have);
    std::vector<Entry> out;
    out.reserve(count);
    for (std::size_t i = 0; i < count; ++i) {
      out.push_back(entries_[(oldest_ + have - 1 - i) % have]);
    }
    return out;
  }

 private:
  std::vector<Entry> entries_;
  // Index of the oldest entry, meaningful only once the ring is full; before
  // that entries_ is in order and this stays 0.
  std::size_t oldest_ = 0;
};

}  // namespace

// Health only (server §4.8, client §13). There is no position argument and no
// engagement argument, which is the v1 promise stated as a signature: adding
// one is a spec decision paired with the server's cell service, not a
// convenience a caller can reach for.
wire::Kipu Build(int battery_pct, bool charging, const std::string& rat, int rssi,
                 int queue_depth, const std::string& fw) {
  wire::Kipu k;
  k.battery_pct = std::min(std::max(battery_pct, kMinBatteryPct), kMaxBatteryPct);
  k.charging = charging;
  k.rat = Ascii(rat, kMaxRatBytes);
  k.rssi = std::min(std::max(rssi, kMinRssiDbm), kMaxRssiDbm);
  k.queue_depth = std::min(std::max(queue_depth, kMinQueueDepth), kMaxQueueDepth);
  k.fw = Ascii(fw, kMaxFwBytes);
  return k;
}

std::unique_ptr<Log> NewLog() { return std::unique_ptr<Log>(new RingLog()); }

}  // namespace chaski::kipu
