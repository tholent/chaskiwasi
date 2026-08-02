// app_main — the composition root and the wake dispatcher.
//
// The ESP32-S3 reboots through here on EVERY wake; the modem does not (design
// §6.4). So this function is deliberately cheap: read why we woke, wire the
// seams to the implementations this build variant uses, do exactly one job, and
// go back to sleep.
//
// Everything policy-shaped is somewhere host-tested. Which job a wake exists to
// do is wake.cpp (§6). The order a response is applied in is syncengine (§5.2).
// The write discipline is store. What is left here is wiring, the three things
// that genuinely need silicon — the wake cause, the RTC segment, entering deep
// sleep — and the two seams whose hardware has not been written yet, each an
// honest no-op rather than a fake.
//
// Nothing here may log letter content at any level, in any build (D-7, C-19).
#include <cinttypes>
#include <cstdint>
#include <type_traits>

#include "esp_attr.h"
#include "esp_log.h"
#include "esp_sleep.h"
#include "esp_timer.h"

#include "session.h"
#include "wake.h"

#if CHASKI_DEV_BUILD
#include "bench_control.h"
#endif

namespace {

using chaski::app::Session;
namespace store = chaski::store;
namespace syncengine = chaski::syncengine;
namespace wake = chaski::wake;

constexpr const char* kTag = "chaski";

// §6: the RTC timer wake runs every 10-15 minutes. It is the doorbell poll's
// cadence and NOT the sync interval — sync_interval_s is hours, while the poll
// is a local serial transaction with a modem that buffered the SMS for us.
// Sync happens on a timer wake only when wake::Decide says it is due.
constexpr int kTimerWakeIntervalS = 900;

// The §4.3 wake bookkeeping. RTC_DATA_ATTR keeps it across deep sleep and loses
// it on power-on, which is exactly what wake.h says this struct is for.
//
// It is also lost on a software reset, which the bench's C-4 cut is: that makes
// a cut look like a slightly colder boot than it really was. Conservative in
// the direction that matters — every field is reconstructible and Reconstruct's
// values are the cautious ones — so the recovery C-4 observes is a lower bound
// on the real one.
RTC_DATA_ATTR wake::RtcState g_rtc;

static_assert(std::is_trivially_copyable<wake::RtcState>::value,
              "RtcState lives in the RTC segment, which is a byte range; a type "
              "needing a constructor there would be rebuilt on every boot and the "
              "bookkeeping would silently never survive a sleep");

std::int64_t UptimeMs() { return esp_timer_get_time() / 1000; }

const char* ReasonName(wake::Reason r) {
  switch (r) {
    case wake::Reason::kColdBoot:
      return "cold";
    case wake::Reason::kTimer:
      return "timer";
    case wake::Reason::kKeypress:
      return "key";
    case wake::Reason::kUsb:
      return "usb";
    case wake::Reason::kUnknown:
      return "unknown";
  }
  return "unknown";
}

// WakeReason maps the IDF enum onto §6's, so nothing below this file knows what
// esp_sleep_get_wakeup_cause() returned.
//
// Reason::kUsb is unreachable today and deliberately not faked: a VBUS attach
// on a device in deep sleep is a power-on, indistinguishable here from a cold
// boot, and the charger-detect line that would tell them apart arrives with the
// power work in wave 4C. §6 gives the two the same job anyway — sync if it was
// already due — so nothing is lost by not guessing.
wake::Reason WakeReason() {
  switch (esp_sleep_get_wakeup_cause()) {
    case ESP_SLEEP_WAKEUP_TIMER:
      return wake::Reason::kTimer;
    case ESP_SLEEP_WAKEUP_EXT0:
    case ESP_SLEEP_WAKEUP_EXT1:
    case ESP_SLEEP_WAKEUP_GPIO:
      return wake::Reason::kKeypress;
    case ESP_SLEEP_WAKEUP_UNDEFINED:
      // Not a wake at all: power-on, brownout, or a reset.
      return wake::Reason::kColdBoot;
    default:
      // A source we did not arm. wake::Decide treats it conservatively and
      // never as a key press: an unexplained wake must not put a letter on the
      // glass (D-1).
      return wake::Reason::kUnknown;
  }
}

syncengine::Trigger TriggerFor(wake::SyncCause c) {
  switch (c) {
    case wake::SyncCause::kUserKey:
      return syncengine::Trigger::kUserKey;
    case wake::SyncCause::kDoorbell:
      return syncengine::Trigger::kPututu;
    case wake::SyncCause::kOutboundQueued:
      return syncengine::Trigger::kOutboundQueued;
    case wake::SyncCause::kNone:
    case wake::SyncCause::kFirstBoot:
    case wake::SyncCause::kScheduled:
      return syncengine::Trigger::kScheduled;
  }
  return syncengine::Trigger::kScheduled;
}

// BoardJobs is the hardware half of §6: what a wake is allowed to do, each
// entry pointing at the implementation this build has. Two of them have no
// implementation yet; both say so and do nothing, which is the only honest
// shape for a seam whose hardware is a wave away.
class BoardJobs final : public wake::Jobs {
 public:
  explicit BoardJobs(Session& s) : s_(s) {}

