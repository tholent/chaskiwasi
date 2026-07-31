// The sync engine against the real file-backed stores, not the doubles.
//
// The unit suite proves the §5.2 order; this one proves the order still holds
// when the state it writes has to survive being closed and reopened — which is
// what a power cycle looks like from above the seam. Everything here goes
// through store::Open* and ayllu::Open on a temp directory, the same code path
// that runs on LittleFS (store.h's portability note).
#include <gtest/gtest.h>

#include <cstdlib>
#include <filesystem>
#include <memory>
#include <string>
#include <vector>

#include "chaski/ayllu.h"
#include "chaski/store.h"
#include "chaski/syncengine.h"
#include "fakes.h"

using chaski::syncengine::Fault;
using chaski::syncengine::Options;
using chaski::syncengine::Outcome;
using chaski::syncengine::Trigger;
using chaski::testing::FakeClock;
using chaski::testing::FakeTransport;
using chaski::testing::Ok;

namespace {

// Device is one power-on: the stores as they exist on flash, plus an engine
// wired to them. Destroying it and building another over the same root is a
// reboot.
struct Device {
  explicit Device(const std::string& root)
      : letters(chaski::store::OpenLetterStore(root)),
        outbox(chaski::store::OpenOutbox(root)),
        seen(chaski::store::OpenSeenRing(root, chaski::store::kMinSeenIds)),
        state(chaski::store::OpenStateStore(root)),
        contacts(chaski::ayllu::Open(root)) {}

  std::unique_ptr<chaski::store::LetterStore> letters;
  std::unique_ptr<chaski::store::Outbox> outbox;
  std::unique_ptr<chaski::store::SeenRing> seen;
  std::unique_ptr<chaski::store::StateStore> state;
  std::unique_ptr<chaski::ayllu::Store> contacts;
  FakeTransport transport;
  FakeClock clock;

  std::unique_ptr<chaski::syncengine::Engine> Engine() {
    chaski::syncengine::Deps d;
    d.transport = &transport;
    d.letters = letters.get();
    d.outbox = outbox.get();
    d.seen = seen.get();
    d.state = state.get();
    d.contacts = contacts.get();
    d.clock = &clock;
    return chaski::syncengine::New(d, Options{});
  }
};

class SyncEngineFiles : public ::testing::Test {
 protected:
  void SetUp() override {
    std::string tmpl = (std::filesystem::temp_directory_path() / "chaski-sync-XXXXXX").string();
    std::vector<char> buf(tmpl.begin(), tmpl.end());
    buf.push_back('\0');
    ASSERT_NE(::mkdtemp(buf.data()), nullptr);
    root_ = buf.data();
  }

  void TearDown() override {
    std::error_code ec;
    std::filesystem::remove_all(root_, ec);
  }

  const std::string& root() const { return root_; }

 private:
  std::string root_;
};

constexpr const char* kResponse =
    "{\"server_time\":1785420202,\"cursor\":\"cursor-1\",\"pututu_counter\":41,\"more\":false,"
    "\"letters\":[{\"id\":\"l-aaa\",\"contact_id\":\"c_02\",\"subject\":\"camping\","
    "\"date\":1785349200,\"body\":\"we went up to the lake\",\"trimmed\":false,"
    "\"truncated\":false,\"degraded\":false},"
    "{\"id\":\"l-bbb\",\"contact_id\":\"c_07\",\"subject\":\"hola\",\"date\":1785349900,"
    "\"body\":\"segunda carta\",\"trimmed\":false,\"truncated\":false,\"degraded\":false}],"
    "\"ayllu\":{\"version\":8,\"contacts\":[{\"id\":\"c_02\",\"name\":\"Abuela\",\"active\":true,"
    "\"pinned\":true,\"order\":0,\"portrait\":\"p01\"},{\"id\":\"c_07\",\"name\":\"Rosa\","
    "\"active\":false,\"pinned\":false,\"order\":3,\"portrait\":\"p04\"}]}}";

}  // namespace

// A sync, then a reboot, then the server re-delivering everything it already
// sent: the cursor and the ring are on flash, so the second pass stores
// nothing and loses nothing (server §4.4, §4.5).
TEST_F(SyncEngineFiles, StateSurvivesARebootAndAbsorbsRedelivery) {
  {
    Device dev(root());
    dev.transport.responder = [](int, const std::string&) { return Ok(kResponse); };
    auto engine = dev.Engine();

    const Outcome out = engine->RunOnce(Trigger::kUserKey);
    EXPECT_EQ(out.fault, Fault::kNone);
    EXPECT_EQ(out.letters_stored, 2);
    EXPECT_EQ(dev.state->Snapshot().cursor, "cursor-1");
  }

  Device rebooted(root());
  EXPECT_EQ(rebooted.state->Snapshot().cursor, "cursor-1");
  EXPECT_EQ(rebooted.letters->Count(), 2u);
  EXPECT_EQ(rebooted.contacts->Version(), 8);

  // The tombstone still resolves for reading old letters and stays out of the
  // picker (server §7.2).
  chaski::ayllu::Contact rosa;
  ASSERT_TRUE(rebooted.contacts->Lookup("c_07", rosa));
  EXPECT_EQ(rosa.name, "Rosa");
  EXPECT_FALSE(rosa.active);

  rebooted.transport.responder = [](int, const std::string&) { return Ok(kResponse); };
  auto engine = rebooted.Engine();
  const Outcome again = engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(again.letters_stored, 0);
  EXPECT_EQ(again.letters_deduped, 2);
  EXPECT_EQ(rebooted.letters->Count(), 2u);

  // The request it just sent echoed the stored cursor verbatim.
  ASSERT_FALSE(rebooted.transport.requests.empty());
  EXPECT_NE(rebooted.transport.requests[0].find("cursor-1"), std::string::npos);
}

