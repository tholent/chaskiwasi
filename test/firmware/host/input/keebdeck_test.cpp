// The KeebDeck I2C source: debounce, auto-repeat, and the scancode table.
//
// HARDWARE-BLOCKED. No KeebDeck exists in this environment, so what is asserted
// here is the logic above the bus — that a bouncing contact produces one
// keystroke, that a held key repeats where repetition is meant and never where
// it is not, and that an unreadable bus produces silence rather than a guess.
// The bus transaction itself and the scancode values are the convention for
// this class of part, and only the part can confirm them (see keebdeck.h).
//
// Why it matters that this layer fails toward silence: the keys it can emit
// include the one that wipes the screen and the one that opens the radio.
#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "chaski/input.h"
#include "chaski/keebdeck.h"
#include "recording_input.h"

namespace {

using chaski::input::Key;
using chaski::input::KeyEvent;
using chaski_test::FakeKeebDeckBus;

// Rig polls a real source over a fake bus, advancing a fake clock by a fixed
// step per poll — which is what the input task does, minus the hardware.
struct Rig {
  FakeKeebDeckBus bus;
  chaski_test::FakeClock clock;
  std::vector<KeyEvent> events;

  std::unique_ptr<chaski::input::Source> New() {
    chaski::input::KeebDeckDeps d;
    d.bus = &bus;
    d.monotonic_ms = [this] { return clock.Now(); };
    return chaski::input::NewKeebDeckSource(d);
  }

  void PollFor(chaski::input::Source& src, int polls, std::int64_t step_ms) {
    KeyEvent e;
    for (int i = 0; i < polls; ++i) {
      clock.Advance(step_ms);
      while (src.Poll(e)) events.push_back(e);
    }
  }
};

}  // namespace

// A contact that bounces is one keystroke, not four. The script is what the bus
// would actually show: the level flapping for a few milliseconds, each phase
// lasting several samples, before it settles. Phases longer than one sample are
// the point — an implementation that merely required two readings to agree
// would pass a script that alternated every time, and would emit three
// keystrokes for this one.
TEST(Input, DebounceYieldsOneEventForABouncingContact) {
  Rig rig;
  rig.bus.Push('k', 3);
  rig.bus.Push(chaski::input::kScanNone, 2);
  rig.bus.Push('k', 4);
  rig.bus.Push(chaski::input::kScanNone, 3);
  rig.bus.Push('k', 40);
  auto src = rig.New();

  rig.PollFor(*src, 52, /*step_ms=*/1);

  ASSERT_EQ(rig.events.size(), 1u);
  EXPECT_EQ(rig.events[0].key, Key::kChar);
  EXPECT_EQ(rig.events[0].codepoint, static_cast<unsigned int>('k'));
}

// A contact that never settles is not a keystroke. This is the other half of
// the same rule and the half that is easy to lose: an implementation that
// emitted on the first reading would pass the test above.
TEST(Input, AContactThatSettlesForLessThanTheWindowIsNotAKey) {
  Rig rig;
  rig.bus.Push('k', 5);  // 5 ms, well short of kDebounceMs
  auto src = rig.New();

  rig.PollFor(*src, 40, /*step_ms=*/1);

  EXPECT_TRUE(rig.events.empty());
}

// A release emits nothing and re-arms the key. Without the release having to
// settle too, the bounce on the way UP would read as a second press.
TEST(Input, ReleaseEmitsNothingAndRearmsTheKey) {
  Rig rig;
  rig.bus.Push('a', 30);
  rig.bus.Push(chaski::input::kScanNone, 30);
  rig.bus.Push('a', 30);
  auto src = rig.New();

  rig.PollFor(*src, 90, /*step_ms=*/1);

  ASSERT_EQ(rig.events.size(), 2u);
  EXPECT_EQ(rig.events[0].codepoint, static_cast<unsigned int>('a'));
  EXPECT_EQ(rig.events[1].codepoint, static_cast<unsigned int>('a'));
}

// Auto-repeat exists for the child correcting a mistake in the middle of a
// 500-grapheme letter (client §11.2). Held for a second and a half, an ordinary
// key produces its press plus repeats at the interval — not a stream at poll
// rate, and not one lonely character.
TEST(Input, AHeldOrdinaryKeyRepeatsAfterTheDelay) {
  Rig rig;
  rig.bus.Push('e', 200);
  auto src = rig.New();

  rig.PollFor(*src, 150, /*step_ms=*/10);  // 1500 ms held

  // 1500 ms of hold: the press, then repeats from 500 ms every 120 ms.
  const std::size_t expected_repeats =
      static_cast<std::size_t>((1500 - 500) / chaski::input::kRepeatIntervalMs);
  EXPECT_GE(rig.events.size(), expected_repeats);
  EXPECT_LE(rig.events.size(), expected_repeats + 3u);
  for (const auto& e : rig.events) {
    EXPECT_EQ(e.key, Key::kChar);
    EXPECT_EQ(e.codepoint, static_cast<unsigned int>('e'));
  }
}

