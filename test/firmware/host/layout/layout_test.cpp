// Scaffold test for components/layout. Wave 1C owns the real suite: C-9's
// grapheme vectors from tools/graphvectors — ZWJ emoji, combining marks,
// regional-indicator flags — asserting no break ever splits a cluster.
#include <gtest/gtest.h>

#include "chaski/layout.h"

// Layout owns every layout number in the system; the server owns none
// (server §4.9, A.10). The reference panel is 264x176 (client §2).
TEST(Layout, ReferenceMetricsMatchThePanel) {
  chaski::layout::Metrics m;
  EXPECT_EQ(m.panel_w_px, 264);
  EXPECT_EQ(m.panel_h_px, 176);
}
