// The screens of client §11, driven through the state machine.
//
// C-13 (tombstones and c_sys), C-14 (an unknown contact id), C-17 (drafts
// never die) live here, together with the flows whose failure modes are
// promises rather than crashes: a rejected letter that keeps its text, a
// counter that counts what the server counts, a cap that parks a letter
// instead of refusing to let a child write.
#include <gtest/gtest.h>

#include <string>

#include "chaski/layout.h"
#include "ui_fixture.h"

namespace {

using chaski::input::Key;
using chaski::ui::Screen;
using chaski_test::Env;
using chaski_test::SampleAyllu;
using chaski_test::SampleLetter;

// Seeded so every letter-facing test has the four contact cases and one letter
// from each of the interesting ones.
void SeedInbox(Env& env) {
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  ASSERT_TRUE(env.letters->Put(
      SampleLetter("l-0000000001", "c_rosa", "camping", "we went to the lake", 1700000100)));
  ASSERT_TRUE(env.letters->Put(
      SampleLetter("l-0000000002", "c_gone", "old news", "from before", 1700000200)));
  ASSERT_TRUE(env.letters->Put(
      SampleLetter("l-0000000003", "c_sys", "a change", "someone was added", 1700000300)));
  ASSERT_TRUE(env.letters->Put(
      SampleLetter("l-0000000004", "c_nobody", "who?", "a letter with no sender we know", 1700000400)));
}

// §11.2 / design §4: the picker is the only way into writing, and it holds
// only people the child may write to. A tombstone in this list would offer a
// letter that server §7.2 will reject terminally — a "couldn't send" the child
// did nothing to deserve.
TEST(C13, TheComposePickerExcludesTombstonesAndTheSystemContact) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);

  env.Press(Key::kRight);  // inbox -> write to
  ASSERT_EQ(env.app->Current(), Screen::kComposePick);

  EXPECT_TRUE(env.RowSays("Rosa"));
  EXPECT_TRUE(env.RowSays("Dad"));
  EXPECT_FALSE(env.RowSays("Tio Beto"));
  EXPECT_FALSE(env.RowSays(S(STR_SYSTEM_SENDER)));
  EXPECT_EQ(env.View().rows.size(), 2u);
}

// The other half of the same rule: you can still READ Rosa's old letters, you
// just cannot write to her (server §7.2, A.15). A tombstone whose letters lost
// their sender name would make the archive unreadable to keep a list tidy.
TEST(C13, LettersFromATombstoneAndFromWasiStillCarryTheirName) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);

  ASSERT_EQ(env.View().rows.size(), 4u);
  EXPECT_TRUE(env.RowSays("Tio Beto"));
  EXPECT_TRUE(env.RowSays(S(STR_SYSTEM_SENDER)));
}

// A.16: the system sender is "Wasi", and it is rendered from the contact list
// like anyone else — no special case, which is why it ships as a tombstone.
TEST(C13, NoticeLettersRenderFromWasi) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);

  bool found = false;
  for (const chaski::ui::Row& r : env.View().rows) {
    if (r.secondary == "a change") {
      EXPECT_EQ(r.primary, S(STR_SYSTEM_SENDER));
      found = true;
    }
  }
  EXPECT_TRUE(found);
}

// C-14: a lookup miss is not an error and must never cost a letter. Dropping
// it would lose a letter to a bookkeeping gap the child had no part in.
TEST(C14, AnUnknownContactIdRendersTheFallbackLabelAndTheLetterIsKept) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);

  ASSERT_EQ(env.View().rows.size(), 4u);
  bool found = false;
  for (const chaski::ui::Row& r : env.View().rows) {
    if (r.secondary == "who?") {
      EXPECT_EQ(r.primary, S(STR_CONTACT_UNKNOWN));
      EXPECT_FALSE(r.primary.empty());
      found = true;
    }
  }
  EXPECT_TRUE(found) << "the letter with an unknown sender was dropped";
}

// Opening a letter marks it read LOCALLY. The server holds no engagement
// state and no read receipt crosses the wire (§1.1, §11.1).
TEST(Ui, OpeningALetterMarksItReadOnTheDeviceOnly) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);
  ASSERT_EQ(env.letters->UnreadCount(), 4u);

  env.Press(Key::kEnter);
  EXPECT_EQ(env.app->Current(), Screen::kRead);
  EXPECT_EQ(env.letters->UnreadCount(), 3u);
}