// Nothing that acts repeats. A leaned-on put-away key must run the wipe once,
// not over and over; a held sync key must not become a radio the child cannot
// switch off; a held Back must not walk out of four screens.
TEST(Input, KeysThatActNeverRepeat) {
  const std::vector<std::uint8_t> scancodes = {
      chaski::input::kScanPutAway, chaski::input::kScanSync,
      chaski::input::kScanEnter, chaski::input::kScanBack,
      chaski::input::kScanFrontlight};
  for (const std::uint8_t scancode : scancodes) {
    Rig rig;
    rig.bus.Push(scancode, 400);
    auto src = rig.New();

    rig.PollFor(*src, 300, /*step_ms=*/10);  // 3 s held

    EXPECT_EQ(rig.events.size(), 1u)
        << "scancode " << static_cast<int>(scancode);
  }
}

// An unreadable bus produces silence. It must not read as a release either: a
// synthesised release fires a fresh press the instant the bus recovers, and on
// the put-away key that is a wipe nobody asked for.
TEST(Input, ABusErrorEmitsNothingAndSynthesisesNoPress) {
  Rig rig;
  rig.bus.Push(chaski::input::kScanPutAway, 30);
  rig.bus.Push(FakeKeebDeckBus::kFail, 10);
  rig.bus.Push(chaski::input::kScanPutAway, 60);
  auto src = rig.New();

  rig.PollFor(*src, 100, /*step_ms=*/1);

  ASSERT_EQ(rig.events.size(), 1u);
  EXPECT_EQ(rig.events[0].key, Key::kPutAway);
}

// A scancode nobody has verified must not become a character in a letter.
TEST(Input, UnmappedScancodesAreIgnored) {
  Rig rig;
  rig.bus.Push(0xF7, 40);
  auto src = rig.New();

  rig.PollFor(*src, 40, /*step_ms=*/1);

  EXPECT_TRUE(rig.events.empty());
}

// The table decodes to CODEPOINTS, not bytes. `ñ` is an ordinary letter in the
// languages these letters are written in, and a byte-for-codepoint decoder
// would have made it unreachable without anyone noticing until a child could
// not write their aunt's name.
TEST(Input, DecodeMapsCapsToCodepointsNotBytes) {
  using chaski::input::DecodeKeebDeck;

  EXPECT_EQ(DecodeKeebDeck('a').key, Key::kChar);
  EXPECT_EQ(DecodeKeebDeck('a').codepoint, static_cast<unsigned int>('a'));
  EXPECT_EQ(DecodeKeebDeck(chaski::input::kScanEnye).key, Key::kChar);
  EXPECT_EQ(DecodeKeebDeck(chaski::input::kScanEnye).codepoint, 0x00F1u);
  EXPECT_EQ(DecodeKeebDeck(chaski::input::kScanEnyeUpper).codepoint, 0x00D1u);
  EXPECT_EQ(DecodeKeebDeck(chaski::input::kScanPutAway).key, Key::kPutAway);
  EXPECT_EQ(DecodeKeebDeck(chaski::input::kScanSync).key, Key::kSync);
  EXPECT_EQ(DecodeKeebDeck(chaski::input::kScanNone).key, Key::kNone);
  EXPECT_EQ(DecodeKeebDeck(0x07).key, Key::kNone);  // no cap reports this
}

// The two keys never collide with a printable one: a put-away that also typed a
// character, or a letter key that also wiped the screen, is the kind of table
// error the part's arrival would otherwise surface first.
TEST(Input, NoPrintableCapDecodesToPutAwayOrSync) {
  for (int scancode = 0x20; scancode <= 0x7E; ++scancode) {
    const KeyEvent e =
        chaski::input::DecodeKeebDeck(static_cast<std::uint8_t>(scancode));
    EXPECT_EQ(e.key, Key::kChar) << scancode;
  }
}

// End to end over the seam: a put-away press on the real source, through the
// real dispatcher, still never reaches a screen (C-11).
TEST(C11, PutAwayFromTheKeebDeckIsInterceptedBelowUiDispatch) {
  Rig rig;
  rig.bus.Push('a', 30);
  rig.bus.Push(chaski::input::kScanNone, 30);
  rig.bus.Push(chaski::input::kScanPutAway, 30);
  auto src = rig.New();

  chaski_test::Trace trace;
  chaski::input::DispatcherDeps d;
  d.source = src.get();
  d.on_put_away = [&trace] { trace.Add("putaway"); };
  auto dispatcher = chaski::input::NewDispatcher(d);

  KeyEvent e;
  for (int i = 0; i < 100; ++i) {
    rig.clock.Advance(1);
    if (dispatcher->Pump(e)) trace.Add(chaski_test::Label(e));
  }

  EXPECT_EQ(trace.Count("putaway"), 1);
  EXPECT_EQ(trace.IndexOf("ui:PUTAWAY"), -1);
  EXPECT_TRUE(trace.Contains("ui:char:97"));
}
