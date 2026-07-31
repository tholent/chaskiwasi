// Package panel drives the e-ink display and owns the wipe sequences.
//
// E-ink bistability is a privacy hazard, not just a power win: the last image
// persists with no power at all, including after a flat battery, in a bag, in
// someone else's hands (design §4.1). Everything here exists to make sure what
// persists is a cover screen and never a letter (D-1).
//
// Two orderings are specified and neither is negotiable:
//   GracefulWipe  — client §9, seven steps
//   EmergencyWipe — client §9.1, eight steps, hardware-alert driven (B.13)
// Getting them wrong freezes the panel mid-waveform, which is worse than not
// wiping at all. C-10 and C-24 assert them against RecordingPanel.
#pragma once

#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <vector>

namespace chaski::panel {

struct Rect {
  int x = 0, y = 0, w = 0, h = 0;
};

// Framebuf is 1-bit packed for compose and reading; 4-grey is a v1 option for
// reading screens only (client §8.1).
struct Framebuf {
  int w = 0, h = 0;
  std::vector<std::uint8_t> bits;
};

// Panel is the driver seam. RecordingPanel implements it in host tests, which
// is how the wipe ORDER is asserted without hardware.
class Panel {
 public:
  virtual ~Panel() = default;

  virtual void PartialRefresh(const Rect& r, const Framebuf& fb) = 0;
  virtual void FastRefresh(const Framebuf& fb) = 0;
  virtual void FullRefresh(const Framebuf& fb) = 0;

  // ClearFlush runs the alternating black/white flush. A single full refresh is
  // NOT a wipe: after a long partial-refresh session, residue stays legible
  // under angled light (design §4.1). Pass count is a measured privacy
  // parameter, tuned by the ghosting procedure in docs/bringup.md.
  virtual void ClearFlush(int passes) = 0;

  // WaitBusy blocks until BUSY deasserts. Callers MUST wait before starting a
  // new waveform; aborting one mid-flight is the failure this API exists to
  // make hard (client §9.1 step 3).
  virtual void WaitBusy() = 0;

  // DeepSleep issues the controller's deep-sleep command. It must happen
  // BEFORE the peripheral rail is cut, never after (client §9 steps 5-6).
  virtual void DeepSleep() = 0;
};

// Rail is the MOSFET-switched peripheral supply feeding panel and frontlight
// (design §5.5). Cutting it is a step in both wipe orderings, always after the
// panel is asleep.
class Rail {
 public:
  virtual ~Rail() = default;
  virtual void PowerOn() = 0;
  virtual void Cut() = 0;
};

// CoverKind selects what is left on the glass. None of these may show content,
// a sender, or a count — the mail flag is a boolean, never a number (B.5).
enum class CoverKind {
  kResting,   // wordmark/road motif + battery + mail-flag glyph when unread
  kChargeMe,  // same composition, battery emphasised (client §9)
};

// CoverState is everything the cover renderer is allowed to know. The absence
// of a letter count or sender name here is the enforcement mechanism for B.5:
// the renderer cannot leak what it was never given.
struct CoverState {
  CoverKind kind = CoverKind::kResting;
  int battery_pct = 0;
  bool charging = false;
  bool any_unread = false;  // a boolean, deliberately not a count
};

// Sleeper abstracts the MCU sleep entry so the controller is host-testable.
class Sleeper {
 public:
  virtual ~Sleeper() = default;
  // DeepSleep never returns on the target. wake_on_charger_only=true is the
  // emergency path: after an emergency wipe the device must NOT wake on keys,
  // or a curious key-press can drain the pack past the brownout floor
  // (client §9.1 step 8, B.13).
  virtual void DeepSleep(bool wake_on_charger_only) = 0;
};

// RadioOff lets the wipe controller drop the modem before flushing. In the
// emergency path this is not hygiene: removing the burst load lets battery
// voltage recover, buying the headroom to finish the flush (client §9.1).
class RadioOff {
 public:
  virtual ~RadioOff() = default;
  virtual void PowerDownRadio() = 0;
};

// WipeController owns both orderings. It is the only place that sequences
// panel, rail, radio, and sleep — precisely so the order lives in one
// reviewable function instead of scattered across UI code paths.
class WipeController {
 public:
  virtual ~WipeController() = default;

  // GracefulWipe: clear flush -> WaitBusy -> render cover -> WaitBusy ->
  // panel DeepSleep -> cut rail -> MCU deep sleep (client §9).
  virtual void GracefulWipe(const CoverState& cover) = 0;

  // EmergencyWipe: mask input -> radio off -> WaitBusy (never abort a
  // waveform) -> flush -> cover-if-voltage-holds else stop on white -> panel
  // DeepSleep -> cut rail -> MCU deep sleep waking on charger ONLY
  // (client §9.1, B.13, C-24).
  virtual void EmergencyWipe(const CoverState& cover) = 0;
};

struct WipeDeps {
  Panel* panel = nullptr;
  Rail* rail = nullptr;
  Sleeper* sleeper = nullptr;
  RadioOff* radio = nullptr;
  // render_cover paints the cover into a framebuffer. It receives only
  // CoverState, so it cannot render what it was not given.
  std::function<void(const CoverState&, Framebuf&)> render_cover;
  // voltage_ok reports whether there is headroom for the optional cover pass
  // in the emergency path (client §9.1 step 5).
  std::function<bool()> voltage_ok;
  // mask_input stops the UI from touching the panel mid-wipe.
  std::function<void()> mask_input;

  int graceful_flush_passes = 4;
  int emergency_flush_passes = 4;  // two black/white cycles (client §9.1)
};

std::unique_ptr<WipeController> NewWipeController(const WipeDeps& d);

}  // namespace chaski::panel
