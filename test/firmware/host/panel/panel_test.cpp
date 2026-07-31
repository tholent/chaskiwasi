// Scaffold test for components/panel. Wave 3A owns the real suite: C-10 and
// C-24 assert the two wipe ORDERINGS against a recording panel, including
// modem-off-first and charger-only wake on the emergency path.
#include <gtest/gtest.h>

#include "chaski/panel.h"

// CoverState is what the cover renderer is allowed to know. The absence of a
// letter count or a sender name here is the enforcement mechanism for B.5:
// the renderer cannot leak what it was never given.
TEST(Panel, CoverStateCarriesNoCountAndNoSender) {
  chaski::panel::CoverState s;
  EXPECT_FALSE(s.any_unread);  // a boolean, deliberately not a count
  EXPECT_EQ(s.kind, chaski::panel::CoverKind::kResting);
  EXPECT_EQ(sizeof(s.any_unread), sizeof(bool));
}
