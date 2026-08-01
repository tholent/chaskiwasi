// C-10 and C-24 (sequence half): the wipe orderings.
//
// These are privacy tests, not cosmetic ones. A wipe that runs its steps in the
// wrong order returns successfully and leaves either a legible letter on the
// glass or a panel frozen mid-transition, and nothing in software can tell the
// difference. So the assertions are about ORDER, against one interleaved trace.
//
// The live halves — a real panel, a real battery, and the angled-light ghosting
// measurement that sets the pass count — are bench work needing hardware that
// does not exist yet (docs/bringup.md).
#include <gtest/gtest.h>

#include <cstddef>
#include <memory>
#include <string>
#include <vector>

#include "chaski/panel.h"
#include "recording_panel.h"

namespace {

using chaski::panel::CoverKind;
using chaski::panel::CoverState;
using chaski::testing::Trace;

// Rig wires the controller to recorders sharing one trace.
struct Rig {
  Trace trace;
  chaski::testing::RecordingPanel panel{&trace};
  chaski::testing::RecordingRail rail{&trace};
  chaski::testing::RecordingRadio radio{&trace};
  chaski::testing::RecordingSleeper sleeper{&trace};

  bool voltage_holds = true;
  int cover_renders = 0;
  CoverState last_cover;
  int input_masked = 0;

  std::unique_ptr<chaski::panel::WipeController> New() {
    chaski::panel::WipeDeps d;
    d.panel = &panel;
    d.rail = &rail;
    d.radio = &radio;
    d.sleeper = &sleeper;
    d.render_cover = [this](const CoverState& c, chaski::panel::Framebuf&) {
      ++cover_renders;
      last_cover = c;
    };
    d.voltage_ok = [this] { return voltage_holds; };
    d.mask_input = [this] { ++input_masked; };
    return chaski::panel::NewWipeController(d);
  }
};

CoverState Resting() {
  CoverState c;
  c.kind = CoverKind::kResting;
  c.battery_pct = 64;
  c.any_unread = true;
  return c;
}

// Before asserts ordering, not adjacency: the spec constrains sequence, and
// demanding adjacency would fail on any harmless extra step.
void Before(const Trace& t, const std::string& a, const std::string& b) {
  const int ia = t.IndexOf(a);
  const int ib = t.IndexOf(b);
  ASSERT_GE(ia, 0) << "missing step: " << a;
  ASSERT_GE(ib, 0) << "missing step: " << b;
  EXPECT_LT(ia, ib) << a << " must happen before " << b;
}

}  // namespace

// §9: flush -> busy -> cover -> busy -> panel sleep -> rail cut -> MCU sleep.
TEST(C10, GracefulWipeFollowsTheSpecifiedOrder) {
  Rig rig;
  rig.New()->GracefulWipe(Resting());

  const std::vector<std::string> expected = {
      "flush", "busy", "full", "busy", "panel_sleep", "rail_cut", "mcu_sleep"};
  EXPECT_EQ(rig.trace.steps, expected);
}

// The panel must be asleep before its supply is cut, and the MCU must sleep
// last. Cutting the rail on a controller that has not taken its deep-sleep
// command can leave the panel undefined at the next power-up.
TEST(C10, PanelSleepsBeforeItsRailIsCut) {
  Rig rig;
  rig.New()->GracefulWipe(Resting());
  Before(rig.trace, "panel_sleep", "rail_cut");
  Before(rig.trace, "rail_cut", "mcu_sleep");
}

// A waveform is not finished when the call returns. Every render is followed by
// a BUSY wait before anything else touches the panel; without it the cover is
// drawn over a flush still in progress and the device sleeps mid-transition.
TEST(C10, EveryRenderIsFollowedByABusyWait) {
  Rig rig;
  rig.New()->GracefulWipe(Resting());

  const auto& s = rig.trace.steps;
  for (std::size_t i = 0; i < s.size(); ++i) {
    if (s[i] == "flush" || s[i] == "full") {
      ASSERT_LT(i + 1, s.size()) << "a render is the last step; nothing awaited it";
      EXPECT_EQ(s[i + 1], "busy") << "render at step " << i << " was not awaited";
    }
  }
}

// design §4.1: a single full refresh is not a wipe. The flush must be
// multi-pass, or residue stays legible under angled light.
TEST(C10, TheFlushIsMultiPass) {
  Rig rig;
  rig.New()->GracefulWipe(Resting());
  EXPECT_GE(rig.panel.last_flush_passes, 2)
      << "one pass is a refresh, not a wipe (design §4.1)";
}

// The graceful path leaves an ordinary sleep the child can wake from.
TEST(C10, AGracefulWipeLeavesTheDeviceWakeable) {
  Rig rig;
  rig.New()->GracefulWipe(Resting());
  EXPECT_FALSE(rig.sleeper.charger_only);
}