// §8.2: `truncated` says the archive at home continues where the device
// stops. `degraded` is server bookkeeping and renders nothing at all.
TEST(Ui, TruncatedSaysTheArchiveContinuesAndDegradedSaysNothing) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  chaski::wire::Letter l = SampleLetter("l-0000000009", "c_rosa", "long", "short body", 1700000100);
  l.truncated = true;
  l.degraded = true;
  ASSERT_TRUE(env.letters->Put(l));
  env.Build();
  env.app->Start(Screen::kInbox);
  env.Press(Key::kEnter);

  ASSERT_EQ(env.app->Current(), Screen::kRead);
  EXPECT_TRUE(env.LineSays("archive"));
  EXPECT_EQ(env.View().title, "Rosa");
}

// §5.6 / C-21: no battery-backed RTC, so a date before the first sync would be
// a guess. Blank is the honest rendering.
TEST(C21, DatesAreBlankUntilTheClockIsValid) {
  Env env;
  SeedInbox(env);
  env.clock_valid = false;
  env.Build();
  env.app->Start(Screen::kInbox);
  for (const chaski::ui::Row& r : env.View().rows) EXPECT_TRUE(r.meta.empty());

  Env dated;
  SeedInbox(dated);
  dated.Build();
  dated.app->Start(Screen::kInbox);
  bool any = false;
  for (const chaski::ui::Row& r : dated.View().rows) any = any || !r.meta.empty();
  EXPECT_TRUE(any);
}

// ---- compose -------------------------------------------------------------

// The counter counts extended grapheme clusters, which is what the server
// counts (server §0) and what the reader perceives. A ZWJ family is ONE
// character by that definition and five code points by any other; counting the
// other way would let the compose screen accept a letter the server rejects.
TEST(Ui, TheComposeCounterCountsGraphemesAcrossAZwjBoundary) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  ASSERT_EQ(env.app->Current(), Screen::kComposeWrite);

  // U+1F468 ZWJ U+1F469 ZWJ U+1F467 — one cluster, five code points.
  const unsigned int cps[] = {0x1F468, 0x200D, 0x1F469, 0x200D, 0x1F467};
  for (unsigned int cp : cps) env.TypeCp(cp);

  const std::string family = "\xF0\x9F\x91\xA8\xE2\x80\x8D\xF0\x9F\x91\xA9\xE2\x80\x8D\xF0\x9F\x91\xA7";
  EXPECT_EQ(chaski::layout::CountGraphemes(family), 1u);
  EXPECT_EQ(env.View().graphemes_used, chaski::layout::CountGraphemes(family));

  env.Type("hi");
  EXPECT_EQ(env.View().graphemes_used, 3u);
}

// A combining mark joins the cluster before it. Counting it as a new character
// would end a letter early for anyone writing an accented language.
TEST(Ui, ACombiningMarkDoesNotAdvanceTheCounter) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);

  env.TypeCp('e');
  EXPECT_EQ(env.View().graphemes_used, 1u);
  env.TypeCp(0x0301);  // COMBINING ACUTE ACCENT
  EXPECT_EQ(env.View().graphemes_used, 1u);
}

TEST(Ui, TheCounterStopsAtThePushedCapAndSaysSo) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  chaski::wire::DeviceConfig c;
  c.max_letter_chars = 3;
  ASSERT_TRUE(env.settings->ApplyConfig(c));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);

  env.Type("abcd");
  EXPECT_EQ(env.View().graphemes_used, 3u);
  EXPECT_EQ(env.View().graphemes_max, 3u);
  EXPECT_EQ(Env::Joined(env.View().message), S(STR_COMPOSE_TOO_LONG));
}

// B.9 / F-C5: at the cap the finished letter parks as the draft. Refusing to
// let a child write is never the right failure, and the cap counts letters
// waiting for the runner, not rejected ones.
TEST(Ui, AFullBagParksTheLetterAsTheDraftInsteadOfRefusingIt) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  for (std::size_t i = 0; i < chaski::store::kOutboxCap; ++i) {
    std::string id;
    ASSERT_TRUE(env.outbox->Add("c_rosa", "s", "queued letter", 0, id));
  }
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  env.Type("one more");
  env.Press(Key::kEnter);  // send

  EXPECT_EQ(Env::Joined(env.View().message), S(STR_OUTBOX_FULL));
  EXPECT_EQ(env.outbox->SendableCount(), chaski::store::kOutboxCap);
  ASSERT_TRUE(env.drafts->Pending());
  chaski::store::Draft parked;
  ASSERT_TRUE(env.drafts->Load(parked));
  EXPECT_EQ(parked.body, "one more");
}

