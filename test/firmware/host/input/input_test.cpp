// C-11: put-away and sync are handled below UI dispatch, and no screen can
// swallow them.
//
// This is a privacy test wearing an input test's clothes. The reason put-away
// wanted to be a discrete switch was that a hung screen must not be able to eat
// it (design §4.1); priority buys the same guarantee without the extra part,
// and these cases are what "priority" has to mean in code:
//
//   1. the handlers run without the UI being consulted, in any state,
//   2. put-away jumps whatever the UI has not got round to yet,
//   3. masking for a wipe does not disarm either key,
//   4. and if the input layer itself stops running, the watchdog wipes.
//
// Assertions are against one interleaved trace rather than per-object counters:
// "the wipe ran and 'a' was delivered" is true of a correct dispatcher AND of
// one that hands put-away to a screen first. Only the order separates them.
#include <functional>
#include <memory>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "chaski/input.h"
#include "recording_input.h"

namespace {

using chaski::input::DispatcherDeps;
using chaski::input::Key;
using chaski::input::KeyEvent;
using chaski_test::HungConsumer;
using chaski_test::ScriptedSource;
using chaski_test::Trace;

struct Rig {
  Trace trace;
  ScriptedSource source;
  int feeds = 0;

  // put_away_hook runs inside the put-away handler, for the re-entrancy case.
  std::function<void()> put_away_hook;

  std::unique_ptr<chaski::input::Dispatcher> dispatcher;

  Rig() {
    DispatcherDeps d;
    d.source = &source;
    d.on_put_away = [this] {
      trace.Add("putaway");
      if (put_away_hook) put_away_hook();
    };
    d.on_sync = [this] { trace.Add("sync"); };
    d.feed_watchdog = [this] { ++feeds; };
    dispatcher = chaski::input::NewDispatcher(d);
  }

  // Drain pumps until the dispatcher has nothing left, recording every event it
  // hands out. The bound is a safety net: a dispatcher that never ran dry would
  // otherwise hang the suite instead of failing it.
  int Drain(HungConsumer* consumer = nullptr, int max_pumps = 200) {
    int delivered = 0;
    KeyEvent e;
    for (int i = 0; i < max_pumps; ++i) {
      if (!dispatcher->Pump(e)) return delivered;
      trace.Add(chaski_test::Label(e));
      ++delivered;
      if (consumer != nullptr) consumer->Handle(e);
    }
    ADD_FAILURE() << "Pump never ran dry";
    return delivered;
  }
};

// Before asserts ordering, not adjacency: the requirement is a sequence, and
// demanding adjacency would fail on any harmless extra step.
void Before(const Trace& t, const std::string& a, const std::string& b) {
  const int ia = t.IndexOf(a);
  const int ib = t.IndexOf(b);
  ASSERT_GE(ia, 0) << "missing step: " << a;
  ASSERT_GE(ib, 0) << "missing step: " << b;
  EXPECT_LT(ia, ib) << a << " must happen before " << b;
}

}  // namespace

// Put-away and sync are their own keys precisely so the dispatcher can consume
// them before any screen sees them (design §4.1, client §10).
TEST(Input, PutAwayAndSyncAreDistinctKeys) {
  EXPECT_NE(Key::kPutAway, Key::kSync);
  EXPECT_NE(Key::kPutAway, Key::kBack);
  EXPECT_NE(Key::kPutAway, Key::kNone);
}

// C-11 proper. The screen is wedged: it consumes nothing, decides nothing, and
// is never coming back for more. The wipe runs anyway, on the first pump, with
// nothing handed to that screen first.
TEST(C11, PutAwayFiresWithAHungUiConsumer) {
  Rig rig;
  HungConsumer screen;
  rig.source.PushChar('h');
  rig.source.PushChar('i');
  rig.source.Push(Key::kPutAway);

  KeyEvent e;
  const bool ui_event = rig.dispatcher->Pump(e);
  if (ui_event) screen.Handle(e);

  EXPECT_EQ(rig.trace.Count("putaway"), 1);
  EXPECT_FALSE(ui_event) << "keys struck before the wipe must not survive it";
  EXPECT_EQ(screen.handled, 0);
}

// The same guarantee stated as latency, which is the form that actually breaks:
// deliver in strict arrival order and put-away waits for the UI to work through
// everything ahead of it — at seconds per e-ink refresh, that is a screen
// swallowing the key by being slow instead of by being stuck.
TEST(C11, PutAwayJumpsAheadOfEverythingTheUiHasNotConsumed) {
  Rig rig;
  for (int i = 0; i < 20; ++i) rig.source.PushChar('a' + (i % 26));
  rig.source.Push(Key::kPutAway);

  KeyEvent e;
  EXPECT_FALSE(rig.dispatcher->Pump(e));
  EXPECT_EQ(rig.trace.Count("putaway"), 1);
  EXPECT_EQ(rig.trace.IndexOf("ui:char:97"), -1);
}

