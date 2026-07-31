// Scaffold test for components/pututu. Wave 2B owns the real suite: MAC
// verification, strict counter monotonicity, silent failure, and the rate
// limit that holds even when validation is wrong (C-8).
#include <gtest/gtest.h>

#include "chaski/pututu.h"

// Every failure is silent by design: no response, no wake, no UI. The verdicts
// exist for tests and content-free diagnostics, never for the child's screen.
TEST(Pututu, RateLimitIsFiveMinutes) {
  EXPECT_EQ(chaski::pututu::kMinWakeIntervalMs, 5 * 60 * 1000);
  using chaski::pututu::Verdict;
  EXPECT_NE(Verdict::kAccept, Verdict::kRateLimited);
  EXPECT_NE(Verdict::kBadMac, Verdict::kStaleCounter);
}
