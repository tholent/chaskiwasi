// Host tests for components/syncengine.
//
// What this suite is for: the §5.2 apply order is a crash-safety argument, and
// an argument that is never executed is a hope. Every numbered step is cut at,
// in turn, and the recovery asserted — no letter lost, no letter sent twice,
// the seen-ring never ahead of the letter store (C-4's host half; the bench
// half cuts the real rail). Around that sit the terminal-ack rules (C-3), the
// drain cap (C-6), clock validity (C-21), dedup, and the status-to-fault
// mapping the child sees as four distinct, honest screens (§11.6).
#include <gtest/gtest.h>

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

#include "chaski/syncengine.h"
#include "fakes.h"

using chaski::syncengine::Fault;
using chaski::syncengine::Options;
using chaski::syncengine::Outcome;
using chaski::syncengine::Trigger;
using chaski::testing::Ok;
using chaski::testing::Rig;
using chaski::testing::Status;
using chaski::testing::TlsTrustFail;
using chaski::testing::TransportFail;
using chaski::wire::AckStatus;

namespace {

std::string LetterJson(const std::string& id, const std::string& contact_id = "c_02",
                       const std::string& body = "hola", std::int64_t date = 1785349200) {
  return "{\"id\":\"" + id + "\",\"contact_id\":\"" + contact_id +
         "\",\"subject\":\"s\",\"date\":" + std::to_string(date) + ",\"body\":\"" + body +
         "\",\"trimmed\":false,\"truncated\":false,\"degraded\":false}";
}

std::string AckJson(const std::string& local_id, const std::string& status) {
  return "{\"local_id\":\"" + local_id + "\",\"status\":\"" + status + "\"}";
}

// ResponseJson assembles a 200 body. `extra` carries whole top-level members
// (letters, acks, ayllu, config), each already comma-prefixed.
std::string ResponseJson(const std::string& cursor, const std::string& extra = "",
                         bool more = false, std::int64_t server_time = 1785420202) {
  return "{\"server_time\":" + std::to_string(server_time) + ",\"cursor\":\"" + cursor +
         "\",\"pututu_counter\":41,\"more\":" + (more ? "true" : "false") + extra + "}";
}

constexpr const char* kAylluBlock =
    ",\"ayllu\":{\"version\":8,\"contacts\":[{\"id\":\"c_02\",\"name\":\"Abuela\","
    "\"active\":true,\"pinned\":true,\"order\":0,\"portrait\":\"p01\"}]}";

constexpr const char* kConfigBlock =
    ",\"config\":{\"max_letter_chars\":300,\"sync_interval_s\":3600,\"rat\":\"ltem\"}";

// The response the C-4 sweep uses: one of every durable thing a step owns.
std::string FullResponse() {
  return ResponseJson("cursor-new", ",\"acks\":[" + AckJson("o-1", "sent") + "],\"letters\":[" +
                                        LetterJson("l-aaa") + "," + LetterJson("l-bbb") + "]" +
                                        kAylluBlock + kConfigBlock);
}

chaski::wire::Response Decoded(const std::string& json) {
  chaski::wire::Response r;
  EXPECT_TRUE(chaski::wire::DecodeResponse(json, r)) << json;
  return r;
}

}  // namespace

// ------------------------------------------------------- C-3: terminal acks

// `sent` means the letter is gone from the outbox for good. The server holds no
// outbound queue (server §4.7), so a device that kept it would send it again.
TEST(C3, SentAckRemovesTheLetterFromTheOutbox) {
  Rig rig;
  rig.outbox.Seed("o-1", "c_02", "camping", "we went to the lake");
  auto engine = rig.Engine();

  const Outcome out =
      engine->ApplyResponse(Decoded(ResponseJson("c1", ",\"acks\":[" + AckJson("o-1", "sent") + "]")));

  EXPECT_EQ(out.acks_applied, 1);
  EXPECT_TRUE(rig.outbox.entries.empty());
  EXPECT_EQ(rig.outbox.SendableCount(), 0u);
}