// Sync sits below UI dispatch for the same reason and on the same terms: it
// syncs from any screen and reveals nothing (client §10).
TEST(C11, SyncFiresFromAnyStateAndIsNeverDelivered) {
  for (const bool masked : {false, true}) {
    Rig rig;
    rig.dispatcher->Mask(masked);
    rig.source.PushChar('x');
    rig.source.Push(Key::kSync);
    rig.source.PushChar('y');
    rig.Drain();

    EXPECT_EQ(rig.trace.Count("sync"), 1) << "masked=" << masked;
    EXPECT_EQ(rig.trace.IndexOf("ui:SYNC"), -1) << "masked=" << masked;
  }
}

// No screen sees either key, from any position in the queue, masked or not.
TEST(C11, PutAwayAndSyncNeverReachAScreen) {
  const std::vector<std::vector<Key>> queues = {
      {Key::kPutAway},
      {Key::kSync},
      {Key::kPutAway, Key::kSync},
      {Key::kSync, Key::kPutAway},
      {Key::kEnter, Key::kSync, Key::kUp, Key::kPutAway, Key::kDown},
  };
  for (const bool masked : {false, true}) {
    for (const auto& queue : queues) {
      Rig rig;
      HungConsumer screen;
      rig.dispatcher->Mask(masked);
      for (const Key k : queue) rig.source.Push(k);
      rig.Drain(&screen);

      EXPECT_EQ(rig.trace.IndexOf("ui:PUTAWAY"), -1);
      EXPECT_EQ(rig.trace.IndexOf("ui:SYNC"), -1);
      EXPECT_NE(screen.last.key, Key::kPutAway);
      EXPECT_NE(screen.last.key, Key::kSync);
    }
  }
}

// Client §9.1 step 1: the mask stops UI delivery for the duration of a wipe. It
// is not a general input off-switch — the two keys that must work when the UI
// does not are exactly the two that stay armed.
TEST(C11, MaskStopsUiEventsButLeavesPutAwayAndSyncArmed) {
  Rig rig;
  rig.dispatcher->Mask(true);
  rig.source.PushChar('a');
  rig.source.Push(Key::kUp);
  rig.source.Push(Key::kSync);
  rig.source.Push(Key::kPutAway);
  const int delivered = rig.Drain();

  EXPECT_EQ(delivered, 0);
  EXPECT_EQ(rig.trace.Count("sync"), 1);
  EXPECT_EQ(rig.trace.Count("putaway"), 1);
}

TEST(C11, UnmaskingResumesUiDelivery) {
  Rig rig;
  rig.dispatcher->Mask(true);
  rig.source.PushChar('a');
  EXPECT_EQ(rig.Drain(), 0);

  rig.dispatcher->Mask(false);
  rig.source.PushChar('b');
  EXPECT_EQ(rig.Drain(), 1);
  EXPECT_TRUE(rig.trace.Contains("ui:char:98"));
}

// Keystrokes buffered before a wipe do not cross it. Masking is step 1 of the
// wipe, so anything still queued belongs to the session being erased, and
// handing it to whatever screen comes back is that session reaching across.
TEST(C11, MaskingDiscardsTheBacklogInsteadOfHoldingIt) {
  Rig rig;
  rig.source.PushChar('s');
  rig.source.PushChar('e');
  rig.source.PushChar('c');
  KeyEvent e;
  ASSERT_TRUE(rig.dispatcher->Pump(e));  // 's' out, 'e' and 'c' still pending

  rig.dispatcher->Mask(true);
  rig.dispatcher->Mask(false);
  EXPECT_EQ(rig.Drain(), 0);
}

// Sync must NOT discard the backlog: a child who taps the mail flag mid-letter
// is sending what is written, not abandoning what they are still typing
// (client §10, §11.2).
TEST(C11, SyncLeavesTypingIntact) {
  Rig rig;
  rig.source.PushChar('h');
  rig.source.Push(Key::kSync);
  rig.source.PushChar('o');
  rig.Drain();

  EXPECT_EQ(rig.trace.Count("sync"), 1);
  EXPECT_TRUE(rig.trace.Contains("ui:char:104"));
  EXPECT_TRUE(rig.trace.Contains("ui:char:111"));
}

