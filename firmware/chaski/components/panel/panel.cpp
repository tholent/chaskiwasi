// WipeController — the two orderings that decide what a stranger can read off
// this device (client §9, §9.1).
//
// Everything here is sequencing. There is no cleverness to find and none to
// add: each order is specified, each step exists because skipping it leaves
// either a legible letter or a frozen panel, and the tests assert the ORDER
// rather than the outcome, because the outcome is invisible from software.
//
// Why the two paths differ at all: the graceful wipe runs when the device
// decides to sleep and can take its time. The emergency wipe runs when the
// battery is about to vanish underneath it, so it drops the radio first to buy
// voltage headroom, and gives up the cover screen rather than the flush if it
// has to choose (§9.1, B.13).
#include "chaski/panel.h"

#include <utility>

namespace chaski::panel {
namespace {

class WipeControllerImpl final : public WipeController {
 public:
  explicit WipeControllerImpl(WipeDeps d) : d_(std::move(d)) {}

  // §9, the seven steps. The pairing that matters is flush-then-wait and
  // render-then-wait: an e-ink waveform is not finished when the call returns,
  // so every render is followed by a BUSY wait before anything else touches the
  // panel. The tail is ordered by what breaks — the controller must be asleep
  // before its supply is cut, and the MCU sleeps last because afterwards it
  // cannot do anything at all.
  void GracefulWipe(const CoverState& cover) override {
    if (d_.mask_input) d_.mask_input();

    // A single full refresh is NOT a wipe: after a long compose session of
    // partial refreshes, residue stays legible under angled light (design
    // §4.1). The pass count is a measured privacy parameter, not a constant to
    // trust — see docs/bringup.md.
    Flush(d_.graceful_flush_passes);

    // Rendered from CoverState alone, which carries no count and no sender:
    // the renderer cannot leak what it was never given (B.5).
    RenderCover(cover);

    Park(/*wake_on_charger_only=*/false);
  }

  // §9.1, the eight steps. Every difference from the graceful path is about
  // finishing before the battery does.
  void EmergencyWipe(const CoverState& cover) override {
    // Input first: this path exists precisely for the case where the UI is not
    // healthy, so a wedged screen must not be able to touch the panel mid-wipe.
    if (d_.mask_input) d_.mask_input();

    // Radio off BEFORE anything else draws current. Not hygiene: removing the
    // transmit burst lets the pack's voltage recover, and that recovery is the
    // headroom the flush then spends. Doing it after the flush would be doing
    // it after the step that needed it.
    if (d_.radio) d_.radio->PowerDownRadio();

    // Never abort a waveform in flight. Aborting freezes the panel
    // mid-transition, which is worse than not wiping at all (design §4.1): a
    // half-transitioned frame is still a readable frame.
    if (d_.panel) d_.panel->WaitBusy();

    // The step that must complete. Everything after it is improvement.
    Flush(d_.emergency_flush_passes);

    // The cover is optional here, and only here. If the voltage will not hold,
    // stopping on white is correct: "never a blank white panel" is UX guidance
    // for a resting device (§9), while "reveals nothing" is the invariant, and
    // a dead white screen satisfies the invariant.
    if (!d_.voltage_ok || d_.voltage_ok()) RenderCover(cover);

    // Wake on charger only. A key-wake here lets a curious child drain the
    // pack past the brownout floor, where no wipe can run at all.
    Park(/*wake_on_charger_only=*/true);
  }

 private:
  void Flush(int passes) {
    if (d_.panel == nullptr) return;
    d_.panel->ClearFlush(passes);
    d_.panel->WaitBusy();
  }

  void RenderCover(const CoverState& cover) {
    if (d_.panel == nullptr) return;
    Framebuf fb;
    if (d_.render_cover) d_.render_cover(cover, fb);
    d_.panel->FullRefresh(fb);
    d_.panel->WaitBusy();
  }

  // Park is the tail both paths share: panel asleep, then its rail cut, then
  // the MCU. Cutting the rail before the controller has taken its deep-sleep
  // command can leave the panel in an undefined state at the next power-up.
  void Park(bool wake_on_charger_only) {
    if (d_.panel) d_.panel->DeepSleep();
    if (d_.rail) d_.rail->Cut();
    if (d_.sleeper) d_.sleeper->DeepSleep(wake_on_charger_only);
  }

  WipeDeps d_;
};

}  // namespace

std::unique_ptr<WipeController> NewWipeController(const WipeDeps& d) {
  return std::make_unique<WipeControllerImpl>(d);
}

}  // namespace chaski::panel
