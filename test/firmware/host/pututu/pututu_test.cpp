// Host tests for components/pututu: the SMS doorbell's verifier (client §7,
// server §10.2, test C-8).
//
// Four properties, and the suite is arranged around them:
//   - the device agrees with the server about what a valid token is, proved
//     against tokens minted by internal/pututu itself, never by hand;
//   - a counter is accepted once and never again, across power loss;
//   - the high-water is persisted before the accept is returned;
//   - one SMS-triggered wake per five minutes, whatever the message was.
#include "chaski/pututu.h"

#include <cstdint>
#include <fstream>
#include <memory>
#include <sstream>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "cJSON.h"
#include "hmac_sha256.h"

namespace {

using chaski::pututu::Deps;
using chaski::pututu::kBootQuietMs;
using chaski::pututu::kMinWakeIntervalMs;
using chaski::pututu::Verdict;
using chaski::pututu::Verifier;

// The key the committed vectors were generated with (tools/pututuvectors'
// typicalKey). It is a test fixture, not a secret: the real one is provisioned
// into NVS and appears in no source file.
constexpr char kKey[] = "a-provisioned-pututu-key-32-bytes";

// Well past the conservative first minute after boot, so a test that is not
// about the boot window does not have to think about it.
constexpr std::int64_t kAfterBoot = kBootQuietMs + 1;

// Counters is the NVS stand-in. The stored value lives OUTSIDE the object so a
// test can destroy the store and the verifier — a power loss — and reopen onto
// the same cell, which is what NVS does and what the monotonicity rule is for.
class Counters : public chaski::pututu::CounterStore {
 public:
  explicit Counters(std::uint64_t* cell) : cell_(cell) {}

  std::uint64_t HighWater() const override { return *cell_; }

  bool SetHighWater(std::uint64_t v) override {
    ++writes;
    if (fail) return false;
    *cell_ = v;
    return true;
  }

  bool fail = false;
  int writes = 0;

 private:
  std::uint64_t* cell_;
};

// Mint builds a token the way the server does. Its agreement with the server is
// not assumed here — C8.ServerMintedTokensAllVerify proves that against
// Go-minted vectors; this exists so tests about counter and rate-limit
// behaviour can use any counter they like.
std::string Mint(std::uint64_t counter, const std::string& key) {
  namespace crypto = chaski::pututu::crypto;
  const std::string text = std::to_string(counter);
  std::uint8_t digest[crypto::kSha256DigestBytes];
  crypto::HmacSha256(reinterpret_cast<const std::uint8_t*>(key.data()), key.size(),
                     reinterpret_cast<const std::uint8_t*>(text.data()), text.size(), digest);
  return "CH1." + text + "." + crypto::Base64(digest, 12);
}

// Fixture wires a verifier to a settable clock and a persistent counter cell.
class Fixture {
 public:
  Fixture() { Open(); }

  // Open is a boot: a new verifier over the same counter cell, with the
  // monotonic clock back at zero the way it is after power loss.
  void Open() {
    verifier_.reset();
    counters_ = std::make_unique<Counters>(&cell_);
    now_ = 0;
    Deps d;
    d.counters = counters_.get();
    d.hmac_key = kKey;
    d.monotonic_ms = [this]() { return now_; };
    verifier_ = chaski::pututu::NewVerifier(d);
  }

  Verdict At(std::int64_t ms, const std::string& body) {
    now_ = ms;
    return verifier_->Verify(body);
  }

  std::uint64_t stored() const { return cell_; }
  Counters& counters() { return *counters_; }
  Verifier& verifier() { return *verifier_; }

 private:
  std::uint64_t cell_ = 0;
  std::int64_t now_ = 0;
  std::unique_ptr<Counters> counters_;
  std::unique_ptr<Verifier> verifier_;
};

struct Vector {
  std::string name;
  std::string key;
  std::string token;
};

// Reads testdata/pututu.json — tokens minted by internal/pututu. Regenerate
// with `go run ./tools/pututuvectors`; never hand-edit.
std::vector<Vector> LoadVectors() {
  const std::string path = std::string(CHASKI_TESTDATA_DIR) + "/pututu.json";
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    ADD_FAILURE() << "cannot open " << path;
    return {};
  }
  std::ostringstream ss;
  ss << in.rdbuf();

  cJSON* root = cJSON_Parse(ss.str().c_str());
  if (root == nullptr) {
    ADD_FAILURE() << "cannot parse " << path;
    return {};
  }

