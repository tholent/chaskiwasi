// Implementation of components/power — see include/chaski/power.h for the
// contract and the spec clauses it must satisfy.
//
// Two paths live here and they fail in opposite directions on purpose (client
// §9.1, B.13):
//
//   graceful  — a software poll of an estimate that can drift. It fires once
//               per crossing and does not fire at all on an unreadable gauge,
//               because a broken sensor must not wipe the screen every second.
//   emergency — a hardware alert. Doubt resolves toward wiping: an unnecessary
//               emergency wipe destroys nothing (letters are in encrypted
//               flash, the draft is autosaved), while a missed one leaves a
//               letter legible on a dead panel indefinitely.
//
// Nothing here logs. This component sees battery numbers rather than letters,
// but a component with no logging statement cannot acquire a D-7 defect by
// accident, and the handler it drives runs next to the panel.
#include "chaski/power.h"

#include <atomic>
#include <utility>

namespace chaski::power {
namespace {

class MonitorImpl final : public Monitor {
 public:
  explicit MonitorImpl(MonitorDeps d) : deps_(std::move(d)) {}

  ~MonitorImpl() override { Disarm(); }

  void BeginSession() override {
    if (session_) return;
    session_ = true;
    Sample(/*force=*/true);
    Arm();
  }

  void EndSession() override {
    if (!session_) return;
    session_ = false;
    Disarm();
  }

  // §9: the wipe fires on the crossing, not on every poll below the line —
  // otherwise the device would re-wipe a resting charge-me cover forever.
  bool GracefulThresholdCrossed() override {
    if (!session_) return false;
    const Reading r = Sample(/*force=*/false);
    if (!r.valid) return false;  // the hardware alert backstops a lying gauge
    if (r.charging) {
      latched_ = false;
      return false;
    }
    if (r.soc_pct >= kGracefulSocPct) return false;
    if (latched_) return false;
    latched_ = true;
    return true;
  }

  bool ChargeMeLatched() const override { return latched_; }

  bool EmergencyPending() const override { return alert_pending_.load(); }

  EmergencyOutcome ServiceEmergency() override {
    if (!alert_pending_.exchange(false)) return EmergencyOutcome::kNoAlert;

    // §9.1 step 2, ahead of everything else: the burst load is what dragged
    // the rail down, and the confirming re-read means nothing until it is gone.
    if (deps_.radio_off) deps_.radio_off();

    if (Recovered()) {
      // The pack came back with margin — an LTE burst or a cold cell, not a
      // dying battery. Acknowledge the latched alert and stay armed: the
      // threshold has not moved and the next real sag must still be caught.
      if (deps_.gauge != nullptr) deps_.gauge->ClearAlert();
      Arm();
      return EmergencyOutcome::kCancelled;
    }

    if (deps_.on_emergency) deps_.on_emergency();
    return EmergencyOutcome::kWiped;
  }

  Reading Last() const override { return last_; }

 private:
  // Recovered performs the single confirming re-read of §9.1. Anything more
  // elaborate — averaging, a second chance, a retry after an I2C failure — is
  // risk in the wrong direction, so every uncertain answer is "not recovered".
  bool Recovered() {
    if (deps_.gauge == nullptr || !deps_.delay_ms) return false;
    deps_.delay_ms(kEmergencyConfirmDelayMs);
    const Reading r = deps_.gauge->Read();
    last_ = r;
    if (!r.valid) return false;
    return r.millivolts >= kEmergencyMilliVolts + kEmergencyConfirmMarginMv;
  }

  void Arm() {
    if (deps_.gauge == nullptr) return;
    deps_.gauge->ArmUndervoltAlert(kEmergencyMilliVolts, [this]() { OnAlert(); });
  }

  void Disarm() {
    if (deps_.gauge != nullptr) deps_.gauge->DisarmUndervoltAlert();
  }

  // OnAlert runs in interrupt context on the target. It sets a flag and hands
  // off; the sequence it triggers needs I2C, a panel, and seconds of waveform.
  void OnAlert() {
    alert_pending_.store(true);
    if (deps_.on_alert_isr) deps_.on_alert_isr();
  }

  Reading Sample(bool force) {
    if (deps_.gauge == nullptr) return last_;
    const std::int64_t now = deps_.monotonic_ms ? deps_.monotonic_ms() : 0;
    if (!force && have_sample_ && now - last_sample_ms_ < kGaugePollIntervalMs) {
      return last_;
    }
    last_ = deps_.gauge->Read();
    last_sample_ms_ = now;
    have_sample_ = true;
    return last_;
  }

  MonitorDeps deps_;
  Reading last_;
  std::atomic<bool> alert_pending_{false};
  std::int64_t last_sample_ms_ = 0;
  bool have_sample_ = false;
  bool session_ = false;
  bool latched_ = false;
};

}  // namespace

std::unique_ptr<Monitor> NewMonitor(const MonitorDeps& d) {
  return std::unique_ptr<Monitor>(new MonitorImpl(d));
}

}  // namespace chaski::power
