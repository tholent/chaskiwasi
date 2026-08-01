// Wake policy: which job this boot exists to do (client §6).
//
// The ESP32-S3 reboots through app_main on every wake, so "what happened while
// we were asleep and what should we do about it" is a decision taken a dozen
// times a day, on a battery, in the dark. It is also the decision no hardware
// here can test: there is no Walter on this bench and deep sleep cannot be
// observed from a host. So the decision lives in this file, which includes no
// esp_* header and is compiled into the host suite; app_main.cpp keeps only the
// parts that genuinely need silicon — reading the wake cause, the RTC segment,
// and entering sleep.
//
// Three rules shape everything below:
//   - exactly one job per wake, then back to sleep (§6)
//   - the radio stays down while composing (§6), so a key wake never syncs
//   - an unexplained wake never opens content (D-1)
#pragma once

#include <cstdint>

namespace chaski::wake {

// Reason is the wake cause, already mapped off the IDF enum so that nothing
// below this header knows what esp_sleep_get_wakeup_cause() returned.
enum class Reason {
  kColdBoot,   // power-on or reset: RTC memory is gone with it
  kTimer,      // the scheduled wake (§6: every 10-15 min)
  kKeypress,   // ext1 on the key matrix / the prototype's interrupt line
  kUsb,        // charger or host attach
  kUnknown,    // a wake source we did not arm; treated conservatively
};

// SyncCause records why a sync is happening, for the engine's Trigger and for
// content-free logging. It changes nothing about the request itself.
enum class SyncCause {
  kNone,
  kFirstBoot,        // the clock is invalid until the first sync (§5.6)
  kScheduled,        // sync_interval_s elapsed
  kDoorbell,         // an accepted pututu token (§7)
  kOutboundQueued,   // a letter is waiting and we woke anyway (§5)
  kUserKey,          // the sync key, from any screen (§10)
};

// RtcState is the §4.3 wake bookkeeping: it survives deep sleep and dies with
// power. Every field is reconstructible or conservative when it is lost, which
// is what makes losing it always safe.
//
// The sync cursor is deliberately NOT here (B.12). It is durable in flash,
// because a flat battery would otherwise cost a 200-letter window resync on a
// per-MB bill.
//
// Trivially copyable on purpose: this struct lives in the RTC slow-memory
// segment, which is a byte range, not a heap.
struct RtcState {
  std::uint32_t magic = 0;
  std::uint32_t boot_count = 0;
  std::int32_t last_reason = 0;         // Reason, widened for a POD segment
  std::uint8_t unread_mirror = 0;       // a boolean for the cover flag (B.5)
  std::uint8_t unread_mirror_valid = 0;
  std::int64_t last_sms_wake_ms = 0;    // doorbell rate limit (§7)
  std::int64_t next_sync_due_ms = 0;    // the scheduled sync (§5)
};

// The magic carries a layout version: a firmware update that changes the
// struct must not read the previous build's bytes as its own.
inline constexpr std::uint32_t kRtcMagic = 0x43483101;  // "CH1", layout 1

// After power loss the doorbell limiter starts conservative: the first minute
// after boot counts as inside the 5-minute window (§7). Cheap insurance —
// the scheduled sync still happens, and the alternative is a device that can
// be woken on demand the instant its battery is swapped.
inline constexpr std::int64_t kBootConservativeWindowMs = 60000;

bool Intact(const RtcState& s);

// Reconstruct fills in the conservative values for a boot that found the RTC
// segment empty. `uptime_ms` shares its zero with the segment's death: both
// are lost at power-off, so the two are always consistent.
void Reconstruct(RtcState& s, std::int64_t uptime_ms);

void NoteWake(RtcState& s, Reason r);
bool SmsWakeAllowed(const RtcState& s, std::int64_t uptime_ms);
void NoteSmsWake(RtcState& s, std::int64_t uptime_ms);
void ScheduleNextSync(RtcState& s, std::int64_t uptime_ms, int interval_s);
void SetUnreadMirror(RtcState& s, bool any_unread);

// CoverNeedsRerender is §6's "after any background sync that stored new
// letters, the cover is re-rendered (flag up) before returning to sleep",
// stated as the general case: the glass is wrong whenever the mirror
// disagrees with the store, or whenever the mirror was lost with the power.
bool CoverNeedsRerender(const RtcState& s, bool any_unread);

struct Inputs {
  Reason reason = Reason::kColdBoot;
  std::int64_t uptime_ms = 0;
  bool rtc_intact = false;
  std::int64_t next_sync_due_ms = 0;
  bool outbound_queued = false;   // the outbox has something to send (§5.1)
  bool below_battery_floor = false;  // < 5% SOC (§6, §9)
};

struct Plan {
  bool poll_doorbell = false;
  bool sync = false;
  SyncCause cause = SyncCause::kNone;
  bool open_ui = false;
  bool charge_me = false;  // wipe to the charge-me cover, open nothing
};

Plan Decide(const Inputs& in);

// DoorbellPoll is what draining the modem's SMS buffer found. `received`
// counts against the rate limit regardless of validity (§7), so a flood of
// forgeries cannot buy an attacker more syncs than one honest token would.
struct DoorbellPoll {
  bool received = false;
  bool accepted = false;
};

// Jobs is everything a wake is allowed to do. It exists so that Run — the
// order in which those things happen — is testable with no modem, no panel,
// and no battery. The implementations live in app_main.cpp.
class Jobs {
 public:
  virtual ~Jobs() = default;

  // PollDoorbell drains buffered SMS and verifies each token (§7). Failures
  // are silent by contract; this reports only what the limiter needs.
  virtual DoorbellPoll PollDoorbell() = 0;

  virtual void Sync(SyncCause cause) = 0;

  // OpenUiSession runs the interactive session and returns when it has ended
  // — timeout, put-away, or the low-battery path. The wipe happens inside it,
  // because the wipe controller owns the panel (client §9).
  virtual void OpenUiSession() = 0;

  virtual bool AnyUnread() = 0;
  virtual void RenderCover(bool any_unread) = 0;
  virtual void RenderChargeMeCover() = 0;
};

struct RunResult {
  bool doorbell_accepted = false;
  bool synced = false;
  SyncCause cause = SyncCause::kNone;
  bool ui_opened = false;
  bool cover_rendered = false;
  bool charge_me = false;
};

// Run performs the plan and updates the RTC bookkeeping it owns. It does not
// sleep: entering deep sleep is app_main's, because it never returns.
RunResult Run(const Inputs& in, RtcState& rtc, Jobs& jobs);

}  // namespace chaski::wake