  std::vector<Vector> out;
  const cJSON* arr = cJSON_GetObjectItemCaseSensitive(root, "vectors");
  const cJSON* item = nullptr;
  cJSON_ArrayForEach(item, arr) {
    const cJSON* name = cJSON_GetObjectItemCaseSensitive(item, "name");
    const cJSON* key_hex = cJSON_GetObjectItemCaseSensitive(item, "key_hex");
    const cJSON* token = cJSON_GetObjectItemCaseSensitive(item, "token");
    if (!cJSON_IsString(name) || !cJSON_IsString(key_hex) || !cJSON_IsString(token)) {
      ADD_FAILURE() << "malformed vector in " << path;
      continue;
    }
    Vector v;
    v.name = name->valuestring;
    v.token = token->valuestring;
    const std::string hex = key_hex->valuestring;
    for (std::size_t i = 0; i + 1 < hex.size(); i += 2) {
      v.key.push_back(static_cast<char>(std::stoul(hex.substr(i, 2), nullptr, 16)));
    }
    out.push_back(std::move(v));
  }
  cJSON_Delete(root);
  return out;
}

std::string Hex(const std::uint8_t* p, std::size_t n) {
  static const char kDigits[] = "0123456789abcdef";
  std::string s;
  for (std::size_t i = 0; i < n; ++i) {
    s.push_back(kDigits[p[i] >> 4]);
    s.push_back(kDigits[p[i] & 0x0f]);
  }
  return s;
}

// --- the primitive -----------------------------------------------------------
//
// SHA-256 and HMAC are compiled into the firmware from this project's own
// source (see components/pututu/hmac_sha256.h for why), so they are checked
// against the published vectors as well as against the server. A primitive
// that is wrong in a way the server is also wrong in would otherwise pass
// C8.ServerMintedTokensAllVerify quite happily.

