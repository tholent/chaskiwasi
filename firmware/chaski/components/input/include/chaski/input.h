// Package input turns key hardware into events, and owns the two keys that
// must work even when the UI does not.
//
// Put-away is intercepted HERE, below UI dispatch (design §4.1, client §10):
// the reason it wanted to be a discrete switch was so a hung screen could not
// swallow it, and priority buys that without the extra part. The sync key gets
// the same treatment for the same reason. C-11 asserts a deliberately hung
// screen handler cannot swallow either.
#pragma once

#include <cstdint>
#include <functional>
#include <memory>

namespace chaski::input {

enum class Key {
  kNone, kUp, kDown, kLeft, kRight, kEnter, kBack,
  kPutAway,     // intercepted below UI dispatch — never delivered to a screen
  kSync,        // likewise: syncs from any screen, reveals nothing
  kFrontlight,
  kChar,        // printable; see KeyEvent::codepoint

  // kErase is backspace in a text field, and is deliberately NOT kBack.
  // Conflating them makes one key mean "delete the letter I am writing" on one
  // screen and "leave this screen" on another, which is the kind of ambiguity a
  // child pays for mid-letter. kBack navigates; kErase deletes. It repeats,
  // because correcting a mistake thirty characters back is otherwise thirty
  // presses, and that is where a kid abandons the letter (§11.2).
  //
  // Appended rather than inserted: the enumerators above are compiled against
  // concurrently, and renumbering them would be a silent change to every
  // scancode table that already exists.
  kErase,
};

struct KeyEvent {
  Key key = Key::kNone;
  unsigned int codepoint = 0;  // valid when key == kChar
};

// Source is the hardware seam: KeebDeck over I2C for the prototype, a scanned
// GPIO matrix with ext1 wake for production (client §2, §10).
class Source {
 public:
  virtual ~Source() = default;
  virtual bool Poll(KeyEvent& out) = 0;
};

// Dispatcher sits between Source and the UI. Interception is its whole reason
// for existing: put-away and sync are handled before any screen sees them.
//
// Put-away JUMPS THE QUEUE. One Pump drains everything the source has and
// services put-away and sync as it meets them, returning at most one UI event;
// the rest wait. So a press arriving behind a burst of typing is serviced on
// the very next Pump, not after that burst has been dispatched through a screen
// that spends seconds per e-ink refresh. Delivering in strict arrival order
// would make put-away's latency a function of how slow the UI is, which is the
// hung-screen failure in a slower costume (design §4.1, C-11).
class Dispatcher {
 public:
  virtual ~Dispatcher() = default;

  // Pump drains the source. Returns true if a UI-visible event was written to
  // `out`; put-away and sync are consumed here and invoke their handlers.
  virtual bool Pump(KeyEvent& out) = 0;

  // Mask stops UI delivery during a wipe (client §9.1 step 1). Put-away and
  // sync handlers stay armed.
  virtual void Mask(bool on) = 0;
};

struct DispatcherDeps {
  Source* source = nullptr;
  std::function<void()> on_put_away;  // immediate wipe, no confirmation
  std::function<void()> on_sync;

  // feed_watchdog is called once per Pump. It is what makes the Watchdog below
  // mean anything: the feed comes from the code path whose silence is the
  // symptom, so an input layer that stops pumping stops feeding (client §10).
  // main/ wires it to Watchdog::Feed; the esp_task_wdt registration is main/'s,
  // because no esp_* header may reach a component (implementation plan rule 3).
  std::function<void()> feed_watchdog;
};

std::unique_ptr<Dispatcher> NewDispatcher(const DispatcherDeps& d);

// Watchdog covers the case interception cannot: the input layer ITSELF wedges,
// so nothing polls the source and the put-away key is never even read. Its
// expiry runs the wipe before the reset (client §10, C-11).
//
// It is deliberately NOT part of Dispatcher and holds none of its state. The
// scenario is "the thing that owns that state is stuck"; a recovery path that
// has to reach through the stuck object to work is not a recovery path.
class Watchdog {
 public:
  virtual ~Watchdog() = default;

  // Feed records that the input layer is alive. Called from the input task.
  virtual void Feed() = 0;

  // Overdue reports that no feed arrived within the timeout. Exposed so a
  // supervisor task can drive expiry with the same logic the hardware watchdog
  // would, and so the decision is assertable on the host with a fake clock.
  virtual bool Overdue() const = 0;

  // Expire runs the wipe. Wired to esp_task_wdt's timeout callback by main/,
  // or called by a supervisor that saw Overdue. Safe to call more than once.
  virtual void Expire() = 0;
};

// The timeout is generous on purpose: it must exceed the longest legitimate
// gap between pumps, and a full-refresh waveform alone is 3-4 s (client §8.1).
// Tripping this is a bug in something, and the response — wipe, then reset —
// costs the child their unsaved seconds; drafts autosave (client §11.3), so it
// costs seconds and not a letter. Final value belongs in docs/bringup.md once
// the real screen timings are measured.
inline constexpr std::int64_t kWatchdogTimeoutMs = 10000;

struct WatchdogDeps {
  // on_expiry is the wipe. main/ sets it to the wipe controller's GracefulWipe
  // bound to the resting cover — the same handler put-away gets, because the
  // requirement is the same one.
  std::function<void()> on_expiry;

  // monotonic_ms is 64-bit: 32 bits of milliseconds wrap after 24.8 days and
  // this device is meant to sit in a bag for weeks.
  std::function<std::int64_t()> monotonic_ms;

  std::int64_t timeout_ms = kWatchdogTimeoutMs;
};

std::unique_ptr<Watchdog> NewWatchdog(const WatchdogDeps& d);

}  // namespace chaski::input
