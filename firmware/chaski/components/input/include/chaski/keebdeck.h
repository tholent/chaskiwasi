// KeebDeck Basic over I2C — the prototype keyboard (client §2, §10).
//
// Everything above the Source seam is identical for this and for the production
// matrix scan, which is the whole point of §10's "one input seam, two
// implementations". What is specific to this part is confined to two things:
// the single I2C transaction behind KeebDeckBus, and the scancode table behind
// DecodeKeebDeck. Debounce and repeat sit between them and belong to neither —
// a silicone dome bounces the same way whoever reads it.
//
// HARDWARE-BLOCKED. No KeebDeck exists in this environment, so the transaction
// shape and the scancode values below are the convention for this class of part
// (M5 CardKB and friends) rather than a measured fact. They are isolated here
// so that being wrong about them costs a table and a method, not the layer.
#pragma once

#include <cstdint>
#include <functional>
#include <memory>

#include "chaski/input.h"

namespace chaski::input {

// kDebounceMs is how long a reading must hold still before it counts. Silicone
// domes over a carbon pill settle in single-digit milliseconds; 12 ms is that
// with margin, and stays far below the ~50 ms where a key starts to feel
// unresponsive. Measured value goes in docs/bringup.md when the part arrives.
inline constexpr std::int64_t kDebounceMs = 12;

// Auto-repeat exists for one user: a child correcting a mistake in the middle
// of a 500-grapheme letter (client §11.2). Without it, backing up a word means
// pressing a key thirty times, and that is the moment a kid gives up on the
// letter rather than on the key. The rate is deliberately unhurried — this is a
// panel that batches refreshes (client §8.3), not a game controller.
inline constexpr std::int64_t kRepeatDelayMs = 500;
inline constexpr std::int64_t kRepeatIntervalMs = 120;

// Scancodes. Provisional — see the HARDWARE-BLOCKED note above.
//
// Printable keys report their ASCII byte; the arrows use the 0xB4..0xB7 block
// this class of keypad has used since the CardKB. Two ranges are ours:
//
//   0x80..0x9F  keys whose codepoint is not ASCII. The families this device is
//               for write in Spanish and Quechua, so `ñ` is an ordinary letter
//               of an ordinary word, and a decoder that handed the layer a byte
//               instead of a codepoint would have made it unreachable without
//               anyone noticing until a child could not write their aunt's
//               name. Decode returns codepoints for exactly this reason.
//   0xC0..0xC2  the three top-row keys. Put-away must be the corner-most one
//               with a distinct cap, findable by touch, in a row of identical
//               keys (design §4.1) — a hardware question the firmware is
//               indifferent to beyond the scancode arriving here.
inline constexpr std::uint8_t kScanNone = 0x00;
inline constexpr std::uint8_t kScanBack = 0x1B;
inline constexpr std::uint8_t kScanEnter = 0x0D;
inline constexpr std::uint8_t kScanLeft = 0xB4;
inline constexpr std::uint8_t kScanUp = 0xB5;
inline constexpr std::uint8_t kScanDown = 0xB6;
inline constexpr std::uint8_t kScanRight = 0xB7;
inline constexpr std::uint8_t kScanEnye = 0x80;       // ñ  U+00F1
inline constexpr std::uint8_t kScanEnyeUpper = 0x81;  // Ñ  U+00D1
inline constexpr std::uint8_t kScanPutAway = 0xC0;
inline constexpr std::uint8_t kScanSync = 0xC1;
inline constexpr std::uint8_t kScanFrontlight = 0xC2;

// KeebDeckBus is the one I2C transaction this source performs, isolated so the
// scan and decode logic is testable with a fake and so no esp_* header reaches
// a component (implementation plan rule 3).
//
// The read is level-reporting: it answers "which key is held right now", zero
// for none, which is what this class of keypad exposes. That means no n-key
// rollover — a second key pressed while the first is held is not reported until
// the first is released. Acceptable for a device with no modifier chords;
// production's matrix scan (client §2) is where rollover would be solved.
class KeebDeckBus {
 public:
  virtual ~KeebDeckBus() = default;

  // ReadPressed returns false on a bus error, leaving `scancode` untouched. A
  // failed read is NOT a release: synthesising one would fire a fresh press the
  // instant the bus recovered, which on the put-away key means a wipe nobody
  // asked for.
  virtual bool ReadPressed(std::uint8_t& scancode) = 0;
};

// DecodeKeebDeck maps a scancode to an event, returning Key::kNone for anything
// unmapped. Exposed rather than hidden so the table is assertable directly —
// when the real part turns up, this function is the diff.
KeyEvent DecodeKeebDeck(std::uint8_t scancode);

struct KeebDeckDeps {
  KeebDeckBus* bus = nullptr;

  // monotonic_ms drives debounce and repeat. 64-bit for the same reason it is
  // everywhere else in this tree: 32 bits of milliseconds wrap after 24.8 days.
  std::function<std::int64_t()> monotonic_ms;

  // decode overrides the table, for tests and for a board profile whose legends
  // differ. Null means DecodeKeebDeck.
  std::function<KeyEvent(std::uint8_t)> decode;
};

std::unique_ptr<Source> NewKeebDeckSource(const KeebDeckDeps& d);

}  // namespace chaski::input