TEST(Ui, SendingMovesTheLetterToTheOutboxAndClearsTheDraft) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);  // the first composable contact
  env.Press(Key::kUp);     // subject field
  env.Type("camping");
  env.Press(Key::kDown);   // body
  env.Type("we saw a fox");
  env.Press(Key::kEnter);

  EXPECT_EQ(env.app->Current(), Screen::kOutbox);
  EXPECT_EQ(Env::Joined(env.View().message), S(STR_COMPOSE_SENT));
  ASSERT_EQ(env.outbox->All().size(), 1u);
  EXPECT_EQ(env.outbox->All()[0].body, "we saw a fox");
  EXPECT_EQ(env.outbox->All()[0].subject, "camping");
  EXPECT_FALSE(env.drafts->Pending());
}

TEST(Ui, AnEmptyLetterIsNotSent) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  env.Press(Key::kEnter);  // send with nothing written

  EXPECT_EQ(env.app->Current(), Screen::kComposeWrite);
  EXPECT_EQ(Env::Joined(env.View().message), S(STR_COMPOSE_EMPTY));
  EXPECT_TRUE(env.outbox->All().empty());
}

// §5.6: an invalid clock stamps 0 rather than a wrong time. The server stamps
// the letter on receipt anyway, so nothing is lost by declining to guess.
TEST(C21, AComposeTimestampIsDeferredWhileTheClockIsInvalid) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.clock_valid = false;
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  env.Type("hello");
  env.Press(Key::kEnter);

  ASSERT_EQ(env.outbox->All().size(), 1u);
  EXPECT_EQ(env.outbox->All()[0].composed_at, 0);
}

// ---- rejected letters ----------------------------------------------------

// §5.4 and D-5: a terminal reject keeps the child's text and offers one key to
// write it again. The words are the child's; a status code is not a reason to
// take them away.
TEST(Ui, ARejectedLetterKeepsItsTextAndOneKeyRecomposesIt) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  std::string local_id;
  ASSERT_TRUE(env.outbox->Add("c_rosa", "camping", "we saw a fox", 0, local_id));
  ASSERT_TRUE(env.outbox->Resolve(local_id, chaski::wire::AckStatus::kRejectedInactive));
  env.Build();
  env.app->Start(Screen::kOutbox);

  ASSERT_EQ(env.View().rows.size(), 1u);
  EXPECT_EQ(env.View().rows[0].meta, S(STR_STATE_COULDNT_SEND));
  EXPECT_EQ(Env::Joined(env.View().message), S(STR_SEND_FAILED));

  env.Press(Key::kEnter);
  EXPECT_EQ(env.app->Current(), Screen::kComposeWrite);
  EXPECT_EQ(env.View().graphemes_used, chaski::layout::CountGraphemes("we saw a fox"));

  // The text now lives in the draft slot; the outbox entry was retired only
  // after it got there.
  chaski::store::Draft d;
  ASSERT_TRUE(env.drafts->Load(d));
  EXPECT_EQ(d.body, "we saw a fox");
  EXPECT_EQ(d.subject, "camping");
  EXPECT_EQ(d.contact_id, "c_rosa");
  EXPECT_TRUE(env.outbox->All().empty());
}

// §5.4: the device never tells the child WHICH reject it was. Those
// distinctions are for guardians, server-side, and a child cannot act on any
// of them.
TEST(Ui, TheDeviceNeverDistinguishesRejectReasons) {
  const chaski::wire::AckStatus kRejects[] = {
      chaski::wire::AckStatus::kRejectedInactive,
      chaski::wire::AckStatus::kRejectedUnknownContact,
      chaski::wire::AckStatus::kInvalid,
      chaski::wire::AckStatus::kRejectedUndeliverable,
      chaski::wire::AckStatus::kUnknown,
  };
  for (chaski::wire::AckStatus s : kRejects) {
    Env env;
    ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
    std::string local_id;
    ASSERT_TRUE(env.outbox->Add("c_rosa", "camping", "we saw a fox", 0, local_id));
    ASSERT_TRUE(env.outbox->Resolve(local_id, s));
    env.Build();
    env.app->Start(Screen::kOutbox);

    EXPECT_EQ(Env::Joined(env.View().message), S(STR_SEND_FAILED));
    EXPECT_EQ(env.View().rows[0].meta, S(STR_STATE_COULDNT_SEND));
  }
}

