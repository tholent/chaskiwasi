// Host tests for the wake dispatch (client §6, §4.3).
//
// There is no Walter on this bench, so nothing here proves the device wakes.
// What it proves is the part that would be untestable even with one: given a
// wake reason and the surviving bookkeeping, exactly one job runs, and losing
// RTC memory degrades in the safe direction every time.
#include "wake.h"

#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "chaski/pututu.h"

namespace {

using chaski::wake::DoorbellPoll;
using chaski::wake::Inputs;
using chaski::wake::Reason;
using chaski::wake::RtcState;
using chaski::wake::RunResult;
using chaski::wake::SyncCause;

// RecordingJobs is the composition root with the hardware taken out: it
// records what was asked of it, in order, which is the assertion "exactly one
// job per wake" needs.
class RecordingJobs final : public chaski::wake::Jobs {
 public:
  DoorbellPoll PollDoorbell() override {
    calls.push_back("doorbell");
    return doorbell;
  }
  void Sync(SyncCause c) override {
    calls.push_back("sync");
    sync_cause = c;
  }
  void OpenUiSession() override { calls.push_back("ui"); }
  bool AnyUnread() override { return any_unread; }
  void RenderCover(bool unread) override {
    calls.push_back(unread ? "cover:flag" : "cover:plain");
  }
  void RenderChargeMeCover() override { calls.push_back("cover:charge_me"); }

  std::vector<std::string> calls;
  DoorbellPoll doorbell;
  bool any_unread = false;
  SyncCause sync_cause = SyncCause::kNone;
};

RtcState FreshRtc(std::int64_t uptime_ms = 0) {
  RtcState s;
  chaski::wake::Reconstruct(s, uptime_ms);
  return s;
}

// A device that has been running: RTC intact, the mirror trusted, the limiter
// long past its window.
RtcState SettledRtc(std::int64_t uptime_ms, bool unread = false) {
  RtcState s = FreshRtc(0);
  chaski::wake::SetUnreadMirror(s, unread);
  chaski::wake::NoteSmsWake(s, uptime_ms - chaski::pututu::kMinWakeIntervalMs);
  chaski::wake::ScheduleNextSync(s, uptime_ms, 900);
  return s;
}

Inputs At(Reason r, std::int64_t uptime_ms, const RtcState& s) {
  Inputs in;
  in.reason = r;
  in.uptime_ms = uptime_ms;
  in.rtc_intact = chaski::wake::Intact(s);
  in.next_sync_due_ms = s.next_sync_due_ms;
  return in;
}

// §6: key wake opens the UI, and the radio stays down while composing. A sync
// on the way into the compose screen is the single most expensive thing this
// device could do wrong, and it would be invisible in a demo.
TEST(Wake, KeyPressOpensTheUiAndNeverSyncs) {
  RtcState rtc = SettledRtc(3600000);
  RecordingJobs jobs;
  const RunResult r =
      chaski::wake::Run(At(Reason::kKeypress, 3600000, rtc), rtc, jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"ui"}));
  EXPECT_TRUE(r.ui_opened);
  EXPECT_FALSE(r.synced);
}

// A key wake is a session: the UI owns the panel and its wipe leaves the cover
// behind (§9). Re-rendering underneath it would fight the wipe sequence.
TEST(Wake, KeyPressDoesNotRenderTheCoverItself) {
  RtcState rtc = SettledRtc(3600000);
  RecordingJobs jobs;
  jobs.any_unread = true;
  chaski::wake::Run(At(Reason::kKeypress, 3600000, rtc), rtc, jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"ui"}));
}

TEST(Wake, TimerWakePollsTheDoorbellAndSyncsWhenDue) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  const std::int64_t now = rtc.next_sync_due_ms + 1;
  chaski::wake::Run(At(Reason::kTimer, now, rtc), rtc, jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"doorbell", "sync"}));
  EXPECT_EQ(jobs.sync_cause, SyncCause::kScheduled);
}

TEST(Wake, TimerWakeBeforeTheIntervalPollsButDoesNotSync) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  chaski::wake::Run(At(Reason::kTimer, rtc.next_sync_due_ms - 1000, rtc), rtc,
                    jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"doorbell"}));
}

// §5: a letter waiting in the outbox is its own reason to spend the radio at
// the next wake — "waiting for the runner" has to end.
TEST(Wake, QueuedOutboundSyncsEvenBeforeTheInterval) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  Inputs in = At(Reason::kTimer, rtc.next_sync_due_ms - 1000, rtc);
  in.outbound_queued = true;
  chaski::wake::Run(in, rtc, jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"doorbell", "sync"}));
  EXPECT_EQ(jobs.sync_cause, SyncCause::kOutboundQueued);
}

TEST(Wake, AcceptedDoorbellSyncsImmediately) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  jobs.doorbell = DoorbellPoll{/*received=*/true, /*accepted=*/true};
  const RunResult r = chaski::wake::Run(
      At(Reason::kTimer, rtc.next_sync_due_ms - 60000, rtc), rtc, jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"doorbell", "sync"}));
  EXPECT_EQ(jobs.sync_cause, SyncCause::kDoorbell);
  EXPECT_TRUE(r.doorbell_accepted);
}

