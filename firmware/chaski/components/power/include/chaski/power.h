// Package power owns battery truth and the low-voltage backstop.
//
// Three layers protect the screen, and they are not redundant (client §9.1,
// decision B.13):
//   graceful   — ~5% SOC, software poll, full wipe to the charge-me cover
//   emergency  — hardware undervoltage alert (~3.3V) via the gauge's ALRT pin
//   last resort— the MCU brownout detector; too late to drive a waveform
//
// The middle layer exists because the first one assumes things that can be
// false: that the gauge's SOC estimate is honest, and that firmware is
// healthy. A hardware interrupt survives both being wrong.
//
// Scope: the alert is armed while AWAKE only. A sleeping device already ran the
// wipe and shows the cover, which is safe past battery death by design (D-1),
// so the backstop needs no deep-sleep wake path and costs nothing at idle.
#pragma once

#include <functional>
#include <memory>

namespace chaski::power {

struct Reading {
  int soc_pct = 0;
  int millivolts = 0;
  bool charging = false;
  bool valid = false;  // false when the gauge has not been read successfully
};

// Gauge is the fuel-gauge seam (MAX17048 class on the real board, a fake in
// host tests). Sampling rate matters: hibernate samples VCELL only every ~45s,
// which is fine asleep and useless awake, so implementations run active
// sampling during sessions (client §2).
class Gauge {
 public:
  virtual ~Gauge() = default;
  virtual Reading Read() = 0;

  // ArmUndervoltAlert configures the gauge's VALRT threshold and wires its
  // ALRT pin to `cb`. Callable only while awake; implementations disarm on
  // sleep. `cb` runs in interrupt context on the target, so it must do nothing
  // but hand off to the wipe path.
  virtual bool ArmUndervoltAlert(int millivolts, std::function<void()> cb) = 0;

  virtual void DisarmUndervoltAlert() = 0;

  // ClearAlert acknowledges the latched alert over I2C.
  virtual void ClearAlert() = 0;
};

// Thresholds. The emergency value is a backstop, deliberately below where the
// graceful path fires, and is FROZEN ONLY after the bring-up cold-battery sag
// measurement (client §16) — a cold cell reads low at every state of charge,
// and an LTE burst can sag the rail through it momentarily.
inline constexpr int kGracefulSocPct = 5;          // client §9
inline constexpr int kEmergencyMilliVolts = 3300;  // client §9.1, provisional

// Debounce policy, stated as code because the reasoning is easy to invert:
// a FALSE POSITIVE emergency wipe destroys nothing — letters are in encrypted
// flash and the draft is autosaved — so the design biases toward wiping on
// doubt. At most one confirming re-read after the radio is off; anything more
// elaborate is risk in the wrong direction (client §9.1).
inline constexpr int kEmergencyConfirmDelayMs = 250;
inline constexpr int kEmergencyConfirmMarginMv = 100;

// Monitor polls the gauge during a session for the graceful threshold and owns
// the armed hardware alert for the emergency one.
class Monitor {
 public:
  virtual ~Monitor() = default;

  // BeginSession arms the hardware alert and starts active sampling.
  virtual void BeginSession() = 0;

  // EndSession disarms before sleep.
  virtual void EndSession() = 0;

  // Poll is called from the UI loop; it returns true when the graceful
  // threshold has been crossed and the caller should wipe to the charge-me
  // cover and refuse to open content (client §9).
  virtual bool GracefulThresholdCrossed() = 0;

  virtual Reading Last() const = 0;
};

struct MonitorDeps {
  Gauge* gauge = nullptr;
  // on_emergency runs the §9.1 sequence. Set by main/ to the wipe controller's
  // EmergencyWipe; it must be safe to invoke from a high-priority context
  // independent of UI dispatch (client §9.1, C-24).
  std::function<void()> on_emergency;
  std::function<int()> monotonic_ms;
};

std::unique_ptr<Monitor> NewMonitor(const MonitorDeps& d);

}  // namespace chaski::power
