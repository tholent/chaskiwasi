// Implementation of components/input — see include/chaski/input.h for the
// contract and the spec clauses it must satisfy.
//
// One rule generates almost everything here: NOTHING SUPPRESSES PUT-AWAY OR
// SYNC. Not the mask, not a full queue, not a screen that never consumes what
// it was handed, not a put-away serviced a moment ago. The single exception is
// a re-entrant call into the same handler, which would start a second waveform
// on a panel mid-transition — worse than not wiping at all (design §4.1).
//
// The child-facing reason, from design §4.1: put-away is about agency. One
// deliberate gesture hides everything NOW, from any screen, with no
// confirmation. Every conditional that could stand between the key and the wipe
// is a way for that promise to quietly stop being true.
//
// Nothing here logs. This component sees every keystroke of a letter, so a
// component with no logging statement cannot acquire a D-7 defect by accident.
#include "chaski/input.h"

#include <array>
#include <atomic>
#include <cstddef>
#include <utility>

namespace chaski::input {
namespace {

// kMaxDrainPerPump bounds one Pump so a source stuck asserting a key cannot
// turn it into a loop that never returns — which would starve the very
// watchdog feed that is supposed to catch a wedged input layer.
constexpr int kMaxDrainPerPump = 64;

// kPendingCapacity is the UI backlog held while the screen catches up. A child
// types faster than a partial refresh, and a few keystrokes of slack absorb
// that. The size is not load-bearing: overflow drops keystrokes, never
// put-away, because the drain continues past a full queue.
constexpr std::size_t kPendingCapacity = 32;

class DispatcherImpl final : public Dispatcher {
 public:
  explicit DispatcherImpl(DispatcherDeps d) : deps_(std::move(d)) {}

  bool Pump(KeyEvent& out) override {
    // Fed first, unconditionally: the feed answers "is the input layer still
    // running", and gating it on having found work would make a quiet keyboard
    // indistinguishable from a wedged one (client §10).
    if (deps_.feed_watchdog) deps_.feed_watchdog();
    Drain();
    return Pop(out);
  }

  // §9.1 step 1. The backlog is discarded rather than held: it belongs to the
  // session being wiped, and replaying it into whatever screen comes back is
  // the pre-wipe session reaching across the wipe.
  void Mask(bool on) override {
    masked_ = on;
    if (on) Clear();
  }

 private:
  void Drain() {
    if (deps_.source == nullptr) return;

    // put_away_seen suppresses UI delivery for the REST of this drain, not
    // handler dispatch. Keys sitting behind put-away in the queue were struck
    // before the screen went away; they are not instructions to the screen that
    // comes after it.
    bool put_away_seen = false;

    KeyEvent e;
    for (int i = 0; i < kMaxDrainPerPump && deps_.source->Poll(e); ++i) {
      switch (e.key) {
        case Key::kPutAway:
          put_away_seen = true;
          Clear();
          FirePutAway();
          break;
        case Key::kSync:
          // Sync does NOT clear the backlog. A child who taps the mail flag
          // mid-letter is sending what is already written, not abandoning what
          // they are still typing (client §10, §11.2).
          FireSync();
          break;
        default:
          if (masked_ || put_away_seen) break;
          Push(e);
          break;
      }
    }
  }

  // A second put-away while a wipe is running fires the wipe again, and that is
  // the intended behaviour: the child pressed the one key that means "hide it
  // now", and answering "already did" is precisely the swallow this layer
  // exists to prevent. Re-running costs a redundant flush — idempotent, and
  // cheap next to the alternative. What is NOT allowed is re-entering the
  // handler from inside itself, which would abort a waveform in flight and
  // freeze the panel mid-wipe (design §4.1, client §9.1 step 3). Hence a guard
  // against nesting only, never against repetition.
  void FirePutAway() {
    if (put_away_active_.exchange(true)) return;
    if (deps_.on_put_away) deps_.on_put_away();
    put_away_active_.store(false);
  }

  // Sync gets its own guard rather than sharing put-away's: a sync can take
  // seconds over LTE-M, and a shared flag would make put-away during a sync —
  // exactly when a child might want it — the one press that does nothing.
  void FireSync() {
    if (sync_active_.exchange(true)) return;
    if (deps_.on_sync) deps_.on_sync();
    sync_active_.store(false);
  }

  // Push drops the NEWEST event when full. Dropping the oldest would delete
  // characters the child already watched appear on the screen; dropping the
  // newest loses a keystroke that never landed, which is what a keyboard out of
  // buffer has always done.
  void Push(const KeyEvent& e) {
    if (count_ == kPendingCapacity) return;
    pending_[(head_ + count_) % kPendingCapacity] = e;
    ++count_;
  }

  bool Pop(KeyEvent& out) {
    if (count_ == 0) return false;
    out = pending_[head_];
    head_ = (head_ + 1) % kPendingCapacity;
    --count_;
    return true;
  }

  void Clear() {
    head_ = 0;
    count_ = 0;
  }

  DispatcherDeps deps_;
  std::array<KeyEvent, kPendingCapacity> pending_{};
  std::size_t head_ = 0;
  std::size_t count_ = 0;
  bool masked_ = false;
  std::atomic<bool> put_away_active_{false};
  std::atomic<bool> sync_active_{false};
};

class WatchdogImpl final : public Watchdog {
 public:
  explicit WatchdogImpl(WatchdogDeps d) : deps_(std::move(d)) {
    last_feed_ms_.store(Now());
  }

  void Feed() override { last_feed_ms_.store(Now()); }

  bool Overdue() const override {
    if (!deps_.monotonic_ms) return false;
    return Now() - last_feed_ms_.load() >= deps_.timeout_ms;
  }

  // Expire runs on a path where the input task is presumed stuck, so it touches
  // nothing that task owns. The nesting guard is FirePutAway's, for the same
  // reason; a second expiry after the handler returned still wipes, because a
  // device that got here twice has even less claim to be healthy than one that
  // got here once.
  void Expire() override {
    if (active_.exchange(true)) return;
    if (deps_.on_expiry) deps_.on_expiry();
    // Refreshed so a supervisor polling Overdue does not re-trip on the same
    // stale timestamp and wipe in a loop.
    last_feed_ms_.store(Now());
    active_.store(false);
  }

 private:
  std::int64_t Now() const {
    return deps_.monotonic_ms ? deps_.monotonic_ms() : 0;
  }

  WatchdogDeps deps_;
  std::atomic<std::int64_t> last_feed_ms_{0};
  std::atomic<bool> active_{false};
};

}  // namespace

std::unique_ptr<Dispatcher> NewDispatcher(const DispatcherDeps& d) {
  return std::unique_ptr<Dispatcher>(new DispatcherImpl(d));
}

std::unique_ptr<Watchdog> NewWatchdog(const WatchdogDeps& d) {
  return std::unique_ptr<Watchdog>(new WatchdogImpl(d));
}

}  // namespace chaski::input