// §7: one SMS-triggered wake per 5 minutes, tracked in RTC memory. The second
// valid token inside the window is ignored — a doorbell that can be rung on
// demand is a battery and a balance someone else gets to spend.
TEST(Wake, DoorbellIsRateLimitedToOnePerFiveMinutes) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  jobs.doorbell = DoorbellPoll{true, true};
  const std::int64_t first = rtc.next_sync_due_ms - 600000;
  chaski::wake::Run(At(Reason::kTimer, first, rtc), rtc, jobs);
  ASSERT_EQ(jobs.calls, (std::vector<std::string>{"doorbell", "sync"}));

  RecordingJobs again;
  again.doorbell = DoorbellPoll{true, true};
  const RunResult r2 = chaski::wake::Run(
      At(Reason::kTimer, first + 60000, rtc), rtc, again);
  EXPECT_EQ(again.calls, (std::vector<std::string>{"doorbell"}));
  EXPECT_FALSE(r2.synced);

  RecordingJobs later;
  later.doorbell = DoorbellPoll{true, true};
  chaski::wake::Run(
      At(Reason::kTimer, first + chaski::pututu::kMinWakeIntervalMs, rtc), rtc,
      later);
  EXPECT_EQ(later.calls, (std::vector<std::string>{"doorbell", "sync"}));
}

// A closed window must not be re-charged by arrivals it already refused.
//
// Charging on every arrival slides the window forward, so a token every four
// minutes suppresses the doorbell forever. That swaps the battery-drain attack
// the limiter defends against for a delay-the-child's-letters attack: letters
// would wait for the six-hourly scheduled sync with nothing visibly wrong.
// Found by this test failing against the first implementation (F-C11).
TEST(Wake, AFloodCannotSuppressTheDoorbellIndefinitely) {
  // Start at the settled uptime, where the window is exactly open and the
  // scheduled sync is not yet due — so only the doorbell can cause a sync.
  const std::int64_t start = 1000000;
  RtcState rtc = SettledRtc(start);

  RecordingJobs first;
  first.doorbell = DoorbellPoll{true, true};
  chaski::wake::Run(At(Reason::kTimer, start, rtc), rtc, first);
  ASSERT_TRUE(first.calls == (std::vector<std::string>{"doorbell", "sync"}));

  // An attacker rings every minute for the whole window. None is accepted, and
  // none may extend the window.
  for (int minute = 1; minute < 5; ++minute) {
    RecordingJobs flood;
    flood.doorbell = DoorbellPoll{true, true};
    const RunResult r =
        chaski::wake::Run(At(Reason::kTimer, start + minute * 60000, rtc), rtc, flood);
    ASSERT_FALSE(r.doorbell_accepted) << "accepted at minute " << minute;
  }

  // The window still opens exactly five minutes after the FIRST wake.
  EXPECT_TRUE(chaski::wake::SmsWakeAllowed(
      rtc, start + chaski::pututu::kMinWakeIntervalMs));
}

// "Regardless of validity" (§7): a forged or replayed token still charges the
// limiter, so a flood cannot buy more syncs than one honest token would.
TEST(Wake, RejectedTokensStillChargeTheRateLimiter) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  jobs.doorbell = DoorbellPoll{/*received=*/true, /*accepted=*/false};
  const std::int64_t now = rtc.next_sync_due_ms - 600000;
  chaski::wake::Run(At(Reason::kTimer, now, rtc), rtc, jobs);
  EXPECT_FALSE(chaski::wake::SmsWakeAllowed(rtc, now + 60000));
}

// §6: after a background sync that stored new letters, the cover is
// re-rendered with the flag up before returning to sleep. Expressed as
// "whenever the glass disagrees with the store", which also covers a boot that
// lost the mirror.
TEST(Wake, CoverIsRerenderedWhenTheFlagChanges) {
  RtcState rtc = SettledRtc(1000000, /*unread=*/false);
  RecordingJobs jobs;
  jobs.any_unread = true;
  chaski::wake::Run(At(Reason::kTimer, rtc.next_sync_due_ms + 1, rtc), rtc,
                    jobs);
  EXPECT_EQ(jobs.calls,
            (std::vector<std::string>{"doorbell", "sync", "cover:flag"}));
  EXPECT_EQ(rtc.unread_mirror, 1);
}

TEST(Wake, CoverIsLeftAloneWhenTheFlagIsUnchanged) {
  RtcState rtc = SettledRtc(1000000, /*unread=*/true);
  RecordingJobs jobs;
  jobs.any_unread = true;
  chaski::wake::Run(At(Reason::kTimer, rtc.next_sync_due_ms + 1, rtc), rtc,
                    jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"doorbell", "sync"}));
}

