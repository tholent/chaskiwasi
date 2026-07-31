// Scaffold test for components/ayllu. Wave 1A owns the real suite: snapshot
// replacement that preserves the overlay, tombstones rendering but not
// composable (C-13), unknown ids kept under a fallback label (C-14).
#include <gtest/gtest.h>

#include "chaski/ayllu.h"

TEST(Ayllu, OverlayFieldsAreAllOptional) {
  chaski::ayllu::Overlay o;
  // An unset field means "use the server's value", which is what makes a
  // guardian's rename still reach the child (client §4.4, B.3).
  EXPECT_FALSE(o.nickname.has_value());
  EXPECT_FALSE(o.pinned.has_value());
  EXPECT_FALSE(o.order.has_value());
  EXPECT_FALSE(o.portrait.has_value());
}