// A second press fires the wipe again. "Already did that" is the swallow this
// layer exists to prevent, and a redundant flush is idempotent and cheap.
TEST(C11, ASecondPutAwayFiresTheWipeAgain) {
  Rig rig;
  rig.source.Push(Key::kPutAway);
  rig.Drain();
  rig.dispatcher->Mask(true);  // the wipe masked input, as §9.1 step 1 requires
  rig.source.Push(Key::kPutAway);
  rig.Drain();

  EXPECT_EQ(rig.trace.Count("putaway"), 2);
}

// The one thing that may suppress a put-away: re-entering the handler from
// inside itself. That would start a second waveform on a panel mid-transition,
// and freezing the panel mid-wipe is worse than not wiping (design §4.1).
TEST(C11, PutAwayHandlerIsNotReentered) {
  Rig rig;
  rig.put_away_hook = [&rig] {
    KeyEvent inner;
    rig.dispatcher->Pump(inner);  // a wipe path that pumps while it runs
  };
  rig.source.Push(Key::kPutAway);
  rig.source.Push(Key::kPutAway);
  rig.Drain();

  EXPECT_EQ(rig.trace.Count("putaway"), 1);
}

// Put-away during a long sync must work — that is exactly when a child is most
// likely to want it — so the two handlers cannot share one guard.
TEST(C11, PutAwayWorksWhileASyncIsRunning) {
  Trace trace;
  ScriptedSource source;
  chaski::input::Dispatcher* dispatcher = nullptr;

  DispatcherDeps d;
  d.source = &source;
  d.on_put_away = [&trace] { trace.Add("putaway"); };
  d.on_sync = [&trace, &dispatcher] {
    trace.Add("sync_begin");
    KeyEvent inner;
    dispatcher->Pump(inner);  // the sync is still running; the key still works
    trace.Add("sync_end");
  };
  auto owned = chaski::input::NewDispatcher(d);
  dispatcher = owned.get();

  source.Push(Key::kSync);
  source.Push(Key::kPutAway);
  KeyEvent e;
  owned->Pump(e);

  Before(trace, "sync_begin", "putaway");
  Before(trace, "putaway", "sync_end");
}

// Ordinary keys round-trip in order. The codepoints are chosen to break any
// implementation that quietly assumes a byte: ñ is an ordinary letter in the
// languages these letters are written in, and an emoji is four bytes of UTF-8.
TEST(C11, OrdinaryKeysRoundTripIncludingNonAsciiCodepoints) {
  Rig rig;
  HungConsumer screen;
  rig.source.PushChar('a');
  rig.source.PushChar(0x00F1);   // ñ
  rig.source.PushChar(0x1F600);  // grinning face
  rig.source.Push(Key::kEnter);
  rig.source.Push(Key::kBack);
  rig.Drain(&screen);

  const std::vector<std::string> expected = {
      "ui:char:97", "ui:char:241", "ui:char:128512", "ui:enter", "ui:back"};
  EXPECT_EQ(rig.trace.steps, expected);
  EXPECT_EQ(screen.handled, 5);
}

// A key held down by a book, or a chattering bus, must not turn Pump into a
// loop that never returns — that would starve the watchdog feed meant to catch
// a wedged input layer. This test terminating at all is the assertion.
TEST(C11, PumpReturnsOnASourceThatNeverGoesQuiet) {
  chaski_test::StuckSource stuck(Key::kChar);
  DispatcherDeps d;
  d.source = &stuck;
  auto dispatcher = chaski::input::NewDispatcher(d);

  KeyEvent e;
  EXPECT_TRUE(dispatcher->Pump(e));
  EXPECT_GT(stuck.polls, 0);
}

// A stuck put-away key must not spin the wipe forever inside one Pump either.
TEST(C11, PumpReturnsOnASourceStuckOnPutAway) {
  chaski_test::StuckSource stuck(Key::kPutAway);
  int wipes = 0;
  DispatcherDeps d;
  d.source = &stuck;
  d.on_put_away = [&wipes] { ++wipes; };
  auto dispatcher = chaski::input::NewDispatcher(d);

  KeyEvent e;
  EXPECT_FALSE(dispatcher->Pump(e));
  EXPECT_GT(wipes, 0);
}

// The feed comes from Pump because Pump's silence is the symptom. A quiet
// keyboard and a wedged input layer must not look the same to the watchdog.
TEST(C11, EveryPumpFeedsTheWatchdog) {
  Rig rig;
  KeyEvent e;
  rig.dispatcher->Pump(e);
  rig.dispatcher->Pump(e);
  EXPECT_EQ(rig.feeds, 2);
}