// Every reject keeps the child's text. The letter moves to "couldn't send" and
// one key re-opens it as a draft (§5.4) — vanishing it would be the device
// deciding what a child wrote was not worth keeping.
TEST(C3, EveryRejectIsTerminalAndKeepsTheText) {
  const char* const rejects[] = {"rejected_inactive", "rejected_unknown_contact", "invalid",
                                 "rejected_undeliverable", "some_future_status"};
  const AckStatus expected[] = {AckStatus::kRejectedInactive, AckStatus::kRejectedUnknownContact,
                                AckStatus::kInvalid, AckStatus::kRejectedUndeliverable,
                                AckStatus::kUnknown};

  for (std::size_t i = 0; i < sizeof(rejects) / sizeof(rejects[0]); ++i) {
    Rig rig;
    const std::string body = "the lake was cold";
    rig.outbox.Seed("o-1", "c_07", "camping", body);
    rig.transport.responder = [&](int, const std::string&) {
      return Ok(ResponseJson("c1", ",\"acks\":[" + AckJson("o-1", rejects[i]) + "]"));
    };
    auto engine = rig.Engine();

    const Outcome out = engine->RunOnce(Trigger::kScheduled);
    EXPECT_EQ(out.fault, Fault::kNone);
    EXPECT_EQ(out.acks_applied, 1);

    const chaski::store::OutboxEntry* e = rig.outbox.Find("o-1");
    ASSERT_NE(e, nullptr) << rejects[i];
    EXPECT_EQ(e->body, body) << rejects[i];
    EXPECT_EQ(e->subject, "camping") << rejects[i];
    EXPECT_TRUE(e->rejected) << rejects[i];
    EXPECT_EQ(e->reject_status, expected[i]) << rejects[i];
    EXPECT_TRUE(chaski::wire::AckIsReject(e->reject_status)) << rejects[i];

    // Terminal means terminal: the next request carries nothing for it.
    EXPECT_TRUE(engine->BuildRequest().outbound.empty()) << rejects[i];
  }
}

// The server may repeat any ack — its ring answers a replayed sync with the
// same outcome (server §4.7) — so applying one twice must change nothing.
TEST(C3, ReplayedAcksAreIdempotent) {
  Rig rig;
  rig.outbox.Seed("o-1", "c_02", "s", "sent one");
  rig.outbox.Seed("o-2", "c_07", "s", "rejected one");
  const std::string body = ResponseJson("c1", ",\"acks\":[" + AckJson("o-1", "sent") + "," +
                                                  AckJson("o-2", "invalid") + "]");
  rig.transport.responder = [&](int, const std::string&) { return Ok(body); };
  auto engine = rig.Engine();

  engine->RunOnce(Trigger::kScheduled);
  const std::size_t after_first = rig.outbox.entries.size();
  engine->RunOnce(Trigger::kScheduled);

  EXPECT_EQ(rig.outbox.entries.size(), after_first);
  EXPECT_EQ(rig.outbox.entries.size(), 1u);
  ASSERT_NE(rig.outbox.Find("o-2"), nullptr);
  EXPECT_EQ(rig.outbox.Find("o-2")->body, "rejected one");
  EXPECT_EQ(rig.outbox.Find("o-1"), nullptr);
}

// ------------------------------------------- C-4: the §5.2 order under a cut

// The hook exists so a test can stand at every boundary of §5.2. If a step is
// ever added, removed, or reordered, this is the assertion that notices.
TEST(C4, FaultHookFiresAtEveryStepInOrder) {
  Rig rig;
  rig.outbox.Seed("o-1", "c_02", "s", "b");
  auto engine = rig.Engine();
  engine->ApplyResponse(Decoded(FullResponse()));

  const std::vector<int> want = {1, 2, 3, 4, 5, 6};
  EXPECT_EQ(rig.steps, want);
}