// A letter with the runner is not something to poke at: only a rejected row
// answers the key (§5.4).
TEST(Ui, AWaitingLetterShowsItsStateAndDoesNotRecompose) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  std::string local_id;
  ASSERT_TRUE(env.outbox->Add("c_rosa", "camping", "we saw a fox", 0, local_id));
  env.Build();
  env.app->Start(Screen::kOutbox);

  EXPECT_EQ(env.View().rows[0].meta, S(STR_STATE_WAITING));
  env.Press(Key::kEnter);
  EXPECT_EQ(env.app->Current(), Screen::kOutbox);
  EXPECT_EQ(env.outbox->All().size(), 1u);
}

// ---- drafts (C-17) -------------------------------------------------------

// §11.3: autosave on every wipe trigger. Put-away runs below UI dispatch and
// gives the screen no chance to ask, so this save is the last one the words
// get.
TEST(C17, PutAwayMidLetterSavesTheDraftAndTheNextWakeOffersIt) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  env.Type("we went to the");
  env.app->AutosaveDraft();  // what the put-away path calls before wiping

  chaski::store::Draft d;
  ASSERT_TRUE(env.drafts->Load(d));
  EXPECT_EQ(d.body, "we went to the");

  // Next wake: a session over the same storage offers to continue.
  Env woken;
  (void)woken;
  env.Build();
  env.app->Start(Screen::kInbox);
  EXPECT_EQ(env.app->Current(), Screen::kDraftConflict);
  EXPECT_EQ(Env::Joined(env.View().message), S(STR_DRAFT_RESUME));

  env.Press(Key::kEnter);  // "keep writing that one"
  EXPECT_EQ(env.app->Current(), Screen::kComposeWrite);
  EXPECT_EQ(env.View().graphemes_used, chaski::layout::CountGraphemes("we went to the"));
}

// The other autosave trigger: ~30 s of typing (§11.3). Flash writes cost
// energy and wear, so this is a floor and not a per-keystroke save.
TEST(C17, TypingAutosavesOnTheIdleCadence) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  env.Type("half a sentence");

  // Before the cadence fires the words are in RAM only. Note the slot does not
  // merely hold an empty body — it does not read back at all: a draft with
  // nothing written in it is deliberately indistinguishable from no draft
  // (draft.h), so that an empty slot can never become a conflict prompt the
  // child has to dismiss before writing.
  chaski::store::Draft d;
  EXPECT_FALSE(env.drafts->Load(d))
      << "the slot must not be rewritten per keystroke; flash writes are what "
         "this device spends its wear and battery budget on";

  env.Advance(chaski::store::kDraftAutosaveIntervalMs);
  ASSERT_TRUE(env.drafts->Load(d));
  EXPECT_EQ(d.body, "half a sentence");
}

// §11.3: one slot, so starting a new letter with one pending is a decision the
// child makes. A silent overwrite is the failure this screen exists to stop.
TEST(C17, StartingANewLetterWithADraftPendingIsADecisionPoint) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  chaski::store::Draft pending;
  pending.contact_id = "c_rosa";
  pending.body = "first letter";
  ASSERT_EQ(env.drafts->Start(pending), chaski::store::StartOutcome::kStarted);
  env.Build();
  env.app->Start(Screen::kInbox);

  // The resume prompt comes first; decline it, then start a new letter.
  ASSERT_EQ(env.app->Current(), Screen::kDraftConflict);
  env.Press(Key::kDown);
  env.Press(Key::kEnter);  // "let it go"
  ASSERT_EQ(env.app->Current(), Screen::kInbox);
  EXPECT_FALSE(env.drafts->Pending());
}

TEST(C17, KeepingTheOpenDraftLeavesTheNewLetterUnstarted) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  env.Type("first letter");
  env.app->AutosaveDraft();

  // Leave the writing screen and pick a different person: the slot is taken,
  // so the child is asked rather than overwritten.
  env.Press(Key::kBack);
  while (!env.View().rows.empty() && env.app->Current() == Screen::kComposeWrite) {
    env.Press(Key::kBack);
  }
  env.app->Start(Screen::kComposePick);
  ASSERT_EQ(env.app->Current(), Screen::kDraftConflict);

  env.Press(Key::kEnter);  // keep writing that one
  EXPECT_EQ(env.app->Current(), Screen::kComposeWrite);
  chaski::store::Draft d;
  ASSERT_TRUE(env.drafts->Load(d));
  EXPECT_EQ(d.body, "first letter");
}