  wake::DoorbellPoll PollDoorbell() override {
    // Draining the modem's SMS buffer needs the modem (wave 4A/4B, §7). Until
    // then this reports NO doorbell — not a fake one. The distinction is the
    // whole point: a stub that answered "yes" would sync on every timer wake
    // and would make C-8 look green while nothing had verified a token.
    ESP_LOGD(kTag, "doorbell poll unavailable: no modem in this build");
    return wake::DoorbellPoll{};
  }

  void Sync(wake::SyncCause cause) override {
    last_ = chaski::app::RunSync(s_, TriggerFor(cause));
  }

  void OpenUiSession() override {
    // The interactive session — screens, compose, the wipe on the way out — is
    // wave 3C's, over the panel wave 3A is writing. On a dev build the session
    // a developer actually gets is the bench control channel, which app_main
    // enters after the wake's job is done.
    ESP_LOGI(kTag, "ui session unavailable: no screens in this build");
  }

  bool AnyUnread() override {
    store::LetterStore* l = s_.letters();
    return l != nullptr && l->UnreadCount() > 0;
  }

  void RenderCover(bool any_unread) override {
    // Cover renderer and panel driver are wave 3. Logging the flag is
    // content-free by construction: it is one bit, which is all the cover ever
    // says about mail (B.5, C-12).
    ESP_LOGI(kTag, "cover render unavailable: no panel in this build (flag=%d)",
             any_unread ? 1 : 0);
  }

  void RenderChargeMeCover() override {
    ESP_LOGI(kTag, "charge-me cover unavailable: no panel in this build");
  }

  const chaski::app::SyncReport& last_sync() const { return last_; }

 private:
  Session& s_;
  chaski::app::SyncReport last_;
};

[[noreturn]] void SleepUntilNextWake() {
  // ext1 wake on the key matrix is wave 3B's: it needs the pin mask, and there
  // is no keyboard on this board yet. A device that sleeps with only the timer
  // armed still wakes, still polls, still raises the flag — it just cannot be
  // woken by a key. That is a missing feature, not a wrong one.
  esp_sleep_enable_timer_wakeup(static_cast<std::uint64_t>(kTimerWakeIntervalS) * 1000000ULL);
  ESP_LOGI(kTag, "sleeping for %ds", kTimerWakeIntervalS);
  esp_deep_sleep_start();
}

}  // namespace

extern "C" void app_main(void) {
  const std::int64_t uptime_ms = UptimeMs();
  const wake::Reason reason = WakeReason();

  // Read intactness before repairing it: Decide treats a lost segment as a
  // reason to sync, and it can only do that if it is told.
  const bool rtc_intact = wake::Intact(g_rtc);
  if (!rtc_intact) wake::Reconstruct(g_rtc, uptime_ms);
  wake::NoteWake(g_rtc, reason);

  ESP_LOGI(kTag, "wake reason=%s boot=%" PRIu32 " rtc=%s", ReasonName(reason), g_rtc.boot_count,
           rtc_intact ? "intact" : "reconstructed");

  Session session;
  if (!session.Open()) {
    // Nothing durable works. Sleeping and retrying is the battery-safe answer;
    // spinning here would flatten the pack behind a cover that says nothing.
    ESP_LOGE(kTag, "session unavailable; sleeping");
    SleepUntilNextWake();
  }

  wake::Inputs in;
  in.reason = reason;
  in.uptime_ms = uptime_ms;
  in.rtc_intact = rtc_intact;
  in.next_sync_due_ms = g_rtc.next_sync_due_ms;
  in.outbound_queued = session.outbox()->SendableCount() > 0;
  // No fuel gauge until wave 4C. "Not below the floor" is the only honest
  // default: the alternative claims a low battery nobody measured, and §6's
  // answer to that is to refuse to open content — permanently, on a device with
  // no way to learn otherwise.
  in.below_battery_floor = false;

  BoardJobs jobs(session);
  const wake::RunResult result = wake::Run(in, g_rtc, jobs);

  if (result.synced) {
    // §5.5: sync_interval_s is the server's to set and the device's to obey.
    // The next due time is bookkeeping app_main owns, because wake::Run does
    // not know what the settings say.
    const store::Settings cfg = session.settings()->Get();
    const int interval_s = cfg.sync_interval_s > store::kMinSyncIntervalS ? cfg.sync_interval_s
                                                                         : store::kMinSyncIntervalS;
    wake::ScheduleNextSync(g_rtc, UptimeMs(), interval_s);
  }

  ESP_LOGI(kTag, "wake done synced=%d ui=%d cover=%d charge_me=%d", result.synced ? 1 : 0,
           result.ui_opened ? 1 : 0, result.cover_rendered ? 1 : 0, result.charge_me ? 1 : 0);

#if CHASKI_DEV_BUILD
  // A board on a bench is attached to a host and to power; deep sleep would end
  // the session and take the console with it. Serving the control channel is
  // this variant's "session" — and it is the one thing that never exists in
  // production, where bench_control.cpp is not compiled at all.
  chaski::bench::BootSummary summary;
  summary.boot_count = static_cast<int>(g_rtc.boot_count);
  summary.wake_reason = ReasonName(reason);
  summary.synced = result.synced;
  summary.fault = chaski::app::FaultName(jobs.last_sync().outcome.fault);
  summary.letters_stored = jobs.last_sync().outcome.letters_stored;
  summary.acks_applied = jobs.last_sync().outcome.acks_applied;
  chaski::bench::Serve(session, summary);
#endif

  SleepUntilNextWake();
}