// The sweep: cut the rail at each numbered step and assert what §5.2 promises —
// the old cursor stands, nothing is half-applied, and the seen-ring never gets
// ahead of the letter store.
TEST(C4, PowerCutAtEachStepLeavesConsistentState) {
  for (int cut = 1; cut <= 6; ++cut) {
    Rig rig;
    rig.state.state.cursor = "cursor-old";
    rig.outbox.Seed("o-1", "c_02", "s", "b");
    rig.cut_at_step = cut;
    auto engine = rig.Engine();

    const Outcome out = engine->ApplyResponse(Decoded(FullResponse()));
    EXPECT_TRUE(out.apply_incomplete) << "cut at step " << cut;

    // Step 6 is last for exactly this reason: the server re-delivers whatever
    // the cut interrupted, and the ring absorbs the repeats.
    EXPECT_EQ(rig.state.state.cursor, "cursor-old") << "cut at step " << cut;
    EXPECT_EQ(rig.state.cursor_writes, 0) << "cut at step " << cut;

    // An id in the ring with no letter behind it would be a letter lost for
    // good: the server would never send it again.
    for (const std::string& id : rig.seen.ids) {
      EXPECT_TRUE(rig.letters.Has(id)) << "cut at step " << cut << ", ring id " << id;
    }

    // What landed is exactly the steps before the cut.
    const bool acks_applied = cut > 2;
    const bool ayllu_applied = cut > 3;
    const bool letters_applied = cut > 4;
    EXPECT_EQ(rig.outbox.Find("o-1") == nullptr, acks_applied) << "cut at step " << cut;
    EXPECT_EQ(rig.contacts.snapshots_applied, ayllu_applied ? 1 : 0) << "cut at step " << cut;
    EXPECT_EQ(rig.letters.Count(), letters_applied ? 2u : 0u) << "cut at step " << cut;
  }
}

// The other half of C-4: after the cut the same response is delivered again and
// the device converges — every letter present exactly once, the ack not
// re-applied into a second send, the cursor finally advanced.
TEST(C4, RecoveryAfterACutLosesNothingAndDuplicatesNothing) {
  for (int cut = 1; cut <= 6; ++cut) {
    Rig rig;
    rig.state.state.cursor = "cursor-old";
    rig.outbox.Seed("o-1", "c_02", "s", "b");
    rig.cut_at_step = cut;
    auto engine = rig.Engine();
    engine->ApplyResponse(Decoded(FullResponse()));

    // Power returns; the server re-delivers because the cursor never moved.
    rig.cut_at_step = 0;
    rig.flash.cut = false;
    const Outcome out = engine->ApplyResponse(Decoded(FullResponse()));

    EXPECT_FALSE(out.apply_incomplete) << "cut at step " << cut;
    EXPECT_EQ(rig.state.state.cursor, "cursor-new") << "cut at step " << cut;
    EXPECT_EQ(rig.letters.Count(), 2u) << "cut at step " << cut;
    EXPECT_TRUE(rig.letters.Has("l-aaa")) << "cut at step " << cut;
    EXPECT_TRUE(rig.letters.Has("l-bbb")) << "cut at step " << cut;
    EXPECT_TRUE(rig.outbox.entries.empty()) << "cut at step " << cut;
    EXPECT_EQ(rig.contacts.Version(), 8) << "cut at step " << cut;
  }
}

