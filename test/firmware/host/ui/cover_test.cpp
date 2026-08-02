// C-12: the cover shows the wordmark, the battery, and a mail flag — never a
// count, never a name, never a byte of content (D-1, B.5).
//
// The strongest form of this assertion is not "the picture looks right", which
// no host can judge, but "two devices whose only difference is how much mail
// is waiting produce exactly the same screen". If a count could leak, that
// equality is what would break.
#include <gtest/gtest.h>

#include <string>

#include "ui_fixture.h"

namespace {

using chaski::ui::Glyph;
using chaski::ui::Screen;
using chaski_test::Env;
using chaski_test::RecordingPainter;
using chaski_test::SampleAyllu;
using chaski_test::SampleLetter;

chaski::panel::Framebuf Paint(const chaski::panel::CoverState& s,
                              RecordingPainter& painter) {
  chaski::panel::Framebuf fb;
  chaski::ui::RenderCover(s, painter, &chaski_text, fb);
  return fb;
}

TEST(C12, TheCoverDrawsTheWordmarkTheBatteryAndNothingElse) {
  chaski_test::Trace trace;
  RecordingPainter painter(&trace);
  chaski::panel::CoverState s;
  s.battery_pct = 64;
  s.any_unread = true;
  Paint(s, painter);

  EXPECT_TRUE(painter.Drew(S(STR_APP_NAME)));
  EXPECT_TRUE(painter.Drew("64"));
  EXPECT_EQ(painter.text.size(), 2u) << "the cover drew text it was not given";

  bool flag = false;
  for (Glyph g : painter.glyphs) flag = flag || g == Glyph::kMailFlag;
  EXPECT_TRUE(flag);
}

// B.5: a raised flag, not a number. The renderer receives a boolean and there
// is no path by which a count could reach it — this test fails the moment
// someone adds one.
TEST(C12, TheCoverIsIdenticalWhateverIsWaiting) {
  chaski_test::Trace one_trace;
  chaski_test::Trace many_trace;
  RecordingPainter one(&one_trace);
  RecordingPainter many(&many_trace);

  Env a;
  ASSERT_TRUE(a.contacts->ApplySnapshot(SampleAyllu()));
  ASSERT_TRUE(a.letters->Put(SampleLetter("l-0000000001", "c_rosa", "hi", "one", 1700000100)));
  a.Build();

  Env b;
  ASSERT_TRUE(b.contacts->ApplySnapshot(SampleAyllu()));
  for (int i = 0; i < 7; ++i) {
    ASSERT_TRUE(b.letters->Put(SampleLetter("l-000000000" + std::to_string(i), "c_rosa",
                                            "hi", "letter " + std::to_string(i),
                                            1700000100 + i)));
  }
  b.Build();

  ASSERT_EQ(a.letters->UnreadCount(), 1u);
  ASSERT_EQ(b.letters->UnreadCount(), 7u);

  const chaski::panel::Framebuf fa = Paint(a.app->Cover(), one);
  const chaski::panel::Framebuf fb = Paint(b.app->Cover(), many);

  EXPECT_EQ(one.text, many.text);
  EXPECT_EQ(one.glyphs, many.glyphs);
  EXPECT_EQ(one_trace.steps, many_trace.steps);
  EXPECT_EQ(fa.bits, fb.bits);
}

// No senders, no subjects, no bodies. The mechanism is the signature —
// CoverState carries none of them — and this is the test that says so.
TEST(C12, TheCoverNeverCarriesANameOrAnythingFromALetter) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  ASSERT_TRUE(env.letters->Put(SampleLetter("l-0000000001", "c_rosa", "camping",
                                            "we went to the lake", 1700000100)));
  env.Build();

  chaski_test::Trace trace;
  RecordingPainter painter(&trace);
  Paint(env.app->Cover(), painter);

  for (const std::string& drawn : painter.text) {
    EXPECT_EQ(drawn.find("Rosa"), std::string::npos);
    EXPECT_EQ(drawn.find("camping"), std::string::npos);
    EXPECT_EQ(drawn.find("lake"), std::string::npos);
  }
}

// The flag is down when nothing is unread, and reading the last letter puts it
// down — the flag is the whole reason to open the device between syncs.
TEST(C12, TheFlagIsUpOnlyWhileSomethingIsUnread) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  EXPECT_FALSE(env.app->Cover().any_unread);

  ASSERT_TRUE(env.letters->Put(SampleLetter("l-0000000001", "c_rosa", "hi", "one", 1700000100)));
  EXPECT_TRUE(env.app->Cover().any_unread);

  env.app->Start(Screen::kInbox);
  env.Press(chaski::input::Key::kEnter);
  EXPECT_FALSE(env.app->Cover().any_unread);
}

// design §4.1: never a blank white panel. A resting device that looks dead is
// a device a child stops carrying — and the charge-me cover is the same
// composition with the battery emphasised, not a different screen.
TEST(C12, TheChargeMeCoverStillDrawsSomething) {
  chaski_test::Trace trace;
  RecordingPainter painter(&trace);
  chaski::panel::CoverState s;
  s.kind = chaski::panel::CoverKind::kChargeMe;
  s.battery_pct = 3;
  Paint(s, painter);

  EXPECT_TRUE(painter.Drew(S(STR_APP_NAME)));
  EXPECT_TRUE(painter.Drew("3"));
  EXPECT_FALSE(painter.glyphs.empty());
}

// The cover screen itself shows no list and no lines: whatever is on the glass
// at rest came from CoverState, not from the view model.
TEST(C12, TheCoverScreenHasNoRowsAndNoLines) {
  Env env;
  chaski_test::SampleAyllu();
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  ASSERT_TRUE(env.letters->Put(SampleLetter("l-0000000001", "c_rosa", "camping", "lake", 1700000100)));
  env.Build();
  env.app->Start(Screen::kCover);

  EXPECT_EQ(env.app->Current(), Screen::kCover);
  EXPECT_TRUE(env.View().rows.empty());
  EXPECT_TRUE(env.View().lines.empty());
  EXPECT_TRUE(env.View().title.empty());
}

// The wipe controller takes a std::function and calls it with CoverState
// alone; this is the adapter that makes that possible (client §9).
TEST(C12, TheWipeControllerRendersTheCoverThroughTheSameFunction) {
  chaski_test::Trace trace;
  RecordingPainter painter(&trace);
  auto render = chaski::ui::CoverRenderer(&painter, &chaski_text);
  ASSERT_TRUE(static_cast<bool>(render));

  chaski::panel::CoverState s;
  s.battery_pct = 50;
  chaski::panel::Framebuf fb;
  render(s, fb);
  EXPECT_TRUE(painter.Drew(S(STR_APP_NAME)));
  EXPECT_GT(fb.bits.size(), 0u);
}

}  // namespace
