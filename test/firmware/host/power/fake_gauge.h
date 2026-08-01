// FakeGauge stands in for the MAX17048 in host tests.
//
// The real driver is Wave 4C's and needs a battery, an I2C bus, and a cold
// room to be interesting. Everything the Monitor decides — when the graceful
// threshold fires, whether the confirming re-read cancels a wipe, whether the
// alert is armed at all — is policy, and policy is testable here (client §9.1,
// implementation-plan ground rule 3).
#pragma once

#include <functional>
#include <utility>
#include <vector>

#include "chaski/power.h"

namespace chaski_test {

class FakeGauge final : public chaski::power::Gauge {
 public:
  chaski::power::Reading Read() override {
    ++reads;
    return reading;
  }

  bool ArmUndervoltAlert(int millivolts, std::function<void()> cb) override {
    armed = true;
    ++arm_count;
    armed_mv = millivolts;
    cb_ = std::move(cb);
    return true;
  }

  void DisarmUndervoltAlert() override {
    armed = false;
    ++disarm_count;
  }

  void ClearAlert() override { ++clear_count; }

  // FireAlert plays the ALRT pin. On the target this arrives as an interrupt;
  // here it is a direct call, which is the same thing minus the context.
  void FireAlert() {
    if (armed && cb_) cb_();
  }

  chaski::power::Reading reading;
  bool armed = false;
  int armed_mv = 0;
  int arm_count = 0;
  int disarm_count = 0;
  int clear_count = 0;
  int reads = 0;

 private:
  std::function<void()> cb_;
};

// FakeClock is a monotonic millisecond source the test advances by hand, so a
// 250 ms debounce costs no wall-clock time and never flakes.
class FakeClock {
 public:
  std::int64_t Now() const { return now_ms_; }
  void Advance(std::int64_t ms) { now_ms_ += ms; }

 private:
  std::int64_t now_ms_ = 0;
};

}  // namespace chaski_test