// A cut inside step 4, between two letters: the first is written and ringed,
// the second is not, and the redelivery finishes the job without writing the
// first one twice.
TEST(C4, CutBetweenTwoLettersResumesWithoutDuplicating) {
  Rig rig;
  rig.state.state.cursor = "cursor-old";
  rig.letters.put_budget = 1;  // the rail drops after the first letter lands
  const chaski::wire::Response r = Decoded(ResponseJson(
      "cursor-new", ",\"letters\":[" + LetterJson("l-aaa") + "," + LetterJson("l-bbb") + "]"));
  auto engine = rig.Engine();

  const Outcome first = engine->ApplyResponse(r);
  EXPECT_TRUE(first.apply_incomplete);
  EXPECT_EQ(first.letters_stored, 1);
  EXPECT_TRUE(rig.letters.Has("l-aaa"));
  EXPECT_FALSE(rig.letters.Has("l-bbb"));
  EXPECT_EQ(rig.state.state.cursor, "cursor-old");

  rig.letters.put_budget = -1;
  const Outcome second = engine->ApplyResponse(r);
  EXPECT_FALSE(second.apply_incomplete);
  EXPECT_EQ(second.letters_deduped, 1);  // l-aaa was already in the ring
  EXPECT_EQ(second.letters_stored, 1);
  EXPECT_EQ(rig.letters.Count(), 2u);
  EXPECT_EQ(rig.state.state.cursor, "cursor-new");
}

// An interrupted apply must not drain: the cursor did not move, so the next
// round would only re-fetch what was just refused.
TEST(C4, AnIncompleteApplyStopsTheDrain) {
  Rig rig;
  rig.cut_at_step = 6;
  rig.transport.responder = [](int, const std::string&) {
    return Ok(ResponseJson("c1", ",\"letters\":[" + LetterJson("l-aaa") + "]", true));
  };
  auto engine = rig.Engine();

  const Outcome out = engine->RunOnce(Trigger::kScheduled);
  EXPECT_TRUE(out.apply_incomplete);
  EXPECT_EQ(rig.transport.requests.size(), 1u);
}

// ------------------------------------------------------- C-6: the drain cap

// more:true forever is a server bug, and the device's answer is to stop at ten
// rounds rather than spend a bill's worth of LTE on it (server §4.6).
TEST(C6, DrainCapsAtTenRounds) {
  Rig rig;
  rig.transport.responder = [](int round, const std::string&) {
    const std::string id = "l-" + std::to_string(round);
    return Ok(ResponseJson("cursor-" + std::to_string(round),
                           ",\"letters\":[" + LetterJson(id) + "]", true));
  };
  auto engine = rig.Engine();

  const Outcome out = engine->RunOnce(Trigger::kScheduled);

  EXPECT_EQ(out.rounds, 10);
  EXPECT_EQ(rig.transport.requests.size(), 10u);
  EXPECT_TRUE(out.more);  // still more up there; the next sync continues
  EXPECT_EQ(out.letters_stored, 10);
  EXPECT_EQ(rig.state.state.cursor, "cursor-9");
}

// The cap is configurable downward, but the default is the spec's ten.
TEST(C6, DrainCapIsConfigurable) {
  Rig rig;
  rig.transport.responder = [](int round, const std::string&) {
    return Ok(ResponseJson("cursor-" + std::to_string(round), "", true));
  };
  Options o;
  o.max_drain_rounds = 3;
  auto engine = rig.Engine(o);

  EXPECT_EQ(engine->RunOnce(Trigger::kScheduled).rounds, 3);
  EXPECT_EQ(rig.transport.requests.size(), 3u);
}

// A drain is ONE sync event. The doorbell's skip-if-recently-synced check
// (server §4.6, §10.1) would otherwise count a busy sync as ten.
TEST(C6, DrainCountsAsOneSyncEvent) {
  Rig rig;
  rig.transport.responder = [](int round, const std::string&) {
    return Ok(ResponseJson("cursor-" + std::to_string(round), "", round < 9));
  };
  auto engine = rig.Engine();

  const Outcome out = engine->RunOnce(Trigger::kPututu);

  EXPECT_EQ(out.rounds, 10);
  EXPECT_FALSE(out.more);
  EXPECT_EQ(rig.state.last_sync_writes, 1);
}