// A person picked with nothing typed is not a draft. Otherwise every stray key
// press would cost the child a question on the next wake.
TEST(C17, PickingAPersonWithNothingTypedIsNotAPendingDraft) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  env.app->AutosaveDraft();
  EXPECT_FALSE(env.drafts->Pending());
}

// ---- fault states --------------------------------------------------------

// §11.6: every fault names an action a child can take, and every one of them
// has text. A fault with no string would be a blank screen, which reads as a
// broken device.
TEST(Ui, EveryFaultStateMapsToAStringThatExists) {
  const struct {
    chaski::ui::FaultKind kind;
    chaski_string_id_t id;
  } kCases[] = {
      {chaski::ui::FaultKind::kCantReachHome, STR_FAULT_CANT_REACH_HOME},
      {chaski::ui::FaultKind::kAskYourGuardians, STR_FAULT_ASK_GUARDIANS},
      {chaski::ui::FaultKind::kRoadBusy, STR_FAULT_ROAD_BUSY},
      {chaski::ui::FaultKind::kChargeMe, STR_FAULT_CHARGE_ME},
  };
  for (const auto& c : kCases) {
    Env env;
    env.Build();
    env.app->Start(Screen::kInbox);
    env.app->ShowFault(c.kind, "");
    EXPECT_EQ(env.app->Current(), Screen::kFault);
    ASSERT_FALSE(env.View().message.empty());
    EXPECT_EQ(Env::Joined(env.View().message), S(c.id));
  }
}

// §11.6: the code goes on a diagnostics line for a guardian, never as the
// primary text. "HTTP 401" is not an action a child can take.
TEST(Ui, AnErrorCodeIsNeverThePrimaryTextOfAFault) {
  Env env;
  env.Build();
  env.app->Start(Screen::kInbox);
  env.app->ShowFault(chaski::ui::FaultKind::kAskYourGuardians, "401");

  EXPECT_EQ(Env::Joined(env.View().message), S(STR_FAULT_ASK_GUARDIANS));
  EXPECT_EQ(Env::Joined(env.View().message).find("401"), std::string::npos);
  EXPECT_NE(env.View().diagnostic.find("401"), std::string::npos);
}

// §9 / design §4.1: below the battery floor the device refuses to open
// content. Not a suggestion — no key gets past it.
TEST(Ui, ChargeMeRefusesToOpenContent) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);
  env.app->ShowFault(chaski::ui::FaultKind::kChargeMe, "");

  for (Key k : {Key::kEnter, Key::kBack, Key::kUp, Key::kDown, Key::kRight}) {
    env.Press(k);
    EXPECT_EQ(env.app->Current(), Screen::kFault);
  }
  EXPECT_TRUE(env.View().rows.empty());
  EXPECT_EQ(env.app->Cover().kind, chaski::panel::CoverKind::kChargeMe);
}

// A fault that is about the road, not the device, lets the child carry on.
TEST(Ui, ARoadFaultCanBeDismissed) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);
  env.app->ShowFault(chaski::ui::FaultKind::kRoadBusy, "503");
  env.Press(Key::kBack);
  EXPECT_EQ(env.app->Current(), Screen::kInbox);
}

// ---- settings ------------------------------------------------------------

// design §3.7's third transparency mechanism: plain sentences, on the device,
// any time. A hex dump is disclosure that cannot be read, which is the same as
// no disclosure.
TEST(Ui, TheHealthLogReadsAsSentences) {
  Env env;
  env.Build();
  chaski::kipu::Entry e;
  e.at = env.epoch;
  e.block = chaski::kipu::Build(64, false, "lte-m", -70, 1, "0.1.0");
  env.health->Record(e);
  env.app->Start(Screen::kSettings);
  env.Press(Key::kDown);
  env.Press(Key::kDown);
  env.Press(Key::kDown);
  env.Press(Key::kEnter);

  ASSERT_EQ(env.app->Current(), Screen::kSettingsLog);
  ASSERT_EQ(env.View().lines.size(), 1u);
  const std::string& line = env.View().lines[0];
  EXPECT_NE(line.find("battery 64%"), std::string::npos);
  EXPECT_NE(line.find(S(STR_LOG_SIGNAL_GOOD)), std::string::npos);
  EXPECT_NE(line.find(S(STR_LOG_ONE_WAITING)), std::string::npos);
  EXPECT_EQ(line.find("kipu"), std::string::npos);
}

