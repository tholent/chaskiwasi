// Scaffold test for components/wire. Wave 1B owns the real suite: fixture
// round-trips against test/firmware/host/testdata/wire/ (implementation-plan
// §2), plus every ack status and worst-case emoji bodies.
#include <gtest/gtest.h>

#include "chaski/wire.h"

using chaski::wire::AckStatus;

// Every ack status is terminal (server §4.7, client D-5). kSent is the only
// one that is not a reject; an ack we cannot interpret still counts as one,
// because telling the child to ask their guardians is the safe reading.
TEST(Wire, AckRejectClassificationIsScaffolded) {
  EXPECT_EQ(chaski::wire::kMaxKipuBytes, 512);
  EXPECT_EQ(chaski::wire::kMaxSubjectGraphemes, 100);
  EXPECT_STREQ(chaski::wire::kSysContactId, "c_sys");
  EXPECT_NE(AckStatus::kSent, AckStatus::kRejectedUndeliverable);
}