// Each round echoes the cursor the previous round returned — the device never
// invents one and never parses one (server §4.4).
TEST(C6, EachDrainRoundEchoesThePreviousCursor) {
  Rig rig;
  rig.state.state.cursor = "cursor-start";
  rig.transport.responder = [](int round, const std::string&) {
    return Ok(ResponseJson("cursor-" + std::to_string(round), "", round < 1));
  };
  auto engine = rig.Engine();
  engine->RunOnce(Trigger::kScheduled);

  ASSERT_EQ(rig.transport.requests.size(), 2u);
  EXPECT_NE(rig.transport.requests[0].find("cursor-start"), std::string::npos);
  EXPECT_NE(rig.transport.requests[1].find("cursor-0"), std::string::npos);
}

// ------------------------------------------------------- C-21: clock truth

// The device has no RTC. Until a sync says otherwise the clock is invalid, and
// a date rendered from an invalid clock would be a confident lie (§5.6).
TEST(C21, ClockStaysInvalidUntilTheServerSaysOtherwise) {
  Rig rig;
  EXPECT_FALSE(rig.clock.Valid());
  EXPECT_EQ(rig.clock.NowEpoch(), 0);

  // A response with no server_time must not validate the clock at epoch zero.
  rig.transport.responder = [](int, const std::string&) {
    return Ok("{\"cursor\":\"c1\",\"more\":false}");
  };
  auto engine = rig.Engine();
  engine->RunOnce(Trigger::kScheduled);

  EXPECT_FALSE(rig.clock.Valid());
  EXPECT_EQ(rig.clock.disciplined, 0);
  EXPECT_EQ(rig.state.last_sync_writes, 0);
  EXPECT_EQ(rig.state.state.last_sync_at, 0);
}

TEST(C21, ServerTimeDisciplinesTheClock) {
  Rig rig;
  rig.transport.responder = [](int, const std::string&) {
    return Ok(ResponseJson("c1", "", false, 1785420202));
  };
  auto engine = rig.Engine();
  engine->RunOnce(Trigger::kScheduled);

  EXPECT_TRUE(rig.clock.Valid());
  EXPECT_EQ(rig.clock.NowEpoch(), 1785420202);
  EXPECT_EQ(rig.state.state.last_sync_at, 1785420202);
}

// A letter's date is the server's, never the device's: an invalid clock changes
// nothing about what a letter says it is.
TEST(C21, LetterDatesComeFromTheWireNotTheDeviceClock) {
  Rig rig;
  auto engine = rig.Engine();
  engine->ApplyResponse(Decoded(ResponseJson(
      "c1", ",\"letters\":[" + LetterJson("l-aaa", "c_02", "hola", 1785349200) + "]", false, 0)));

  chaski::store::StoredLetter got;
  ASSERT_TRUE(rig.letters.Get("l-aaa", got));
  EXPECT_EQ(got.letter.date, 1785349200);
  EXPECT_FALSE(rig.clock.Valid());
}

// ------------------------------------------------------------------ dedup

// The server MAY re-send any letter at any time and correctness never depends
// on it not doing so (server §4.5).
TEST(SyncEngine, RedeliveredLetterIsDroppedSilently) {
  Rig rig;
  const std::string body =
      ResponseJson("c1", ",\"letters\":[" + LetterJson("l-aaa") + "," + LetterJson("l-bbb") + "]");
  rig.transport.responder = [&](int, const std::string&) { return Ok(body); };
  auto engine = rig.Engine();

  const Outcome first = engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(first.letters_stored, 2);
  EXPECT_EQ(first.letters_deduped, 0);

  const Outcome second = engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(second.letters_stored, 0);
  EXPECT_EQ(second.letters_deduped, 2);
  EXPECT_EQ(rig.letters.Count(), 2u);
}

