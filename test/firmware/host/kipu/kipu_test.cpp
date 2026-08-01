// Host tests for components/kipu: the tier-1 health block and the ring the
// settings screen reads back to the child (client §13, §11.7, server §4.8).
//
// No C-number: the C-table has no kipu row, so these carry the component name.
// What they hold to is the two promises the child is owed — that the block is
// health only, and that it is small enough to actually be sent.
#include "chaski/kipu.h"

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "cJSON.h"
#include "chaski/wire.h"

namespace {

using chaski::kipu::Entry;
using chaski::kipu::kLogCapacity;

// KipuJson pulls the block back out of an encoded sync request, which is the
// only place its serialised size is decided. Empty means the encoder dropped
// it for exceeding the cap (server §4.8) — a silent loss of telemetry that
// Build exists to make impossible.
std::string KipuJson(const chaski::wire::Kipu& k) {
  chaski::wire::Request r;
  r.kipu = k;
  const std::string encoded = chaski::wire::EncodeRequest(r);

  cJSON* root = cJSON_Parse(encoded.c_str());
  if (root == nullptr) {
    ADD_FAILURE() << "request did not parse";
    return "";
  }
  cJSON* block = cJSON_GetObjectItemCaseSensitive(root, "kipu");
  std::string out;
  if (block != nullptr) {
    char* text = cJSON_PrintUnformatted(block);
    if (text != nullptr) {
      out = text;
      cJSON_free(text);
    }
  }
  cJSON_Delete(root);
  return out;
}

Entry MakeEntry(std::int64_t at, int battery) {
  Entry e;
  e.at = at;
  e.block = chaski::kipu::Build(battery, false, "ltem", -97, 0, "0.1.0");
  return e;
}

// --- the block ---------------------------------------------------------------

TEST(Kipu, BuildCarriesTheTier1FieldsThrough) {
  const chaski::wire::Kipu k = chaski::kipu::Build(64, true, "ltem", -97, 1, "0.1.0");
  EXPECT_EQ(k.battery_pct, 64);
  EXPECT_TRUE(k.charging);
  EXPECT_EQ(k.rat, "ltem");
  EXPECT_EQ(k.rssi, -97);
  EXPECT_EQ(k.queue_depth, 1);
  EXPECT_EQ(k.fw, "0.1.0");
}

// The block is capped at 512 bytes and an oversized one is DROPPED by the
// encoder rather than failing the sync (server §4.8) — so a caller passing
// nonsense would cost the whole kipu, silently, and the settings screen would
// go blank with nothing to explain it. Build clamps so that cannot happen.
TEST(Kipu, WorstCaseBlockStaysUnderTheWireCap) {
  const chaski::wire::Kipu k =
      chaski::kipu::Build(999999, true, std::string(4096, 'r'), -999999, 999999999,
                          std::string(4096, 'v'));

  const std::string json = KipuJson(k);
  ASSERT_FALSE(json.empty()) << "the encoder dropped the block";
  EXPECT_LT(json.size(), static_cast<std::size_t>(chaski::wire::kMaxKipuBytes));

  EXPECT_EQ(k.battery_pct, 100);
  EXPECT_GE(k.rssi, -199);
  EXPECT_LE(k.queue_depth, 9999);
}

// A modem returning a UTF-8 name, or a build id someone pasted, must not reach
// the encoder as a truncated multi-byte sequence: these fields are machine
// identifiers, and anything else is dropped rather than cut in half.
TEST(Kipu, NonAsciiFieldsAreNotTruncatedIntoInvalidJson) {
  const chaski::wire::Kipu k = chaski::kipu::Build(50, false, "lte\xc3\xa9m", -80, 0, "0.1.0-caf\xc3\xa9");
  EXPECT_EQ(k.rat, "ltem");
  EXPECT_EQ(k.fw, "0.1.0-caf");
  EXPECT_FALSE(KipuJson(k).empty());
}

// v1 is health only (server §4.8, client §13). This asserts the block's shape
// exactly: a new field here means someone added telemetry, which is a spec
// decision, not a patch.
TEST(Kipu, BlockIsHealthOnly) {
  const chaski::wire::Kipu k = chaski::kipu::Build(64, false, "ltem", -97, 1, "0.1.0");
  cJSON* block = cJSON_Parse(KipuJson(k).c_str());
  ASSERT_NE(block, nullptr);

  std::vector<std::string> keys;
  const cJSON* field = nullptr;
  cJSON_ArrayForEach(field, block) { keys.push_back(field->string); }
  cJSON_Delete(block);

  const std::vector<std::string> want = {"battery_pct", "charging", "fw",
                                         "queue_depth", "rat",      "rssi"};
  EXPECT_EQ(keys, want);
}

// --- the readable log --------------------------------------------------------

TEST(Kipu, LogReturnsTheMostRecentEntriesNewestFirst) {
  const std::unique_ptr<chaski::kipu::Log> log = chaski::kipu::NewLog();
  EXPECT_TRUE(log->Recent(5).empty());

  for (int i = 1; i <= 3; ++i) log->Record(MakeEntry(i, 50 + i));

  const std::vector<Entry> recent = log->Recent(5);
  ASSERT_EQ(recent.size(), 3u);
  EXPECT_EQ(recent[0].at, 3);
  EXPECT_EQ(recent[1].at, 2);
  EXPECT_EQ(recent[2].at, 1);
  EXPECT_EQ(recent[0].block.battery_pct, 53);

  const std::vector<Entry> two = log->Recent(2);
  ASSERT_EQ(two.size(), 2u);
  EXPECT_EQ(two[0].at, 3);
  EXPECT_EQ(two[1].at, 2);

  EXPECT_TRUE(log->Recent(0).empty());
}

// The ring is short on purpose: the kipu is designed to be forgotten (client
// §13, A.6). What the screen can show is all the device keeps, so the oldest
// syncs fall off rather than accumulating into a history.
TEST(Kipu, LogBoundsAtCapacityAndDropsTheOldest) {
  const std::unique_ptr<chaski::kipu::Log> log = chaski::kipu::NewLog();
  const std::size_t overfill = kLogCapacity * 2 + 3;
  for (std::size_t i = 1; i <= overfill; ++i) {
    log->Record(MakeEntry(static_cast<std::int64_t>(i), 50));
  }

  const std::vector<Entry> all = log->Recent(overfill);
  ASSERT_EQ(all.size(), kLogCapacity);
  EXPECT_EQ(all.front().at, static_cast<std::int64_t>(overfill));
  EXPECT_EQ(all.back().at, static_cast<std::int64_t>(overfill - kLogCapacity + 1));

  for (std::size_t i = 1; i < all.size(); ++i) {
    EXPECT_LT(all[i].at, all[i - 1].at) << "entry " << i;
  }
}

// The ring holds exactly what was sent, not a summary of it: the settings
// screen's promise is "what my Chaski tells home" (design §3.7), and a
// rounded or re-derived number would make that a different sentence.
TEST(Kipu, LogKeepsTheBlockAsItWasSent) {
  const std::unique_ptr<chaski::kipu::Log> log = chaski::kipu::NewLog();
  Entry e;
  e.at = 1700000000;
  e.block = chaski::kipu::Build(64, true, "nbiot", -103, 2, "0.2.1");
  log->Record(e);

  const std::vector<Entry> recent = log->Recent(1);
  ASSERT_EQ(recent.size(), 1u);
  EXPECT_EQ(recent[0].at, 1700000000);
  EXPECT_EQ(recent[0].block.battery_pct, 64);
  EXPECT_TRUE(recent[0].block.charging);
  EXPECT_EQ(recent[0].block.rat, "nbiot");
  EXPECT_EQ(recent[0].block.rssi, -103);
  EXPECT_EQ(recent[0].block.queue_depth, 2);
  EXPECT_EQ(recent[0].block.fw, "0.2.1");
}

}  // namespace
