// Scaffold test for components/kipu. Wave 2B owns the real suite: block size
// under the wire cap, and the readable log the settings screen renders.
#include <gtest/gtest.h>

#include "chaski/kipu.h"

// v1 is health only. There is no position field here, and adding one is a v2
// decision paired with the server's cell service, not a convenience.
TEST(Kipu, Tier1BlockIsHealthOnly) {
  chaski::wire::Kipu k;
  EXPECT_EQ(k.battery_pct, 0);
  EXPECT_EQ(k.queue_depth, 0);
  EXPECT_FALSE(k.charging);
  EXPECT_GT(chaski::kipu::kLogCapacity, 0u);
}
