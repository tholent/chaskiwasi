// Scaffold test for components/store. Wave 1A owns the real suite: idempotent
// Put, eviction that does not resurrect (C-2), monotonic never-reused local
// ids across simulated power loss (C-5), atomic write discipline.
#include <gtest/gtest.h>

#include "chaski/store.h"

// The bounded window is a decision, not a limit of the flash: the mailbox is
// canonical and the device is a derived view (design Principle 5, B.8).
TEST(Store, SpecConstantsMatchTheSpec) {
  EXPECT_EQ(chaski::store::kDefaultLettersKeep, 200u);  // §4.1, matches resync_window
  EXPECT_GE(chaski::store::kMinSeenIds, 1000u);         // server §4.5 wire contract
  EXPECT_EQ(chaski::store::kOutboxCap, 12u);            // B.9
}
