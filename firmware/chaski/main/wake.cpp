// Implementation of the wake policy — see wake.h.
//
// No esp_* header, deliberately (implementation-plan ground rule 3): this is
// the half of the wake path that can be tested without a board, and the half
// that would otherwise be untestable forever, since deep sleep cannot be
// observed from a host and there is no hardware on this bench.
#include "wake.h"

#include "chaski/pututu.h"

namespace chaski::wake {
namespace {

// SyncIsDue folds the three reasons a background wake spends the radio (§5):
// a letter is waiting, the interval elapsed, or we have no idea when it last
// elapsed because the RTC segment died with the power.
bool SyncIsDue(const Inputs& in, SyncCause& cause) {
  if (in.outbound_queued) {
    cause = SyncCause::kOutboundQueued;
    return true;
  }
  if (!in.rtc_intact || in.uptime_ms >= in.next_sync_due_ms) {
    cause = SyncCause::kScheduled;
    return true;
  }
  return false;
}

}  // namespace

bool Intact(const RtcState& s) { return s.magic == kRtcMagic; }

void Reconstruct(RtcState& s, std::int64_t uptime_ms) {
  s = RtcState{};
  s.magic = kRtcMagic;
  s.last_reason = static_cast<std::int32_t>(Reason::kColdBoot);
  // Due now: the clock is invalid until the first sync of a power cycle
  // (§5.6), so dates render blank and outbound letters have no timestamp
  // until this happens.
  s.next_sync_due_ms = uptime_ms;
  // The limiter starts inside its window and leaves it one minute from now.
  s.last_sms_wake_ms =
      uptime_ms + kBootConservativeWindowMs - pututu::kMinWakeIntervalMs;
  // The mail flag is reconstructible from the letter store; until it is read,
  // the mirror is not to be trusted.
  s.unread_mirror_valid = 0;
}

void NoteWake(RtcState& s, Reason r) {
  s.last_reason = static_cast<std::int32_t>(r);
  ++s.boot_count;
}

bool SmsWakeAllowed(const RtcState& s, std::int64_t uptime_ms) {
  return uptime_ms - s.last_sms_wake_ms >= pututu::kMinWakeIntervalMs;
}

void NoteSmsWake(RtcState& s, std::int64_t uptime_ms) {
  s.last_sms_wake_ms = uptime_ms;
}

void ScheduleNextSync(RtcState& s, std::int64_t uptime_ms, int interval_s) {
  s.next_sync_due_ms = uptime_ms + static_cast<std::int64_t>(interval_s) * 1000;
}

void SetUnreadMirror(RtcState& s, bool any_unread) {
  s.unread_mirror = any_unread ? 1 : 0;
  s.unread_mirror_valid = 1;
}

bool CoverNeedsRerender(const RtcState& s, bool any_unread) {
  if (s.unread_mirror_valid == 0) return true;
  return (s.unread_mirror != 0) != any_unread;
}

Plan Decide(const Inputs& in) {
  Plan p;

  // The battery floor outranks every wake reason (§6, §9). Below 5% the device
  // refuses to open content and does not spend the radio — the one job left is
  // showing the child why, and e-ink keeps showing it after the pack is flat.
  if (in.below_battery_floor) {
    p.charge_me = true;
    return p;
  }

  switch (in.reason) {
    case Reason::kKeypress:
      // The child pressed a key: open the UI, and leave the radio down. The
      // sync key syncs from inside the session (§10); composing with the modem
      // up is the largest avoidable draw on this device (§6, design §7).
      p.open_ui = true;
      break;

    case Reason::kTimer:
      // The doorbell is the whole reason the timer wake polls the modem: SMS
      // is buffered while the ESP32 sleeps, so a local serial transaction is
      // all it costs to learn there is mail (§7, design §6.4).
      p.poll_doorbell = true;
      p.sync = SyncIsDue(in, p.cause);
      break;

    case Reason::kUsb:
      // An attach is a charging event, not a request to show content: the
      // device is on a desk or in a bag with a cable, and nobody asked for the
      // inbox. Sync only if it was already due.
      p.sync = SyncIsDue(in, p.cause);
      break;

    case Reason::kColdBoot:
      // First boot of a power cycle: sync unconditionally. It disciplines the
      // clock (§5.6), rebuilds the cover, and is the cheapest way to be
      // correct after a battery swap. No doorbell poll — a sync is happening
      // anyway, and buffered SMS keeps until the next timer wake.
      p.sync = true;
      p.cause = SyncCause::kFirstBoot;
      break;

    case Reason::kUnknown:
      // A wake we did not arm. Treat it as a timer wake and never as a key
      // press: an unexplained wake must not put a letter on the glass (D-1).
      p.poll_doorbell = true;
      p.sync = SyncIsDue(in, p.cause);
      break;
  }
  return p;
}

RunResult Run(const Inputs& in, RtcState& rtc, Jobs& jobs) {
  RunResult out;
  const Plan p = Decide(in);

  if (p.charge_me) {
    jobs.RenderChargeMeCover();
    out.charge_me = true;
    out.cover_rendered = true;
    return out;
  }

  bool sync = p.sync;
  out.cause = p.cause;

  if (p.poll_doorbell) {
    const DoorbellPoll db = jobs.PollDoorbell();
    // Allowance is read BEFORE the limiter is charged, so one poll cannot both
    // consume the window and be refused by it.
    const bool allowed = SmsWakeAllowed(rtc, in.uptime_ms);

    // Charge the limiter only when the window was open. "Regardless of
    // validity" (§7) means a forged or replayed token still spends the window
    // it was allowed to spend — an attacker must not buy extra wakes with
    // garbage. It does NOT mean re-charging a window that is already closed:
    // doing that slides the window forward on every arrival, so a token every
    // four minutes would suppress the doorbell indefinitely. That trades the
    // battery-drain attack the limiter defends against for a
    // delay-the-child's-letters attack, which is worse — letters would then
    // wait for the six-hourly scheduled sync with nothing visibly wrong.
    if (db.received && allowed) NoteSmsWake(rtc, in.uptime_ms);

    if (db.accepted && allowed) {
      out.doorbell_accepted = true;
      sync = true;
      out.cause = SyncCause::kDoorbell;
    }
  }

  if (sync) {
    jobs.Sync(out.cause);
    out.synced = true;
  }

  if (p.open_ui) {
    // The session owns the panel from here: it renders, and its wipe leaves
    // the cover behind on the way out (§9). Nothing below re-renders it.
    jobs.OpenUiSession();
    out.ui_opened = true;
    return out;
  }

  // §6: the mail flag is the whole point of the timer wake, so the cover is
  // re-rendered whenever the glass disagrees with the store — which includes
  // every boot that lost the mirror with the power.
  const bool unread = jobs.AnyUnread();
  if (CoverNeedsRerender(rtc, unread)) {
    jobs.RenderCover(unread);
    out.cover_rendered = true;
  }
  SetUnreadMirror(rtc, unread);
  return out;
}

}  // namespace chaski::wake
