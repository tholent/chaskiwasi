// Implementation of the KeebDeck I2C source — see include/chaski/keebdeck.h.
//
// The state machine is a debouncer with a repeat timer bolted to its accepted
// state, and it is written to fail toward silence: every uncertain reading
// produces no event. The keys this source can emit include the one that wipes
// the screen, so a spurious press is not a cosmetic bug.
//
// Nothing here logs. This is the layer that sees the letter being typed (D-7).
#include "chaski/keebdeck.h"

#include <utility>

namespace chaski::input {
namespace {

// Repeat applies only where repetition is what the user means. Deliberately
// excluded: kEnter (repeating a send or a confirmation), kBack (walking out of
// several screens on one held key), kFrontlight, and above all kPutAway and
// kSync — a leaned-on put-away key must not run the wipe over and over, and a
// held sync key must not become a radio the child cannot switch off.
bool Repeatable(Key k) {
  switch (k) {
    case Key::kChar:
    case Key::kUp:
    case Key::kDown:
    case Key::kLeft:
    case Key::kRight:
      return true;
    default:
      return false;
  }
}

class KeebDeckSource final : public Source {
 public:
  explicit KeebDeckSource(KeebDeckDeps d) : deps_(std::move(d)) {}

  // Poll performs at most one bus transaction and emits at most one event, so a
  // caller draining in a loop always terminates.
  bool Poll(KeyEvent& out) override {
    if (deps_.bus == nullptr) return false;
    const std::int64_t now = Now();

    std::uint8_t raw = kScanNone;
    if (!deps_.bus->ReadPressed(raw)) {
      // The bus is unreadable, so the held key is unknown. Stop repeating —
      // repeat is an assertion that a key is still down, and that is exactly
      // what has stopped being knowable — but keep the accepted state, so
      // recovery reports a press only after a real release is observed.
      repeat_armed_ = false;
      candidate_ = accepted_;
      return false;
    }

    if (raw != candidate_) {
      candidate_ = raw;
      candidate_since_ms_ = now;
      return false;
    }

    if (raw == accepted_) return Repeat(now, out);

    if (now - candidate_since_ms_ < kDebounceMs) return false;

    accepted_ = raw;
    repeat_armed_ = false;
    if (raw == kScanNone) return false;  // a settled release emits nothing

    const KeyEvent e = Decode(raw);
    if (e.key == Key::kNone) return false;  // unmapped cap, silently ignored

    if (Repeatable(e.key)) {
      repeat_armed_ = true;
      repeat_event_ = e;
      next_repeat_ms_ = now + kRepeatDelayMs;
    }
    out = e;
    return true;
  }

 private:
  bool Repeat(std::int64_t now, KeyEvent& out) {
    if (!repeat_armed_ || now < next_repeat_ms_) return false;
    // Scheduled from now rather than advanced by one interval: a caller that
    // stopped pumping for two seconds — a full refresh, say — should resume at
    // the repeat rate, not deliver the sixteen keystrokes it "owes".
    next_repeat_ms_ = now + kRepeatIntervalMs;
    out = repeat_event_;
    return true;
  }

  KeyEvent Decode(std::uint8_t scancode) const {
    return deps_.decode ? deps_.decode(scancode) : DecodeKeebDeck(scancode);
  }

  std::int64_t Now() const {
    return deps_.monotonic_ms ? deps_.monotonic_ms() : 0;
  }

  KeebDeckDeps deps_;
  std::uint8_t accepted_ = kScanNone;
  std::uint8_t candidate_ = kScanNone;
  std::int64_t candidate_since_ms_ = 0;
  bool repeat_armed_ = false;
  KeyEvent repeat_event_;
  std::int64_t next_repeat_ms_ = 0;
};

}  // namespace

KeyEvent DecodeKeebDeck(std::uint8_t scancode) {
  KeyEvent e;
  switch (scancode) {
    case kScanPutAway: e.key = Key::kPutAway; return e;
    case kScanSync: e.key = Key::kSync; return e;
    case kScanFrontlight: e.key = Key::kFrontlight; return e;
    case kScanEnter: e.key = Key::kEnter; return e;
    case kScanBack: e.key = Key::kBack; return e;
    case kScanUp: e.key = Key::kUp; return e;
    case kScanDown: e.key = Key::kDown; return e;
    case kScanLeft: e.key = Key::kLeft; return e;
    case kScanRight: e.key = Key::kRight; return e;
    case kScanEnye:
      e.key = Key::kChar;
      e.codepoint = 0x00F1;  // ñ
      return e;
    case kScanEnyeUpper:
      e.key = Key::kChar;
      e.codepoint = 0x00D1;  // Ñ
      return e;
    default:
      break;
  }
  // Printable ASCII is its own codepoint. Everything else — including the
  // control range below 0x20, which no cap on this keyboard reports — is
  // unmapped rather than guessed: a scancode nobody has verified must not
  // become a character in a child's letter.
  if (scancode >= 0x20 && scancode <= 0x7E) {
    e.key = Key::kChar;
    e.codepoint = scancode;
  }
  return e;
}

std::unique_ptr<Source> NewKeebDeckSource(const KeebDeckDeps& d) {
  return std::unique_ptr<Source>(new KeebDeckSource(d));
}

}  // namespace chaski::input