// §8.2 / A.10: font size is a runtime accessibility setting, so pagination is
// computed on the device and changing size repaginates locally. Nothing is
// re-downloaded, which is the whole reason no page count rides the wire.
TEST(Ui, ChangingFontSizeRepaginatesLocally) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  std::string body;
  for (int i = 0; i < 60; ++i) body += "a long sentence about the lake ";
  ASSERT_TRUE(env.letters->Put(SampleLetter("l-000000000a", "c_rosa", "lake", body, 1700000100)));
  env.Build();
  env.app->Start(Screen::kInbox);
  env.Press(Key::kEnter);
  const int small_pages = env.View().page_count;
  ASSERT_GT(small_pages, 1);

  env.Press(Key::kBack);
  env.Press(Key::kRight);  // write to
  env.Press(Key::kRight);  // on the road
  env.Press(Key::kRight);  // settings
  ASSERT_EQ(env.app->Current(), Screen::kSettings);
  env.Press(Key::kEnter);  // cycle text size
  EXPECT_EQ(env.settings->Get().font_step, 1);

  env.Press(Key::kRight);  // back round to letters
  ASSERT_EQ(env.app->Current(), Screen::kInbox);
  env.Press(Key::kEnter);
  EXPECT_GT(env.View().page_count, small_pages);
}

// B.3: cosmetics are device-local decoration over guardian-owned membership.
// Nothing here is sent anywhere, and clearing a nickname lets the guardian's
// name show through again (§4.4).
TEST(Ui, ANicknameIsStoredAsALocalOverlayAndCanBeCleared) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kSettings);
  env.Press(Key::kDown);
  env.Press(Key::kDown);
  env.Press(Key::kEnter);
  ASSERT_EQ(env.app->Current(), Screen::kSettingsContacts);

  // Select Dad (pinned first in the child's order) and rename him.
  ASSERT_FALSE(env.View().rows.empty());
  const std::string original = env.View().rows[0].primary;
  env.Press(Key::kEnter);
  ASSERT_TRUE(env.View().editing);
  for (std::size_t i = 0; i < original.size(); ++i) env.Press(Key::kBack);
  env.Type("Papi");
  env.Press(Key::kEnter);

  chaski::ayllu::Contact c;
  ASSERT_TRUE(env.contacts->Lookup("c_dad", c));
  EXPECT_EQ(c.name, "Papi");
  EXPECT_EQ(c.server_name, "Dad");

  // Clearing it restores the guardian's name.
  env.Press(Key::kEnter);
  for (int i = 0; i < 4; ++i) env.Press(Key::kBack);
  env.Press(Key::kEnter);
  ASSERT_TRUE(env.contacts->Lookup("c_dad", c));
  EXPECT_EQ(c.name, "Dad");
}

// ---- refresh discipline --------------------------------------------------

// §8.3: never a partial refresh per keystroke — batched at a word boundary or
// a short idle. This is design §7's largest power lever, and it is also what
// keeps the panel from ghosting its way through a compose session.
TEST(Ui, TypingDoesNotRefreshThePanelPerKeystroke) {
  Env env;
  ASSERT_TRUE(env.contacts->ApplySnapshot(SampleAyllu()));
  env.Build();
  env.app->Start(Screen::kComposePick);
  env.Press(Key::kEnter);
  const int before = env.trace.Count("partial");

  env.Type("hello");
  EXPECT_EQ(env.trace.Count("partial"), before)
      << "a partial refresh landed on a keystroke inside a word";

  env.TypeCp(' ');  // a word boundary flushes
  EXPECT_GT(env.trace.Count("partial"), before);

  const int after_word = env.trace.Count("partial");
  env.Type("more");
  env.Advance(chaski::ui::kComposeIdleRefreshMs);
  EXPECT_GT(env.trace.Count("partial"), after_word) << "the idle batch never flushed";
}

// §8.3: entering a reading screen bounds ghosting with a full refresh, because
// on this device residue is a privacy parameter and not a cosmetic one.
TEST(Ui, OpeningALetterUsesAFullRefresh) {
  Env env;
  SeedInbox(env);
  env.Build();
  env.app->Start(Screen::kInbox);
  const int before = env.trace.Count("full");
  env.Press(Key::kEnter);
  EXPECT_GT(env.trace.Count("full"), before);
}

}  // namespace
