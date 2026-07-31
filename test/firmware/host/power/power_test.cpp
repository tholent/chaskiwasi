// Scaffold test for components/power. Wave 4C owns the real suite, with C-24's
// live half on the bench; the sequence half runs against the recording panel.
#include <gtest/gtest.h>

#include "chaski/power.h"

// The emergency threshold is a BACKSTOP: it must sit below where the graceful
// path fires, or the hardware alert would pre-empt the orderly wipe instead of
// catching what it misses (client §9.1, B.13). The value is provisional until
// the cold-battery sag measurement at bring-up (client §16).
TEST(Power, EmergencyThresholdSitsBelowTheGracefulPath) {
  EXPECT_EQ(chaski::power::kGracefulSocPct, 5);
  EXPECT_EQ(chaski::power::kEmergencyMilliVolts, 3300);
  EXPECT_GT(chaski::power::kEmergencyConfirmDelayMs, 0);
}