// Eviction removes the letter file and not the ring entry, so a letter that
// aged out of the inbox is never re-delivered into it (client §4.1, B.8).
TEST(SyncEngine, EvictionDoesNotResurrectOldLetters) {
  Rig rig;
  const std::string body =
      ResponseJson("c1", ",\"letters\":[" + LetterJson("l-aaa") + "," + LetterJson("l-bbb") + "," +
                             LetterJson("l-ccc") + "]");
  rig.transport.responder = [&](int, const std::string&) { return Ok(body); };
  Options o;
  o.letters_keep = 2;
  auto engine = rig.Engine(o);

  engine->RunOnce(Trigger::kScheduled);
  ASSERT_EQ(rig.letters.Count(), 2u);
  ASSERT_FALSE(rig.letters.evict_calls.empty());
  EXPECT_EQ(rig.letters.evict_calls.back(), 2u);

  const Outcome second = engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(second.letters_stored, 0);
  EXPECT_EQ(second.letters_deduped, 3);
  EXPECT_EQ(rig.letters.Count(), 2u);
}

// ---------------------------------------------------------- request shape

// §5.1: the cursor verbatim, the version actually held, the doorbell counter
// accepted, and everything sendable in the outbox.
TEST(SyncEngine, RequestCarriesCursorVerbatimVersionCounterAndOutbox) {
  Rig rig;
  const std::string opaque = "b64/cursor+with=padding";
  rig.state.state.cursor = opaque;
  rig.contacts.snapshot.version = 8;
  rig.outbox.Seed("o-1", "c_02", "camping", "we went to the lake");
  rig.outbox.Seed("o-2", "c_07", "s", "already rejected");
  rig.outbox.entries[1].rejected = true;

  chaski::syncengine::Deps d = rig.Deps();
  d.pututu_counter_seen = [] { return std::uint64_t{41}; };
  d.kipu = [] {
    chaski::wire::Kipu k;
    k.battery_pct = 84;
    k.fw = "0.1.0";
    return std::optional<chaski::wire::Kipu>(k);
  };
  auto engine = chaski::syncengine::New(d, Options{});

  const chaski::wire::Request req = engine->BuildRequest();
  EXPECT_EQ(req.cursor, opaque);
  EXPECT_EQ(req.ayllu_version, 8);
  EXPECT_EQ(req.pututu_counter_seen, 41u);
  ASSERT_TRUE(req.kipu.has_value());
  EXPECT_EQ(req.kipu->battery_pct, 84);
  ASSERT_EQ(req.outbound.size(), 1u);
  EXPECT_EQ(req.outbound[0].local_id, "o-1");
  EXPECT_EQ(req.outbound[0].subject, "camping");
  EXPECT_EQ(req.outbound[0].body, "we went to the lake");
}

// The version on the wire is the version whose contacts actually landed. If the
// snapshot write failed, claiming it would stop the server ever sending it
// again and the contact list would go permanently stale (§5.2 step 3).
TEST(SyncEngine, AylluVersionFollowsTheAppliedSnapshotNotTheResponse) {
  Rig rig;
  rig.cut_at_step = 3;
  auto engine = rig.Engine();

  engine->ApplyResponse(Decoded(ResponseJson("c1", kAylluBlock)));
  EXPECT_EQ(rig.contacts.snapshots_applied, 0);
  EXPECT_EQ(engine->BuildRequest().ayllu_version, 0);
}

// Step 3 runs before step 4 so a letter from a contact introduced by the same
// response has a name to resolve against (§5.2).
TEST(SyncEngine, AylluIsAppliedBeforeLetters) {
  Rig rig;
  auto engine = rig.Engine();
  const Outcome out = engine->ApplyResponse(Decoded(
      ResponseJson("c1", ",\"letters\":[" + LetterJson("l-aaa", "c_02") + "]" + kAylluBlock)));

  EXPECT_TRUE(out.ayllu_updated);
  chaski::ayllu::Contact c;
  ASSERT_TRUE(rig.contacts.Lookup("c_02", c));
  EXPECT_EQ(c.name, "Abuela");
  EXPECT_TRUE(rig.letters.Has("l-aaa"));
}

// An unknown contact_id is never a reason to drop a letter: it renders under
// the fallback label instead (client §4.4, C-14).
TEST(SyncEngine, LetterFromAnUnknownContactIsStillStored) {
  Rig rig;
  auto engine = rig.Engine();
  const Outcome out = engine->ApplyResponse(
      Decoded(ResponseJson("c1", ",\"letters\":[" + LetterJson("l-aaa", "c_nobody") + "]")));

  EXPECT_EQ(out.letters_stored, 1);
  EXPECT_TRUE(rig.letters.Has("l-aaa"));
}