// §5.6: the clock is invalid until the first sync of a power cycle, so a cold
// boot always syncs — dates render blank until it does.
TEST(Wake, ColdBootSyncsAndRendersTheCover) {
  RtcState rtc = FreshRtc(0);
  RecordingJobs jobs;
  jobs.any_unread = true;
  chaski::wake::Run(At(Reason::kColdBoot, 0, rtc), rtc, jobs);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"sync", "cover:flag"}));
  EXPECT_EQ(jobs.sync_cause, SyncCause::kFirstBoot);
}

// A USB attach is a charging event. It must not open the inbox: the device may
// be on someone else's desk.
TEST(Wake, UsbAttachNeverOpensTheUi) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  const RunResult r =
      chaski::wake::Run(At(Reason::kUsb, rtc.next_sync_due_ms - 1000, rtc), rtc,
                        jobs);
  EXPECT_FALSE(r.ui_opened);
  EXPECT_FALSE(r.synced);
}

// D-1: an unexplained wake never puts content on the glass.
TEST(Wake, UnknownWakeIsTreatedAsBackgroundWork) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  const RunResult r = chaski::wake::Run(
      At(Reason::kUnknown, rtc.next_sync_due_ms + 1, rtc), rtc, jobs);
  EXPECT_FALSE(r.ui_opened);
  EXPECT_EQ(jobs.calls, (std::vector<std::string>{"doorbell", "sync"}));
}

// §6, §9: below 5% the device refuses to open content and does not spend the
// radio. E-ink keeps showing the charge-me cover after the pack is flat, which
// is what makes this the right last act.
TEST(Wake, BelowTheBatteryFloorOnlyTheChargeMeCoverHappens) {
  for (const Reason r : {Reason::kKeypress, Reason::kTimer, Reason::kColdBoot,
                         Reason::kUsb, Reason::kUnknown}) {
    RtcState rtc = SettledRtc(1000000);
    RecordingJobs jobs;
    Inputs in = At(r, rtc.next_sync_due_ms + 1, rtc);
    in.below_battery_floor = true;
    in.outbound_queued = true;
    const RunResult res = chaski::wake::Run(in, rtc, jobs);
    EXPECT_EQ(jobs.calls, (std::vector<std::string>{"cover:charge_me"}));
    EXPECT_TRUE(res.charge_me);
    EXPECT_FALSE(res.synced);
    EXPECT_FALSE(res.ui_opened);
  }
}

// §4.3: losing RTC memory is always safe — every field is reconstructible or
// conservative. These are the four that matter.
TEST(Wake, RtcLossDegradesSafely) {
  RtcState lost;  // zeroed, as a power-cycled RTC segment reads
  EXPECT_FALSE(chaski::wake::Intact(lost));

  RtcState rtc;
  chaski::wake::Reconstruct(rtc, 0);
  EXPECT_TRUE(chaski::wake::Intact(rtc));

  // 1. The sync is due immediately: the clock is invalid and the cursor is in
  //    flash, so the resync is cheap and the dates start rendering.
  EXPECT_LE(rtc.next_sync_due_ms, 0);

  // 2. The doorbell limiter starts inside its window and leaves it a minute in.
  EXPECT_FALSE(chaski::wake::SmsWakeAllowed(rtc, 0));
  EXPECT_FALSE(chaski::wake::SmsWakeAllowed(
      rtc, chaski::wake::kBootConservativeWindowMs - 1));
  EXPECT_TRUE(chaski::wake::SmsWakeAllowed(
      rtc, chaski::wake::kBootConservativeWindowMs));

  // 3. The unread mirror is not trusted, so the cover is rendered from the
  //    store rather than from a value that died with the power.
  EXPECT_TRUE(chaski::wake::CoverNeedsRerender(rtc, false));
  EXPECT_TRUE(chaski::wake::CoverNeedsRerender(rtc, true));

  // 4. The boot counter restarts; nothing durable is keyed to it.
  EXPECT_EQ(rtc.boot_count, 0u);
}

TEST(Wake, RtcLossStillSyncsEvenWithAStaleSchedule) {
  RtcState rtc = SettledRtc(1000000);
  RecordingJobs jobs;
  Inputs in = At(Reason::kTimer, 0, rtc);
  in.rtc_intact = false;             // the segment was lost; the schedule is noise
  in.next_sync_due_ms = 99999999;    // and it says "not for a day"
  chaski::wake::Run(in, rtc, jobs);
  EXPECT_EQ(jobs.calls[1], "sync");
}

TEST(Wake, BootCounterAdvancesPerWake) {
  RtcState rtc = FreshRtc(0);
  chaski::wake::NoteWake(rtc, Reason::kTimer);
  chaski::wake::NoteWake(rtc, Reason::kKeypress);
  EXPECT_EQ(rtc.boot_count, 2u);
  EXPECT_EQ(rtc.last_reason, static_cast<std::int32_t>(Reason::kKeypress));
}

TEST(Wake, NextSyncIsScheduledFromTheAppliedInterval) {
  RtcState rtc = FreshRtc(0);
  chaski::wake::ScheduleNextSync(rtc, 5000, 900);
  EXPECT_EQ(rtc.next_sync_due_ms, 5000 + 900 * 1000);
}

}  // namespace