TEST(C8, Sha256MatchesPublishedVectors) {
  namespace crypto = chaski::pututu::crypto;
  std::uint8_t out[crypto::kSha256DigestBytes];

  crypto::Sha256(reinterpret_cast<const std::uint8_t*>(""), 0, out);
  EXPECT_EQ(Hex(out, sizeof(out)),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");

  const std::string abc = "abc";
  crypto::Sha256(reinterpret_cast<const std::uint8_t*>(abc.data()), abc.size(), out);
  EXPECT_EQ(Hex(out, sizeof(out)),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");

  // 56 bytes: the message that lands exactly on the length-field boundary and
  // forces a second padding block.
  const std::string two_block = "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq";
  ASSERT_EQ(two_block.size(), 56u);
  crypto::Sha256(reinterpret_cast<const std::uint8_t*>(two_block.data()), two_block.size(), out);
  EXPECT_EQ(Hex(out, sizeof(out)),
            "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
}

TEST(C8, HmacSha256MatchesRfc4231Vectors) {
  namespace crypto = chaski::pututu::crypto;
  std::uint8_t out[crypto::kSha256DigestBytes];

  const std::vector<std::uint8_t> key1(20, 0x0b);
  const std::string data1 = "Hi There";
  crypto::HmacSha256(key1.data(), key1.size(),
                     reinterpret_cast<const std::uint8_t*>(data1.data()), data1.size(), out);
  EXPECT_EQ(Hex(out, sizeof(out)),
            "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7");

  const std::string key2 = "Jefe";
  const std::string data2 = "what do ya want for nothing?";
  crypto::HmacSha256(reinterpret_cast<const std::uint8_t*>(key2.data()), key2.size(),
                     reinterpret_cast<const std::uint8_t*>(data2.data()), data2.size(), out);
  EXPECT_EQ(Hex(out, sizeof(out)),
            "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843");

  // 131-byte key: the branch RFC 2104 hashes first, which no current
  // deployment exercises and a key rotation might.
  const std::vector<std::uint8_t> key6(131, 0xaa);
  const std::string data6 = "Test Using Larger Than Block-Size Key - Hash Key First";
  crypto::HmacSha256(key6.data(), key6.size(),
                     reinterpret_cast<const std::uint8_t*>(data6.data()), data6.size(), out);
  EXPECT_EQ(Hex(out, sizeof(out)),
            "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54");
}

// --- agreement with the server -----------------------------------------------

// The one test that would catch a silent drift between the two halves of the
// doorbell. Every token here was minted by internal/pututu.Token.
TEST(C8, ServerMintedTokensAllVerify) {
  const std::vector<Vector> vectors = LoadVectors();
  ASSERT_FALSE(vectors.empty());

  for (const Vector& v : vectors) {
    std::uint64_t cell = 0;
    Counters counters(&cell);
    std::int64_t now = kAfterBoot;
    Deps d;
    d.counters = &counters;
    d.hmac_key = v.key;
    d.monotonic_ms = [&now]() { return now; };
    const std::unique_ptr<Verifier> verifier = chaski::pututu::NewVerifier(d);

    const Verdict got = verifier->Verify(v.token);
    if (v.name == "zero") {
      // Counter 0 carries a correct MAC and is still refused: the high-water
      // starts at 0 and the rule is STRICTLY greater (server §10.2).
      EXPECT_EQ(got, Verdict::kStaleCounter) << v.name;
      continue;
    }
    EXPECT_EQ(got, Verdict::kAccept) << v.name;
  }
}

// The same tokens, one character of the MAC changed. Nothing about the
// device's answer may depend on how close a forgery got.
TEST(C8, ServerMintedTokensWithATamperedMacAreRejected) {
  const std::vector<Vector> vectors = LoadVectors();
  ASSERT_FALSE(vectors.empty());

  for (const Vector& v : vectors) {
    std::uint64_t cell = 0;
    Counters counters(&cell);
    std::int64_t now = kAfterBoot;
    Deps d;
    d.counters = &counters;
    d.hmac_key = v.key;
    d.monotonic_ms = [&now]() { return now; };
    const std::unique_ptr<Verifier> verifier = chaski::pututu::NewVerifier(d);

    std::string tampered = v.token;
    char& last = tampered.back();
    last = last == 'A' ? 'B' : 'A';

    EXPECT_EQ(verifier->Verify(tampered), Verdict::kBadMac) << v.name;
    EXPECT_EQ(cell, 0u) << v.name;
    EXPECT_EQ(counters.writes, 0) << v.name;
  }
}

// --- the counter -------------------------------------------------------------

TEST(C8, ValidTokenIsAccepted) {
  Fixture f;
  EXPECT_EQ(f.At(kAfterBoot, Mint(1, kKey)), Verdict::kAccept);
  EXPECT_EQ(f.stored(), 1u);
}

TEST(C8, ReplayedTokenIsStale) {
  Fixture f;
  const std::string token = Mint(7, kKey);
  ASSERT_EQ(f.At(kAfterBoot, token), Verdict::kAccept);

  // Same bytes, valid MAC, a full window later so the rate limit is not what
  // rejects it: the counter is.
  EXPECT_EQ(f.At(kAfterBoot + kMinWakeIntervalMs, token), Verdict::kStaleCounter);
  EXPECT_EQ(f.stored(), 7u);
}

TEST(C8, LowerCounterIsStaleAndHigherIsAccepted) {
  Fixture f;
  ASSERT_EQ(f.At(kAfterBoot, Mint(7, kKey)), Verdict::kAccept);
  EXPECT_EQ(f.At(kAfterBoot + kMinWakeIntervalMs, Mint(6, kKey)), Verdict::kStaleCounter);
  EXPECT_EQ(f.stored(), 7u);
  EXPECT_EQ(f.At(kAfterBoot + 2 * kMinWakeIntervalMs, Mint(8, kKey)), Verdict::kAccept);
  EXPECT_EQ(f.stored(), 8u);
}

// The rule the doorbell depends on: a counter accepted before the power went
// out is still spent afterwards. NVS keeps the cell; the verifier keeps
// nothing.
TEST(C8, CounterMonotonicitySurvivesPowerLoss) {
  Fixture f;
  ASSERT_EQ(f.At(kAfterBoot, Mint(12, kKey)), Verdict::kAccept);
  ASSERT_EQ(f.stored(), 12u);

  f.Open();  // power loss: new verifier, clock back to zero, same NVS cell

  EXPECT_EQ(f.At(kAfterBoot, Mint(12, kKey)), Verdict::kStaleCounter);
  EXPECT_EQ(f.At(kAfterBoot + kMinWakeIntervalMs, Mint(11, kKey)), Verdict::kStaleCounter);
  EXPECT_EQ(f.At(kAfterBoot + 2 * kMinWakeIntervalMs, Mint(13, kKey)), Verdict::kAccept);
  EXPECT_EQ(f.stored(), 13u);
}

// client §7: persist the high-water BEFORE acting on the accept. The caller
// acts on the returned verdict, so "before" means the write must have happened
// by the time kAccept comes back — and a write that did not happen must not
// come back as kAccept at all.
TEST(C8, HighWaterIsPersistedBeforeTheAcceptIsReturned) {
  Fixture f;
  EXPECT_EQ(f.At(kAfterBoot, Mint(5, kKey)), Verdict::kAccept);
  EXPECT_EQ(f.counters().writes, 1);
  EXPECT_EQ(f.stored(), 5u);

  f.counters().fail = true;
  EXPECT_EQ(f.At(kAfterBoot + kMinWakeIntervalMs, Mint(6, kKey)), Verdict::kNotPersisted);
  EXPECT_EQ(f.stored(), 5u);

  // And the counter it could not persist is not treated as spent: once the
  // store works again, the same doorbell is still accepted.
  f.counters().fail = false;
  EXPECT_EQ(f.At(kAfterBoot + 2 * kMinWakeIntervalMs, Mint(6, kKey)), Verdict::kAccept);
  EXPECT_EQ(f.stored(), 6u);
}

TEST(C8, CounterSeenReportsTheHighWaterForTheSyncRequest) {
  Fixture f;
  EXPECT_EQ(f.verifier().CounterSeen(), 0u);
  ASSERT_EQ(f.At(kAfterBoot, Mint(31, kKey)), Verdict::kAccept);
  // server §10.3: this is what heals a Wasi restored from backup.
  EXPECT_EQ(f.verifier().CounterSeen(), 31u);
}

// --- malformed input ---------------------------------------------------------

TEST(C8, MalformedTokensAreRejected) {
  const std::string valid = Mint(4, kKey);
  const std::string mac = valid.substr(valid.rfind('.') + 1);

  const std::vector<std::string> bad = {
      "",
      "CH1",
      "CH1.",
      "CH1.4",
      "CH1.4." + mac.substr(1),           // MAC one character short
      "CH1.4." + mac + "A",               // one too long
      "CH2.4." + mac,                     // wrong version tag
      "ch1.4." + mac,                     // the tag is case-sensitive
      " CH1.4." + mac,                    // leading space
      "CH1.4." + mac + "\n",              // trailing newline from the modem
      "CH1..4." + mac,                    // empty counter
      "CH1.-4." + mac,                    // signed
      "CH1.+4." + mac,                    // signed the other way
      "CH1.4x." + mac,                    // trailing junk in the counter
      "CH1.004." + mac,                   // non-canonical: the MACed bytes differ
      "CH1.18446744073709551616." + mac,  // one past uint64
      "CH1.99999999999999999999999." + mac,
      "Your prepaid balance is low.",  // what a carrier actually sends
  };

  for (const std::string& body : bad) {
    Fixture f;
    EXPECT_EQ(f.At(kAfterBoot, body), Verdict::kBadFormat) << "body: " << body;
    EXPECT_EQ(f.stored(), 0u) << "body: " << body;
    EXPECT_EQ(f.counters().writes, 0) << "body: " << body;
  }
}

TEST(C8, TokenMintedWithTheWrongKeyIsRejected) {
  Fixture f;
  EXPECT_EQ(f.At(kAfterBoot, Mint(4, "not-the-provisioned-key")), Verdict::kBadMac);
  EXPECT_EQ(f.stored(), 0u);
  EXPECT_EQ(f.counters().writes, 0);
}

// --- silence -----------------------------------------------------------------

// D-7 and client §7: a failure is silence. Not a quiet log line — silence. The
// verifier writes nothing anywhere, which is also why the SMS body it was
// handed cannot end up on a serial console someone is reading over a
// shoulder (B.11).
TEST(C8, NothingIsEverPrinted) {
  testing::internal::CaptureStdout();
  testing::internal::CaptureStderr();

  Fixture f;
  f.At(kAfterBoot, Mint(1, kKey));
  f.At(kAfterBoot + kMinWakeIntervalMs, Mint(1, kKey));
  f.At(kAfterBoot + 2 * kMinWakeIntervalMs, "CH1.2.AAAAAAAAAAAAAAAA");
  f.At(kAfterBoot + 3 * kMinWakeIntervalMs, "hello from a stranger");
  f.At(kAfterBoot + 3 * kMinWakeIntervalMs + 1, Mint(9, kKey));

  EXPECT_EQ(testing::internal::GetCapturedStdout(), "");
  EXPECT_EQ(testing::internal::GetCapturedStderr(), "");
}

// --- the rate limit ----------------------------------------------------------

TEST(C8, RateLimitHoldsForValidFreshTokens) {
  Fixture f;
  ASSERT_EQ(f.At(kAfterBoot, Mint(1, kKey)), Verdict::kAccept);

  // A perfectly good doorbell, one millisecond inside the window.
  EXPECT_EQ(f.At(kAfterBoot + kMinWakeIntervalMs - 1, Mint(2, kKey)), Verdict::kRateLimited);
  EXPECT_EQ(f.stored(), 1u);

  EXPECT_EQ(f.At(kAfterBoot + kMinWakeIntervalMs, Mint(2, kKey)), Verdict::kAccept);
  EXPECT_EQ(f.stored(), 2u);
}

// The limit is on wakes "regardless of validity" (server §10.2), so a message
// that would have failed validation still consumes the window — that is what
// keeps a validation bug from becoming a battery drain.
TEST(C8, RateLimitHoldsWhenValidationWouldFail) {
  Fixture f;
  EXPECT_EQ(f.At(kAfterBoot, "not a token at all"), Verdict::kBadFormat);
  EXPECT_EQ(f.At(kAfterBoot + 1, Mint(1, kKey)), Verdict::kRateLimited);
  EXPECT_EQ(f.stored(), 0u);
}

// A flood must not be able to hold the window open forever: the window is
// anchored on the last message actually processed, so one gets through every
// five minutes no matter how many arrive.
TEST(C8, FloodDoesNotExtendTheWindow) {
  Fixture f;
  ASSERT_EQ(f.At(kAfterBoot, Mint(1, kKey)), Verdict::kAccept);
  for (std::int64_t t = kAfterBoot + 1; t < kAfterBoot + kMinWakeIntervalMs; t += 1000) {
    ASSERT_EQ(f.At(t, "CH1.2.AAAAAAAAAAAAAAAA"), Verdict::kRateLimited);
  }
  EXPECT_EQ(f.At(kAfterBoot + kMinWakeIntervalMs, Mint(2, kKey)), Verdict::kAccept);
}

// client §7: after power loss the limiter starts conservative — the first
// minute after boot counts as inside the window. Keyed on the clock, so a
// device that has been up for hours is not gagged again on every wake.
TEST(C8, FirstMinuteAfterPowerLossIsInsideTheWindow) {
  Fixture f;
  EXPECT_EQ(f.At(0, Mint(1, kKey)), Verdict::kRateLimited);
  EXPECT_EQ(f.At(kBootQuietMs - 1, Mint(1, kKey)), Verdict::kRateLimited);
  EXPECT_EQ(f.stored(), 0u);
  EXPECT_EQ(f.counters().writes, 0);

  EXPECT_EQ(f.At(kBootQuietMs, Mint(1, kKey)), Verdict::kAccept);
}

// The limiter's timestamp can live in RTC memory, which is what makes it hold
// across a deep sleep the SMS itself ended (client §7). Two wakes, two
// verifiers, one shared cell — and the second wake is still refused.
TEST(C8, WakeLimiterCanBeBackedByRtcMemory) {
  std::uint64_t cell = 0;
  std::int64_t rtc_last_wake = 0;
  std::int64_t now = 0;
  Counters counters(&cell);

  auto boot = [&]() {
    Deps d;
    d.counters = &counters;
    d.hmac_key = kKey;
    d.monotonic_ms = [&now]() { return now; };
    d.load_last_wake_ms = [&rtc_last_wake]() { return rtc_last_wake; };
    d.store_last_wake_ms = [&rtc_last_wake](std::int64_t v) { rtc_last_wake = v; };
    return chaski::pututu::NewVerifier(d);
  };

  now = kAfterBoot;
  ASSERT_EQ(boot()->Verify(Mint(1, kKey)), Verdict::kAccept);
  EXPECT_EQ(rtc_last_wake, kAfterBoot);

  // A fresh verifier — deep sleep ended, the object is gone, the clock kept
  // running. An instance-local timestamp would have let this one through.
  now = kAfterBoot + 1000;
  EXPECT_EQ(boot()->Verify(Mint(2, kKey)), Verdict::kRateLimited);

  now = kAfterBoot + kMinWakeIntervalMs;
  EXPECT_EQ(boot()->Verify(Mint(2, kKey)), Verdict::kAccept);
}

// A missing dependency must fail shut, never open: no clock means no way to
// honour the limit, so nothing is accepted. Costing the doorbell is the
// tolerable half of that trade; costing the battery is not.
TEST(C8, MissingDependenciesFailShut) {
  std::uint64_t cell = 0;
  Counters counters(&cell);

  Deps no_clock;
  no_clock.counters = &counters;
  no_clock.hmac_key = kKey;
  EXPECT_EQ(chaski::pututu::NewVerifier(no_clock)->Verify(Mint(1, kKey)), Verdict::kRateLimited);

  std::int64_t now = kAfterBoot;
  Deps no_store;
  no_store.hmac_key = kKey;
  no_store.monotonic_ms = [&now]() { return now; };
  const std::unique_ptr<Verifier> v = chaski::pututu::NewVerifier(no_store);
  EXPECT_EQ(v->Verify(Mint(1, kKey)), Verdict::kNotPersisted);
  EXPECT_EQ(v->CounterSeen(), 0u);
}

}  // namespace
