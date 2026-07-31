// Scaffold test for components/input. Wave 3B owns the real suite: C-11
// asserts a deliberately hung screen handler cannot swallow put-away or sync.
#include <gtest/gtest.h>

#include "chaski/input.h"

// Put-away and sync are their own keys precisely so the dispatcher can consume
// them before any screen sees them (design §4.1, client §10).
TEST(Input, PutAwayAndSyncAreDistinctKeys) {
  using chaski::input::Key;
  EXPECT_NE(Key::kPutAway, Key::kSync);
  EXPECT_NE(Key::kPutAway, Key::kBack);
  EXPECT_NE(Key::kPutAway, Key::kNone);
}
