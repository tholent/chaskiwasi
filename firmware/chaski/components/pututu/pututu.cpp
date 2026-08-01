// Implementation of components/pututu — see include/chaski/pututu.h for the
// contract and the spec clauses it must satisfy.
//
// Nothing in this file logs, at any level. Client §7 allows a debug line
// carrying the counter and forbids anything more; a file that contains no
// logging statement cannot drift into carrying the token, and the token is the
// one thing on this path produced with a MAC key. The verdict is returned
// instead — the caller decides, with the SMS body already out of scope (D-7).
#include "chaski/pututu.h"

#include <cstdint>
#include <limits>
#include <string>

#include "hmac_sha256.h"

namespace chaski::pututu {
namespace {

// server §10.2: CH1.<counter>.<mac>.
constexpr char kPrefix[] = "CH1.";
constexpr std::size_t kPrefixLen = sizeof(kPrefix) - 1;

// The MAC is base64 of the first 12 digest bytes. 12 is a multiple of 3, so
// the encoding is exactly 16 characters and carries no padding.
constexpr std::size_t kMacBytes = 12;
constexpr std::size_t kMacChars = 16;

// A uint64 is at most 20 decimal digits; anything longer cannot be a counter
// and is rejected before the overflow check ever runs.
constexpr std::size_t kMaxCounterDigits = 20;

// ParseCounter accepts only the canonical decimal rendering of a uint64 — no
// sign, no leading zero, no whitespace.
//
// Strictness matters here beyond taste: the MAC is computed over the ASCII
// counter, so "7" and "007" are different messages. Accepting both would mean
// the value the device stores and the bytes the device authenticated are no
// longer in one-to-one correspondence. Rejecting non-canonical forms keeps
// them so, which is what lets the MAC be recomputed from the parsed integer.
bool ParseCounter(const std::string& s, std::uint64_t& out) {
  if (s.empty() || s.size() > kMaxCounterDigits) return false;
  if (s.size() > 1 && s[0] == '0') return false;
  constexpr std::uint64_t kMax = std::numeric_limits<std::uint64_t>::max();
  std::uint64_t v = 0;
  for (const char c : s) {
    if (c < '0' || c > '9') return false;
    const std::uint64_t d = static_cast<std::uint64_t>(c - '0');
    if (v > (kMax - d) / 10) return false;
    v = v * 10 + d;
  }
  out = v;
  return true;
}

class VerifierImpl final : public Verifier {
 public:
  explicit VerifierImpl(const Deps& d)
      : counters_(d.counters),
        key_(d.hmac_key),
        monotonic_ms_(d.monotonic_ms),
        load_last_wake_ms_(d.load_last_wake_ms),
        store_last_wake_ms_(d.store_last_wake_ms) {}

  Verdict Verify(const std::string& sms_body) override {
    // server §10.2: the limit is on SMS-triggered wakes "regardless of
    // validity", so it gates the whole function rather than the accept path.
    // That is the point — a validation bug that accepted everything would
    // still cost at most one wake per five minutes. The price is that a
    // stranger who knows the number, or a carrier's own balance notice, can
    // delay a real doorbell by up to five minutes; the doorbell only ever
    // saves the device a wait until its next periodic sync, so that is the
    // cheaper side of the trade.
    const std::int64_t now = Now();
    if (RateLimited(now)) return Verdict::kRateLimited;
    RecordWake(now);

    std::uint64_t counter = 0;
    std::string mac;
    if (!Split(sms_body, counter, mac)) return Verdict::kBadFormat;

    // The MAC is checked before the counter is compared, so an unauthenticated
    // message never reaches the counter store at all.
    if (!crypto::Equals(mac, ComputeMac(counter))) return Verdict::kBadMac;

    if (counters_ == nullptr) return Verdict::kNotPersisted;

    // server §10.2: accept only a counter strictly greater than the highest
    // previously accepted. This is what makes a replayed token — the same
    // bytes, a valid MAC — worth nothing.
    if (counter <= counters_->HighWater()) return Verdict::kStaleCounter;

    // client §7: persist the new high-water BEFORE acting on it. A crash
    // between the write and the sync costs one missed doorbell, which the next
    // periodic sync covers; the other order would let the same counter be
    // accepted twice, which is the replay the counter exists to stop.
    if (!counters_->SetHighWater(counter)) return Verdict::kNotPersisted;
    return Verdict::kAccept;
  }

  // server §10.3: reported on every sync so a Wasi restored from backup jumps
  // its counter past what the device has already seen. The device needs no
  // logic beyond reporting its high-water; the healing is server-side (§7).
  std::uint64_t CounterSeen() const override {
    return counters_ == nullptr ? 0 : counters_->HighWater();
  }

 private:
  std::int64_t Now() const {
    // No clock means no way to honour the limit, so the limit holds shut: 0 is
    // inside the boot-quiet window and every message is refused. A
    // misconfiguration costs the doorbell, never the battery.
    return monotonic_ms_ ? monotonic_ms_() : 0;
  }

  std::int64_t LastWake() const {
    return load_last_wake_ms_ ? load_last_wake_ms_() : last_wake_ms_;
  }

  bool RateLimited(std::int64_t now) const {
    if (now < kBootQuietMs) return true;
    const std::int64_t last = LastWake();
    if (last == 0) return false;  // no wake yet, and the quiet minute is past
    // A clock that ran backwards is not a clock to make an exception for.
    return now < last || now - last < kMinWakeIntervalMs;
  }

  void RecordWake(std::int64_t now) {
    // now is at least kBootQuietMs here (RateLimited refuses everything below
    // it), so it can never collide with the 0 that means "no wake yet".
    last_wake_ms_ = now;
    if (store_last_wake_ms_) store_last_wake_ms_(now);
  }

  bool Split(const std::string& body, std::uint64_t& counter, std::string& mac) const {
    if (body.size() <= kPrefixLen) return false;
    if (body.compare(0, kPrefixLen, kPrefix) != 0) return false;
    const std::size_t dot = body.find('.', kPrefixLen);
    if (dot == std::string::npos) return false;
    if (body.size() - dot - 1 != kMacChars) return false;
    if (body.find('.', dot + 1) != std::string::npos) return false;
    if (!ParseCounter(body.substr(kPrefixLen, dot - kPrefixLen), counter)) return false;
    mac = body.substr(dot + 1);
    return true;
  }

  // ComputeMac renders the counter the way the server does and MACs that.
  // Comparing base64 rather than decoding the message's own MAC means a
  // non-canonical encoding of the right bytes is a mismatch, not an accept.
  std::string ComputeMac(std::uint64_t counter) const {
    const std::string text = std::to_string(counter);
    std::uint8_t digest[crypto::kSha256DigestBytes];
    crypto::HmacSha256(reinterpret_cast<const std::uint8_t*>(key_.data()), key_.size(),
                       reinterpret_cast<const std::uint8_t*>(text.data()), text.size(),
                       digest);
    return crypto::Base64(digest, kMacBytes);
  }

  CounterStore* counters_;
  const std::string key_;
  std::function<std::int64_t()> monotonic_ms_;
  std::function<std::int64_t()> load_last_wake_ms_;
  std::function<void(std::int64_t)> store_last_wake_ms_;
  std::int64_t last_wake_ms_ = 0;
};

}  // namespace

// Never returns null, even for incomplete deps: every failure on this path is
// silence (§7), and a null the caller forgot to check is a crash instead.
std::unique_ptr<Verifier> NewVerifier(const Deps& d) {
  return std::unique_ptr<Verifier>(new VerifierImpl(d));
}

}  // namespace chaski::pututu