TEST(SyncEngine, ConfigReachesTheCallerWithUnknownFieldsAlreadyDropped) {
  Rig rig;
  rig.transport.responder = [](int, const std::string&) {
    return Ok(ResponseJson("c1", kConfigBlock));
  };
  auto engine = rig.Engine();

  const Outcome out = engine->RunOnce(Trigger::kScheduled);
  EXPECT_TRUE(out.config_updated);
  ASSERT_EQ(rig.configs.size(), 1u);
  EXPECT_EQ(rig.configs[0].max_letter_chars.value_or(-1), 300);
  EXPECT_EQ(rig.configs[0].sync_interval_s.value_or(-1), 3600);
  EXPECT_EQ(rig.configs[0].rat, "ltem");
}

// ----------------------------------------------- faults and the §5.3 ladder

// 401 is a guardian problem, not a road problem: the device stops rather than
// retrying, until someone deliberately presses the sync key (§5.3, §11.6).
TEST(SyncEngine, UnauthorizedHaltsInsteadOfBackingOff) {
  Rig rig;
  rig.transport.responder = [](int, const std::string&) { return Status(401); };
  auto engine = rig.Engine();

  EXPECT_EQ(engine->RunOnce(Trigger::kScheduled).fault, Fault::kProvisioningFault);
  EXPECT_EQ(engine->NextBackoffMs(), chaski::syncengine::kBackoffHalted);

  // Repeating it does not start a ladder either — halted is halted.
  engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(engine->NextBackoffMs(), chaski::syncengine::kBackoffHalted);

  // The deliberate press is what clears it.
  rig.transport.responder = [](int, const std::string&) { return Ok(ResponseJson("c1")); };
  EXPECT_EQ(engine->RunOnce(Trigger::kUserKey).fault, Fault::kNone);
  EXPECT_EQ(engine->NextBackoffMs(), chaski::syncengine::kBackoffNextScheduledWake);
}

// 503 says when to come back, and the device believes it (§5.3).
TEST(SyncEngine, RoadBusyHonoursRetryAfter) {
  Rig rig;
  rig.transport.responder = [](int, const std::string&) { return Status(503, 42); };
  auto engine = rig.Engine();

  EXPECT_EQ(engine->RunOnce(Trigger::kUserKey).fault, Fault::kRoadBusy);
  EXPECT_EQ(engine->NextBackoffMs(), 42 * 1000);
}

// Without the header there is nothing to honour, so the ordinary ladder runs.
TEST(SyncEngine, RoadBusyWithoutRetryAfterUsesTheLadder) {
  Rig rig;
  rig.transport.responder = [](int, const std::string&) { return Status(503); };
  auto engine = rig.Engine();

  engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(engine->NextBackoffMs(), 30 * 1000);
}

// D-6: a trust failure is its own visible state. Collapsing it into "no signal"
// would hide the one failure that means something is between the device and
// home.
TEST(SyncEngine, TlsTrustFailureIsItsOwnFault) {
  Rig rig;
  rig.transport.responder = [](int, const std::string&) { return TlsTrustFail(); };
  auto engine = rig.Engine();

  const Outcome out = engine->RunOnce(Trigger::kUserKey);
  EXPECT_EQ(out.fault, Fault::kCantReachHome);
  EXPECT_NE(out.fault, Fault::kNoSignal);
  EXPECT_NE(out.fault, Fault::kServerFault);
  EXPECT_EQ(engine->NextBackoffMs(), 30 * 1000);
}