// A letter written before the first sync goes out on the wire, and the ack
// takes it off the device for good — across a reboot, since an outbox that
// forgot an ack would send it again (D-5).
TEST_F(SyncEngineFiles, AckedOutboundDoesNotSurviveAReboot) {
  std::string local_id;
  {
    Device dev(root());
    ASSERT_TRUE(dev.outbox->Add("c_02", "camping", "we went to the lake", 0, local_id));
    ASSERT_FALSE(local_id.empty());

    dev.transport.responder = [&](int, const std::string&) {
      return Ok("{\"server_time\":1785420202,\"cursor\":\"cursor-1\",\"more\":false,"
                "\"acks\":[{\"local_id\":\"" +
                local_id + "\",\"status\":\"sent\"}]}");
    };
    auto engine = dev.Engine();
    const Outcome out = engine->RunOnce(Trigger::kUserKey);

    EXPECT_EQ(out.acks_applied, 1);
    ASSERT_FALSE(dev.transport.requests.empty());
    EXPECT_NE(dev.transport.requests[0].find(local_id), std::string::npos);
  }

  Device rebooted(root());
  EXPECT_EQ(rebooted.outbox->SendableCount(), 0u);
  EXPECT_TRUE(rebooted.outbox->All().empty());
}

// A reject is terminal too, and the text has to still be there after a reboot:
// one key re-opens it as a draft (§5.4).
TEST_F(SyncEngineFiles, RejectedOutboundKeepsItsTextAcrossAReboot) {
  const std::string body = "the lake was very cold";
  std::string local_id;
  {
    Device dev(root());
    ASSERT_TRUE(dev.outbox->Add("c_07", "camping", body, 0, local_id));
    dev.transport.responder = [&](int, const std::string&) {
      return Ok("{\"server_time\":1785420202,\"cursor\":\"cursor-1\",\"more\":false,"
                "\"acks\":[{\"local_id\":\"" +
                local_id + "\",\"status\":\"rejected_inactive\"}]}");
    };
    auto engine = dev.Engine();
    engine->RunOnce(Trigger::kUserKey);
  }

  Device rebooted(root());
  const std::vector<chaski::store::OutboxEntry> all = rebooted.outbox->All();
  ASSERT_EQ(all.size(), 1u);
  EXPECT_EQ(all[0].body, body);
  EXPECT_TRUE(all[0].rejected);
  EXPECT_EQ(all[0].reject_status, chaski::wire::AckStatus::kRejectedInactive);
  EXPECT_EQ(rebooted.outbox->SendableCount(), 0u);

  // And it is not on the wire again, ever.
  rebooted.transport.responder = [](int, const std::string&) {
    return Ok("{\"cursor\":\"cursor-2\",\"more\":false}");
  };
  auto engine = rebooted.Engine();
  engine->RunOnce(Trigger::kScheduled);
  ASSERT_FALSE(rebooted.transport.requests.empty());
  EXPECT_EQ(rebooted.transport.requests[0].find(body), std::string::npos);
}

// The cursor is written last, so a reboot before it re-runs the whole response.
// On real files that means the letters are already there and only the cursor is
// behind — the case §5.2 is built around.
TEST_F(SyncEngineFiles, CursorNeverLandsAheadOfTheLetters) {
  {
    Device dev(root());
    chaski::wire::Response r;
    ASSERT_TRUE(chaski::wire::DecodeResponse(kResponse, r));

    chaski::syncengine::Deps d;
    d.transport = &dev.transport;
    d.letters = dev.letters.get();
    d.outbox = dev.outbox.get();
    d.seen = dev.seen.get();
    d.state = dev.state.get();
    d.contacts = dev.contacts.get();
    d.clock = &dev.clock;
    // Stand at the boundary of step 6 and look at the flash: the letters are
    // already there and the cursor is still the old one. That is the whole
    // safety argument, observed rather than asserted about.
    d.fault_hook = [&](int step) {
      if (step != 6) return;
      EXPECT_EQ(dev.letters->Count(), 2u);
      EXPECT_TRUE(dev.seen->Contains("l-aaa"));
      EXPECT_EQ(dev.state->Snapshot().cursor, "");
    };

    auto engine = chaski::syncengine::New(d, Options{});
    engine->ApplyResponse(r);
    EXPECT_EQ(dev.state->Snapshot().cursor, "cursor-1");
  }

  Device rebooted(root());
  EXPECT_EQ(rebooted.letters->Count(), 2u);
  EXPECT_TRUE(rebooted.seen->Contains("l-aaa"));
  EXPECT_TRUE(rebooted.seen->Contains("l-bbb"));
}