// B.5: the renderer is handed CoverState and nothing else, so it cannot leak a
// count or a sender it was never given.
TEST(C12, TheCoverRendererLearnsNothingItCouldLeak) {
  Rig rig;
  rig.New()->GracefulWipe(Resting());

  ASSERT_EQ(rig.cover_renders, 1);
  EXPECT_TRUE(rig.last_cover.any_unread);  // a boolean...
  EXPECT_EQ(rig.last_cover.battery_pct, 64);
  // ...and there is deliberately no count and no sender field to assert on.
  static_assert(sizeof(CoverState::any_unread) == sizeof(bool),
                "the mail flag is a boolean; a count is a conversation someone "
                "else can start (design §11, B.5)");
}

// The charge-me cover is the same composition with the battery emphasised, and
// a low battery does not excuse skipping the wipe (client §9, D-1).
TEST(C12, TheChargeMeCoverIsStillACover) {
  Rig rig;
  CoverState c;
  c.kind = CoverKind::kChargeMe;
  c.battery_pct = 3;
  c.any_unread = true;
  rig.New()->GracefulWipe(c);

  ASSERT_EQ(rig.cover_renders, 1);
  EXPECT_EQ(rig.last_cover.kind, CoverKind::kChargeMe);
  EXPECT_TRUE(rig.trace.Contains("flush"));
}

// §9.1: mask input -> radio off -> busy -> flush -> busy -> cover -> busy ->
// panel sleep -> rail cut -> MCU sleep waking on charger only.
TEST(C24, EmergencyWipeFollowsTheSpecifiedOrder) {
  Rig rig;
  rig.New()->EmergencyWipe(Resting());

  const std::vector<std::string> expected = {
      "radio_off",   "busy",     "flush",
      "busy",        "full",     "busy",
      "panel_sleep", "rail_cut", "mcu_sleep_charger_only"};
  EXPECT_EQ(rig.trace.steps, expected);
  EXPECT_EQ(rig.input_masked, 1);
}

// The radio goes down BEFORE the flush. Removing the transmit burst is what
// lets the pack's voltage recover, and that recovery is the headroom the flush
// spends — doing it afterwards would be doing it after the step that needed it.
TEST(C24, RadioIsDroppedBeforeTheFlushDrawsCurrent) {
  Rig rig;
  rig.New()->EmergencyWipe(Resting());
  Before(rig.trace, "radio_off", "flush");
}

// A waveform in flight is awaited, never aborted: a panel frozen
// mid-transition is worse than one never wiped, because a half-transitioned
// frame is still a readable frame (design §4.1).
TEST(C24, AnInFlightWaveformIsAwaitedBeforeTheFlush) {
  Rig rig;
  rig.New()->EmergencyWipe(Resting());
  Before(rig.trace, "busy", "flush");
}

// The cover is the one step this path may skip. If the voltage will not hold,
// stopping on white is correct: "never a blank panel" is UX guidance for a
// resting device, while "reveals nothing" is the invariant.
TEST(C24, TheCoverIsSacrificedRatherThanTheFlush) {
  Rig rig;
  rig.voltage_holds = false;
  rig.New()->EmergencyWipe(Resting());

  EXPECT_EQ(rig.cover_renders, 0);
  EXPECT_FALSE(rig.trace.Contains("full"));
  EXPECT_TRUE(rig.trace.Contains("flush")) << "the flush must still happen";
  Before(rig.trace, "flush", "panel_sleep");
  EXPECT_TRUE(rig.trace.Contains("mcu_sleep_charger_only"));
}

// After an emergency wipe the device must not wake on a key press: a curious
// child could otherwise drain the pack past the brownout floor, where no wipe
// can run at all (B.13).
TEST(C24, AfterAnEmergencyWipeTheDeviceWakesOnlyOnCharge) {
  Rig rig;
  rig.New()->EmergencyWipe(Resting());
  EXPECT_TRUE(rig.sleeper.charger_only);
  EXPECT_FALSE(rig.trace.Contains("mcu_sleep"))
      << "a key-wakeable sleep was entered";
}

// Both paths must survive being handed nothing. The emergency caller may be a
// fault handler with a half-built world, and crashing there would leave the
// letter on the glass — the exact outcome the path exists to prevent.
TEST(C24, AWipeWithMissingDependenciesDoesNotCrash) {
  chaski::panel::WipeDeps empty;
  auto w = chaski::panel::NewWipeController(empty);
  w->GracefulWipe(Resting());
  w->EmergencyWipe(Resting());
  SUCCEED() << "no dependency is dereferenced blindly";
}
