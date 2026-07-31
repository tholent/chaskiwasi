// Scaffold test for components/syncengine. Wave 1B owns the real suite: the
// §5.2 apply order driven through the fault hook at every step (C-4), terminal
// acks (C-3), the drain cap (C-6), clock validity (C-21).
#include <gtest/gtest.h>

#include "chaski/syncengine.h"

// The three transport outcomes must stay distinct: collapsing them loses the
// difference between "the road is busy" and "can't reach home", and D-6
// requires a TLS trust failure to be visibly its own state.
TEST(SyncEngine, FaultKindsAreDistinct) {
  using chaski::syncengine::Fault;
  EXPECT_NE(Fault::kCantReachHome, Fault::kNoSignal);
  EXPECT_NE(Fault::kCantReachHome, Fault::kServerFault);
  EXPECT_NE(Fault::kProvisioningFault, Fault::kRoadBusy);
}
