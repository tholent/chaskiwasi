// Host tests for components/power: the graceful poll and the emergency
// backstop, both driven by a fake gauge (client §9, §9.1, B.13).
//
// What is NOT here, and cannot be: the MAX17048 driver, real ALRT timing, and
// the battery sag through a flush. Those are Wave 4C's and the bench's (C-24's
// power half). What is here is every decision: when the threshold fires, what
// the confirming re-read is allowed to cancel, and whether the alert is armed.
#include <memory>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "chaski/power.h"
#include "fake_gauge.h"

namespace {

using chaski::power::EmergencyOutcome;
using chaski::power::MonitorDeps;
using chaski::power::Reading;
using chaski_test::FakeClock;
using chaski_test::FakeGauge;

Reading At(int soc, int mv, bool charging = false) {
  Reading r;
  r.soc_pct = soc;
  r.millivolts = mv;
  r.charging = charging;
  r.valid = true;
  return r;
}

// Rig wires a Monitor to a fake gauge and records what the seams were asked to
// do, in order — the emergency path's whole contract is an ordering.
struct Rig {
  FakeGauge gauge;
  FakeClock clock;
  std::vector<std::string> calls;

  std::unique_ptr<chaski::power::Monitor> Build() {
    MonitorDeps d;
    d.gauge = &gauge;
    d.on_emergency = [this]() { calls.push_back("wipe"); };
    d.radio_off = [this]() { calls.push_back("radio_off"); };
    d.delay_ms = [this](int ms) {
      calls.push_back("delay");
      clock.Advance(ms);
    };
    d.on_alert_isr = [this]() { calls.push_back("isr"); };
    d.monotonic_ms = [this]() { return clock.Now(); };
    return chaski::power::NewMonitor(d);
  }
};

// The emergency threshold is a BACKSTOP: it must sit below where the graceful
// path fires, or the hardware alert would pre-empt the orderly wipe instead of
// catching what it misses (client §9.1, B.13). The value is provisional until
// the cold-battery sag measurement at bring-up (client §16).
TEST(Power, EmergencyThresholdSitsBelowTheGracefulPath) {
  EXPECT_EQ(chaski::power::kGracefulSocPct, 5);
  EXPECT_EQ(chaski::power::kEmergencyMilliVolts, 3300);
  EXPECT_GT(chaski::power::kEmergencyConfirmDelayMs, 0);
}

TEST(Power, GracefulCrossingFiresOnceAndLatches) {
  Rig rig;
  rig.gauge.reading = At(20, 3900);
  auto m = rig.Build();
  m->BeginSession();
  EXPECT_FALSE(m->GracefulThresholdCrossed());
  EXPECT_FALSE(m->ChargeMeLatched());

  rig.gauge.reading = At(4, 3600);
  rig.clock.Advance(chaski::power::kGaugePollIntervalMs);
  EXPECT_TRUE(m->GracefulThresholdCrossed());
  EXPECT_TRUE(m->ChargeMeLatched());

  // Still below the line: the state persists, but the wipe does not repeat.
  // A cover that re-wipes every poll is a device flashing at a child.
  for (int i = 0; i < 3; ++i) {
    rig.clock.Advance(chaski::power::kGaugePollIntervalMs);
    EXPECT_FALSE(m->GracefulThresholdCrossed());
  }
  EXPECT_TRUE(m->ChargeMeLatched());
}

// "Refuse to open content until charging" (client §6): charging is what
// releases the latch, not a recovered SOC estimate.
TEST(Power, ChargingReleasesTheChargeMeLatch) {
  Rig rig;
  rig.gauge.reading = At(3, 3550);
  auto m = rig.Build();
  m->BeginSession();
  EXPECT_TRUE(m->GracefulThresholdCrossed());

  rig.gauge.reading = At(3, 3700, /*charging=*/true);
  rig.clock.Advance(chaski::power::kGaugePollIntervalMs);
  EXPECT_FALSE(m->GracefulThresholdCrossed());
  EXPECT_FALSE(m->ChargeMeLatched());
}

// An unreadable gauge must not drive the graceful path: it is an estimate, and
// the hardware alert is what covers a gauge that lies or goes silent.
TEST(Power, InvalidReadingDoesNotFireTheGracefulPath) {
  Rig rig;
  rig.gauge.reading = Reading{};  // valid == false
  auto m = rig.Build();
  m->BeginSession();
  EXPECT_FALSE(m->GracefulThresholdCrossed());
  EXPECT_FALSE(m->ChargeMeLatched());
}

TEST(Power, PollingIsRateLimitedToTheActiveSamplePeriod) {
  Rig rig;
  rig.gauge.reading = At(50, 3900);
  auto m = rig.Build();
  m->BeginSession();
  const int after_begin = rig.gauge.reads;
  for (int i = 0; i < 10; ++i) (void)m->GracefulThresholdCrossed();
  EXPECT_EQ(rig.gauge.reads, after_begin);

  rig.clock.Advance(chaski::power::kGaugePollIntervalMs);
  (void)m->GracefulThresholdCrossed();
  EXPECT_EQ(rig.gauge.reads, after_begin + 1);
}

// C-24: the alert is armed while AWAKE only. A sleeping device already ran the
// wipe and shows the cover, which is safe past battery death (D-1), so arming
// it in sleep would add idle cost for nothing.
TEST(C24, AlertIsArmedForTheSessionAndDisarmedBeforeSleep) {
  Rig rig;
  rig.gauge.reading = At(80, 4000);
  auto m = rig.Build();
  EXPECT_FALSE(rig.gauge.armed);

  m->BeginSession();
  EXPECT_TRUE(rig.gauge.armed);
  EXPECT_EQ(rig.gauge.armed_mv, chaski::power::kEmergencyMilliVolts);

  m->EndSession();
  EXPECT_FALSE(rig.gauge.armed);

  // A disarmed gauge cannot deliver an alert, so a wipe cannot be triggered
  // from sleep by a stale callback.
  rig.gauge.FireAlert();
  EXPECT_FALSE(m->EmergencyPending());
}

// C-24 host half: the gauge's alert path reaches the emergency handler, and it
// reaches it in the §9.1 order — radio off BEFORE the confirming re-read.
TEST(C24, GaugeAlertRunsTheEmergencyHandlerRadioOffFirst) {
  Rig rig;
  rig.gauge.reading = At(6, 3250);  // still sagging when the re-read lands
  auto m = rig.Build();
  m->BeginSession();

  rig.gauge.FireAlert();
  EXPECT_TRUE(m->EmergencyPending());
  // The callback runs in interrupt context on the target: it hands off and
  // does nothing else. No wipe may have started yet.
  EXPECT_EQ(rig.calls, (std::vector<std::string>{"isr"}));

  EXPECT_EQ(m->ServiceEmergency(), EmergencyOutcome::kWiped);
  EXPECT_EQ(rig.calls,
            (std::vector<std::string>{"isr", "radio_off", "delay", "wipe"}));
  EXPECT_FALSE(m->EmergencyPending());
}

// The debounce of §9.1: at most one re-read, and it may cancel only when the
// voltage came back with margin. An LTE burst sags a healthy pack; letting it
// wipe every time costs winter runtime for nothing.
TEST(C24, RecoveredVoltageCancelsTheWipeAndStaysArmed) {
  Rig rig;
  rig.gauge.reading = At(40, 3400);
  auto m = rig.Build();
  m->BeginSession();
  const int arms_before = rig.gauge.arm_count;

  rig.gauge.reading =
      At(40, chaski::power::kEmergencyMilliVolts +
                 chaski::power::kEmergencyConfirmMarginMv + 10);
  rig.gauge.FireAlert();
  EXPECT_EQ(m->ServiceEmergency(), EmergencyOutcome::kCancelled);
  EXPECT_EQ(rig.calls,
            (std::vector<std::string>{"isr", "radio_off", "delay"}));
  EXPECT_EQ(rig.gauge.clear_count, 1);
  EXPECT_GT(rig.gauge.arm_count, arms_before);
  EXPECT_TRUE(rig.gauge.armed);
}

// Margin, not equality: a reading that merely touches the threshold again is
// not a recovery. Doubt resolves toward wiping (client §9.1).
TEST(C24, VoltageBackAtTheThresholdWithoutMarginStillWipes) {
  Rig rig;
  rig.gauge.reading = At(10, 3200);
  auto m = rig.Build();
  m->BeginSession();

  rig.gauge.reading = At(10, chaski::power::kEmergencyMilliVolts +
                                 chaski::power::kEmergencyConfirmMarginMv - 1);
  rig.gauge.FireAlert();
  EXPECT_EQ(m->ServiceEmergency(), EmergencyOutcome::kWiped);
}

// A gauge that cannot be read during the confirmation is exactly the case the
// hardware backstop exists for. Silence is not a recovery.
TEST(C24, UnreadableGaugeDuringConfirmationWipes) {
  Rig rig;
  rig.gauge.reading = At(10, 3200);
  auto m = rig.Build();
  m->BeginSession();

  rig.gauge.reading = Reading{};  // valid == false
  rig.gauge.FireAlert();
  EXPECT_EQ(m->ServiceEmergency(), EmergencyOutcome::kWiped);
}

TEST(C24, ServicingWithNoAlertDoesNothing) {
  Rig rig;
  rig.gauge.reading = At(80, 4000);
  auto m = rig.Build();
  m->BeginSession();
  EXPECT_EQ(m->ServiceEmergency(), EmergencyOutcome::kNoAlert);
  EXPECT_TRUE(rig.calls.empty());
}

// One alert produces one sequence: the flag is consumed, not level-triggered,
// so a handler task that loops cannot wipe twice off a single sag.
TEST(C24, AlertIsConsumedOnce) {
  Rig rig;
  rig.gauge.reading = At(6, 3200);
  auto m = rig.Build();
  m->BeginSession();
  rig.gauge.FireAlert();
  EXPECT_EQ(m->ServiceEmergency(), EmergencyOutcome::kWiped);
  EXPECT_EQ(m->ServiceEmergency(), EmergencyOutcome::kNoAlert);
}

}  // namespace