// No signal is not an error state on this device: the letters simply wait
// (client §5.3, design Principle 4).
TEST(SyncEngine, TransportFailureIsNoSignal) {
  Rig rig;
  rig.outbox.Seed("o-1", "c_02", "s", "b");
  rig.transport.responder = [](int, const std::string&) { return TransportFail(); };
  auto engine = rig.Engine();

  EXPECT_EQ(engine->RunOnce(Trigger::kScheduled).fault, Fault::kNoSignal);
  EXPECT_EQ(rig.outbox.SendableCount(), 1u);  // still on the road
  EXPECT_EQ(rig.state.state.cursor, "");
}

TEST(SyncEngine, OtherStatusesAreServerFaults) {
  const int codes[] = {400, 404, 418, 500, 502};
  for (int code : codes) {
    Rig rig;
    rig.transport.responder = [code](int, const std::string&) { return Status(code); };
    auto engine = rig.Engine();
    EXPECT_EQ(engine->RunOnce(Trigger::kScheduled).fault, Fault::kServerFault) << code;
  }
}

// A 200 the device cannot parse changes nothing: the identical request is safe
// to retry (server §4.1), and half-applying a guess is not.
TEST(SyncEngine, MalformedBodyLeavesTheCursorAlone) {
  Rig rig;
  rig.state.state.cursor = "cursor-old";
  rig.transport.responder = [](int, const std::string&) { return Ok("{\"letters\":[}"); };
  auto engine = rig.Engine();

  EXPECT_EQ(engine->RunOnce(Trigger::kScheduled).fault, Fault::kServerFault);
  EXPECT_EQ(rig.state.state.cursor, "cursor-old");
  EXPECT_EQ(rig.state.cursor_writes, 0);
}

// 30 s, 2 min, 10 min, then the next scheduled wake. Capped, never a hot loop
// on a link billed by the megabyte (§5.3).
TEST(SyncEngine, BackoffLadderIsCappedAndResets) {
  Rig rig;
  rig.transport.responder = [](int, const std::string&) { return TransportFail(); };
  auto engine = rig.Engine();

  engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(engine->NextBackoffMs(), 30 * 1000);
  engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(engine->NextBackoffMs(), 2 * 60 * 1000);
  engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(engine->NextBackoffMs(), 10 * 60 * 1000);
  engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(engine->NextBackoffMs(), chaski::syncengine::kBackoffNextScheduledWake);
  engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(engine->NextBackoffMs(), chaski::syncengine::kBackoffNextScheduledWake);

  rig.transport.responder = [](int, const std::string&) { return Ok(ResponseJson("c1")); };
  EXPECT_EQ(engine->RunOnce(Trigger::kScheduled).fault, Fault::kNone);
  EXPECT_EQ(engine->NextBackoffMs(), chaski::syncengine::kBackoffNextScheduledWake);
}

// A failure part-way through a drain still counts as one sync event, and the
// rounds that did land keep their letters.
TEST(SyncEngine, FailureMidDrainKeepsWhatLanded) {
  Rig rig;
  rig.transport.responder = [](int round, const std::string&) {
    if (round == 0) {
      return Ok(ResponseJson("cursor-0", ",\"letters\":[" + LetterJson("l-aaa") + "]", true));
    }
    return TransportFail();
  };
  auto engine = rig.Engine();

  const Outcome out = engine->RunOnce(Trigger::kScheduled);
  EXPECT_EQ(out.fault, Fault::kNoSignal);
  EXPECT_EQ(out.rounds, 2);
  EXPECT_EQ(out.letters_stored, 1);
  EXPECT_EQ(rig.state.state.cursor, "cursor-0");
  EXPECT_EQ(rig.state.last_sync_writes, 1);
}

// The engine must not dereference a half-wired composition root: a missing seam
// applies nothing rather than crashing a device in a pocket.
TEST(SyncEngine, UnwiredSeamsApplyNothing) {
  chaski::syncengine::Deps d;
  auto engine = chaski::syncengine::New(d, Options{});
  EXPECT_TRUE(engine->RunOnce(Trigger::kScheduled).apply_incomplete);
  EXPECT_TRUE(engine->BuildRequest().cursor.empty());
}