// Client §10's residual case: the input layer itself wedges, so nothing polls
// the source and the key is never even read. The watchdog's expiry runs the
// wipe before the reset.
TEST(C11, WatchdogExpiryRunsTheWipeWhenNothingPumpsAnyMore) {
  chaski_test::FakeClock clock;
  Trace trace;
  chaski::input::WatchdogDeps wd;
  wd.on_expiry = [&trace] { trace.Add("wipe"); };
  wd.monotonic_ms = [&clock] { return clock.Now(); };
  auto watchdog = chaski::input::NewWatchdog(wd);

  ScriptedSource source;
  DispatcherDeps d;
  d.source = &source;
  d.feed_watchdog = [&watchdog] { watchdog->Feed(); };
  auto dispatcher = chaski::input::NewDispatcher(d);

  // A healthy loop first: pumping keeps it fed across many timeouts' worth of
  // time, so the expiry below cannot be an artefact of the clock advancing.
  KeyEvent e;
  for (int i = 0; i < 10; ++i) {
    clock.Advance(chaski::input::kWatchdogTimeoutMs / 2);
    dispatcher->Pump(e);
    EXPECT_FALSE(watchdog->Overdue());
  }

  // Now the input layer wedges: no more pumps, so no more feeds.
  clock.Advance(chaski::input::kWatchdogTimeoutMs);
  ASSERT_TRUE(watchdog->Overdue());
  watchdog->Expire();
  EXPECT_EQ(trace.Count("wipe"), 1);
}

// Expiry must not depend on the dispatcher: the premise is that the object
// owning the input state is stuck, and a recovery path that has to reach
// through the stuck object to work is not a recovery path.
TEST(C11, WatchdogWipesWithNoDispatcherAtAll) {
  chaski_test::FakeClock clock;
  int wipes = 0;
  chaski::input::WatchdogDeps wd;
  wd.on_expiry = [&wipes] { ++wipes; };
  wd.monotonic_ms = [&clock] { return clock.Now(); };
  auto watchdog = chaski::input::NewWatchdog(wd);

  clock.Advance(chaski::input::kWatchdogTimeoutMs);
  EXPECT_TRUE(watchdog->Overdue());
  watchdog->Expire();
  EXPECT_EQ(wipes, 1);
}

// After an expiry the timer restarts, so a supervisor polling Overdue does not
// re-trip on the same stale timestamp and wipe in a loop.
TEST(Input, WatchdogRearmsAfterExpiry) {
  chaski_test::FakeClock clock;
  int wipes = 0;
  chaski::input::WatchdogDeps wd;
  wd.on_expiry = [&wipes] { ++wipes; };
  wd.monotonic_ms = [&clock] { return clock.Now(); };
  auto watchdog = chaski::input::NewWatchdog(wd);

  clock.Advance(chaski::input::kWatchdogTimeoutMs);
  watchdog->Expire();
  EXPECT_FALSE(watchdog->Overdue());

  clock.Advance(chaski::input::kWatchdogTimeoutMs);
  EXPECT_TRUE(watchdog->Overdue());
  watchdog->Expire();
  EXPECT_EQ(wipes, 2);
}

// The expiry handler must not be re-entered from inside itself, for the reason
// put-away's must not: a second waveform on a panel mid-transition.
TEST(Input, WatchdogExpiryIsNotReentered) {
  chaski_test::FakeClock clock;
  int wipes = 0;
  chaski::input::Watchdog* self = nullptr;
  chaski::input::WatchdogDeps wd;
  wd.on_expiry = [&wipes, &self] {
    ++wipes;
    if (self != nullptr) self->Expire();
  };
  wd.monotonic_ms = [&clock] { return clock.Now(); };
  auto watchdog = chaski::input::NewWatchdog(wd);
  self = watchdog.get();

  clock.Advance(chaski::input::kWatchdogTimeoutMs);
  watchdog->Expire();
  EXPECT_EQ(wipes, 1);
}

// A dispatcher wired with no handlers is a composition-root mistake, not a
// crash. Worth pinning: the alternative is a null call on the wipe path.
TEST(Input, MissingHandlersAreNotFatal) {
  ScriptedSource source;
  source.Push(Key::kPutAway);
  source.Push(Key::kSync);
  source.PushChar('a');
  DispatcherDeps d;
  d.source = &source;
  auto dispatcher = chaski::input::NewDispatcher(d);

  KeyEvent e;
  EXPECT_FALSE(dispatcher->Pump(e));
}
