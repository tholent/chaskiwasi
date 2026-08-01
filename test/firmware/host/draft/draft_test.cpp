// C-17: the draft survives timeout, put-away, and simulated power loss
// mid-compose (client §11.3).
//
// Timeout and put-away are the UI calling Save on the way into the wipe; both
// reduce here to "the last Save reached flash". Power loss is the store object
// being destroyed without ceremony and reopened over the same directory, which
// is the only kind of power cut a host test can honestly simulate — and the
// only one that matters for this file, since every write is atomic.
#include "chaski/draft.h"

#include <memory>
#include <string>

#include <gtest/gtest.h>

#include "chaski/fsutil.h"
#include "temp_root.h"

namespace {

using chaski::store::Draft;
using chaski::store::OpenDraftStore;
using chaski::store::StartOutcome;
using chaski_test::TempRoot;

Draft Compose(const std::string& body) {
  Draft d;
  d.contact_id = "c_rosa";
  d.subject = "camping";
  d.body = body;
  d.updated_at = 1700000000;
  return d;
}

TEST(C17, DraftSurvivesPowerLossMidComposeAndIsOfferedOnTheNextWake) {
  TempRoot root;
  {
    auto drafts = OpenDraftStore(root.path());
    EXPECT_FALSE(drafts->Pending());
    EXPECT_EQ(drafts->Start(Compose("we went to the")), StartOutcome::kStarted);
    // Autosave at the ~30 s mark, then the lights go out.
    EXPECT_TRUE(drafts->Save(Compose("we went to the lake and")));
  }

  auto drafts = OpenDraftStore(root.path());
  ASSERT_TRUE(drafts->Pending());
  Draft got;
  ASSERT_TRUE(drafts->Load(got));
  EXPECT_EQ(got.body, "we went to the lake and");
  EXPECT_EQ(got.contact_id, "c_rosa");
  EXPECT_EQ(got.subject, "camping");
  EXPECT_EQ(got.updated_at, 1700000000);
}

// The put-away key wipes from any screen with no confirmation (client §10),
// which means the autosave it triggers is the last chance the text gets. Same
// shape as the timeout path, asserted separately because they are separate
// triggers in §11.3 and a regression in either loses words.
TEST(C17, PutAwayAndTimeoutAutosavesArePreserved) {
  TempRoot root;
  auto drafts = OpenDraftStore(root.path());
  ASSERT_EQ(drafts->Start(Compose("half a")), StartOutcome::kStarted);

  EXPECT_TRUE(drafts->Save(Compose("half a sentence")));  // ~45 s timeout wipe
  EXPECT_TRUE(drafts->Save(Compose("half a sentence and more")));  // put-away

  Draft got;
  ASSERT_TRUE(drafts->Load(got));
  EXPECT_EQ(got.body, "half a sentence and more");
}

// §11.3: one draft slot in v1, so starting a new letter with one pending asks
// which to keep. The refusal is what makes that a decision the child makes
// rather than one the firmware makes for them by overwriting.
TEST(C17, StartingANewLetterWithADraftPendingIsADecisionPoint) {
  TempRoot root;
  auto drafts = OpenDraftStore(root.path());
  ASSERT_EQ(drafts->Start(Compose("first letter")), StartOutcome::kStarted);

  EXPECT_EQ(drafts->Start(Compose("second letter")), StartOutcome::kDraftPending);
  Draft got;
  ASSERT_TRUE(drafts->Load(got));
  EXPECT_EQ(got.body, "first letter");  // untouched

  // "Start new" is Discard then Start — an explicit act, at the child's word.
  ASSERT_TRUE(drafts->Discard());
  EXPECT_FALSE(drafts->Pending());
  EXPECT_EQ(drafts->Start(Compose("second letter")), StartOutcome::kStarted);
  ASSERT_TRUE(drafts->Load(got));
  EXPECT_EQ(got.body, "second letter");
}

// Autosave is the same draft being written again, so it replaces the slot
// without asking. Only Start is a decision point.
TEST(C17, AutosaveReplacesTheSlotWithoutAsking) {
  TempRoot root;
  auto drafts = OpenDraftStore(root.path());
  ASSERT_EQ(drafts->Start(Compose("a")), StartOutcome::kStarted);
  for (const char* body : {"ab", "abc", "abcd"}) {
    ASSERT_TRUE(drafts->Save(Compose(body)));
  }
  Draft got;
  ASSERT_TRUE(drafts->Load(got));
  EXPECT_EQ(got.body, "abcd");
}

TEST(C17, DiscardClearsTheSlotDurably) {
  TempRoot root;
  {
    auto drafts = OpenDraftStore(root.path());
    ASSERT_EQ(drafts->Start(Compose("sent already")), StartOutcome::kStarted);
    ASSERT_TRUE(drafts->Discard());
    EXPECT_FALSE(drafts->Pending());
  }
  auto drafts = OpenDraftStore(root.path());
  EXPECT_FALSE(drafts->Pending());
  Draft got;
  EXPECT_FALSE(drafts->Load(got));
}

TEST(C17, DiscardingAnEmptySlotSucceeds) {
  TempRoot root;
  auto drafts = OpenDraftStore(root.path());
  EXPECT_TRUE(drafts->Discard());
}

// An abandoned picker — a person chosen, nothing typed — is not a draft. It
// must not raise the conflict prompt on the next compose, or every stray key
// press would cost the child a question.
TEST(C17, EmptyBodyAndSubjectIsNotAPendingDraft) {
  TempRoot root;
  auto drafts = OpenDraftStore(root.path());
  Draft empty;
  empty.contact_id = "c_rosa";
  ASSERT_TRUE(drafts->Save(empty));
  EXPECT_FALSE(drafts->Pending());
  EXPECT_EQ(drafts->Start(Compose("a fresh letter")), StartOutcome::kStarted);
}

// A slot that cannot be decoded must not lock the child out of composing —
// the same reasoning that keeps terminally rejected letters out of the outbox
// cap (B.9): a stuck state must never become a lockout.
TEST(C17, UndecodableDraftDoesNotBlockANewLetter) {
  TempRoot root;
  ASSERT_TRUE(chaski::fsutil::WriteAtomic(
      chaski::fsutil::Join(root.path(), "draft"), "\x01\x02 not a record"));

  auto drafts = OpenDraftStore(root.path());
  EXPECT_FALSE(drafts->Pending());
  Draft got;
  EXPECT_FALSE(drafts->Load(got));
  EXPECT_EQ(drafts->Start(Compose("starting over")), StartOutcome::kStarted);
  ASSERT_TRUE(drafts->Load(got));
  EXPECT_EQ(got.body, "starting over");
}

// Bodies carry newlines and backslashes; the record format escapes them. A
// draft that came back mangled would be the device editing the child's words.
TEST(C17, BodyBytesRoundTripExactly) {
  TempRoot root;
  const std::string body = "line one\nline two\\ still two\r\nthree";
  {
    auto drafts = OpenDraftStore(root.path());
    Draft d = Compose(body);
    d.subject = "an=odd\nsubject";
    ASSERT_EQ(drafts->Start(d), StartOutcome::kStarted);
  }
  auto drafts = OpenDraftStore(root.path());
  Draft got;
  ASSERT_TRUE(drafts->Load(got));
  EXPECT_EQ(got.body, body);
  EXPECT_EQ(got.subject, "an=odd\nsubject");
}

}  // namespace
