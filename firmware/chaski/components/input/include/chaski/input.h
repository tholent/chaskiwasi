// Package input turns key hardware into events, and owns the two keys that
// must work even when the UI does not.
//
// Put-away is intercepted HERE, below UI dispatch (design §4.1, client §10):
// the reason it wanted to be a discrete switch was so a hung screen could not
// swallow it, and priority buys that without the extra part. The sync key gets
// the same treatment for the same reason. C-11 asserts a deliberately hung
// screen handler cannot swallow either.
#pragma once

#include <functional>
#include <memory>

namespace chaski::input {

enum class Key {
  kNone, kUp, kDown, kLeft, kRight, kEnter, kBack,
  kPutAway,     // intercepted below UI dispatch — never delivered to a screen
  kSync,        // likewise: syncs from any screen, reveals nothing
  kFrontlight,
  kChar,        // printable; see KeyEvent::codepoint
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
};

std::unique_ptr<Dispatcher> NewDispatcher(const DispatcherDeps& d);

}  // namespace chaski::input
