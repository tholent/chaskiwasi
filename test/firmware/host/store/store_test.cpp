// Host tests for components/store: the durable half of the letter path.
//
// Everything here runs with no ESP-IDF and no hardware, which is the point of
// ground rule 3. "Power loss" is simulated by destroying the store objects and
// reopening them over the same directory — what survives is exactly what
// reached flash.
#include "chaski/store.h"

#include <fcntl.h>
#include <unistd.h>

#include <cstdlib>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "chaski/fsutil.h"

namespace {

using chaski::store::OutboxEntry;
using chaski::store::StoredLetter;

// TempRoot stands in for one device's LittleFS mount for the length of a test.
class TempRoot {
 public:
  TempRoot() {
    char tmpl[] = "/tmp/chaski-store-XXXXXX";
    const char* d = ::mkdtemp(tmpl);
    path_ = d == nullptr ? "" : d;
  }
  ~TempRoot() {
    if (!path_.empty()) RemoveTree(path_);
  }
  TempRoot(const TempRoot&) = delete;
  TempRoot& operator=(const TempRoot&) = delete;

  const std::string& path() const { return path_; }

 private:
  static void RemoveTree(const std::string& dir) {
    std::vector<std::string> names;
    if (chaski::fsutil::ListNames(dir, names)) {
      for (const std::string& n : names) {
        const std::string child = chaski::fsutil::Join(dir, n);
        RemoveTree(child);
        ::unlink(child.c_str());
      }
    }
    ::rmdir(dir.c_str());
  }

  std::string path_;
};

chaski::wire::Letter MakeLetter(const std::string& id, std::int64_t date) {
  chaski::wire::Letter l;
  l.id = id;
  l.contact_id = "c_rosa";
  l.subject = "hello";
  l.date = date;
  l.body = "line one\nline two\\ with a backslash";
  return l;
}

// The bounded window is a decision, not a limit of the flash: the mailbox is
// canonical and the device is a derived view (design Principle 5, B.8).
TEST(Store, SpecConstantsMatchTheSpec) {
  EXPECT_EQ(chaski::store::kDefaultLettersKeep, 200u);  // §4.1, matches resync_window
  EXPECT_GE(chaski::store::kMinSeenIds, 1000u);         // server §4.5 wire contract
  EXPECT_EQ(chaski::store::kOutboxCap, 12u);            // B.9
}

TEST(Store, PutIsIdempotentById) {
  TempRoot root;
  auto s = chaski::store::OpenLetterStore(root.path());
  ASSERT_TRUE(s->Put(MakeLetter("l-aaaa000001", 100)));
  ASSERT_TRUE(s->Put(MakeLetter("l-aaaa000001", 100)));
  EXPECT_EQ(s->Count(), 1u);

  StoredLetter got;
  ASSERT_TRUE(s->Get("l-aaaa000001", got));
  // The body round-trips byte for byte, newlines and backslashes included: the
  // record escaping is the only thing between a letter and a corrupt one.
  EXPECT_EQ(got.letter.body, MakeLetter("l-aaaa000001", 100).body);
  EXPECT_EQ(got.letter.subject, "hello");
}

// server §4.5: the server MAY re-send any letter at any time. A re-send must
// not undo the fact that the child already read it (client §1.1).
TEST(Store, PutDoesNotResurrectTheUnreadFlag) {
  TempRoot root;
  auto s = chaski::store::OpenLetterStore(root.path());
  ASSERT_TRUE(s->Put(MakeLetter("l-aaaa000001", 100)));
  ASSERT_TRUE(s->MarkRead("l-aaaa000001"));
  EXPECT_EQ(s->UnreadCount(), 0u);

  ASSERT_TRUE(s->Put(MakeLetter("l-aaaa000001", 100)));
  EXPECT_EQ(s->UnreadCount(), 0u);

  s.reset();
  auto reopened = chaski::store::OpenLetterStore(root.path());
  EXPECT_EQ(reopened->UnreadCount(), 0u);
}

TEST(Store, ListNewestFirstIsNewestFirstAndBounded) {
  TempRoot root;
  auto s = chaski::store::OpenLetterStore(root.path());
  ASSERT_TRUE(s->Put(MakeLetter("l-aaaa000001", 100)));
  ASSERT_TRUE(s->Put(MakeLetter("l-aaaa000002", 300)));
  ASSERT_TRUE(s->Put(MakeLetter("l-aaaa000003", 200)));

  const std::vector<StoredLetter> all = s->ListNewestFirst(10);
  ASSERT_EQ(all.size(), 3u);
  EXPECT_EQ(all[0].letter.id, "l-aaaa000002");
  EXPECT_EQ(all[1].letter.id, "l-aaaa000003");
  EXPECT_EQ(all[2].letter.id, "l-aaaa000001");
  EXPECT_EQ(s->ListNewestFirst(2).size(), 2u);
}

// A letter id is server-supplied and becomes a path component, so it is
// untrusted input. A traversal attempt is refused, not quietly sanitised.
TEST(Store, RefusesLetterIdsThatAreNotSafePathComponents) {
  TempRoot root;
  auto s = chaski::store::OpenLetterStore(root.path());
  EXPECT_FALSE(s->Put(MakeLetter("../../etc/passwd", 1)));
  EXPECT_FALSE(s->Put(MakeLetter("", 1)));
  EXPECT_FALSE(s->Put(MakeLetter(".hidden", 1)));
  EXPECT_EQ(s->Count(), 0u);
}

// C-2's host half: eviction removes the letter and keeps the ring entry, so a
// later re-delivery is dropped rather than resurrecting it (§4.1).
TEST(C2, EvictionDoesNotResurrectAnEvictedLetter) {
  TempRoot root;
  auto letters = chaski::store::OpenLetterStore(root.path());
  auto seen = chaski::store::OpenSeenRing(root.path(), chaski::store::kMinSeenIds);

  for (int i = 0; i < 5; ++i) {
    const std::string id = "l-0000000" + std::to_string(i);
    ASSERT_TRUE(letters->Put(MakeLetter(id, 100 + i)));
    ASSERT_TRUE(seen->Add(id));
  }
  EXPECT_EQ(letters->EvictBeyond(3), 2u);
  EXPECT_EQ(letters->Count(), 3u);

  StoredLetter gone;
  EXPECT_FALSE(letters->Get("l-00000000", gone));
  // The ring is untouched, so the sync engine's step-4 check drops the repeat
  // before Put is ever reached (§5.2).
  EXPECT_TRUE(seen->Contains("l-00000000"));

  letters.reset();
  seen.reset();
  auto reopened = chaski::store::OpenSeenRing(root.path(), chaski::store::kMinSeenIds);
  EXPECT_TRUE(reopened->Contains("l-00000000"));
}

TEST(C2, SeenRingHoldsAtLeastAThousandIdsAcrossRestart) {
  TempRoot root;
  auto seen = chaski::store::OpenSeenRing(root.path(), 16);
  // The wire contract states a floor: a smaller request is raised to it, never
  // honoured (server §4.5).
  EXPECT_EQ(seen->Capacity(), chaski::store::kMinSeenIds);

  for (std::size_t i = 0; i < chaski::store::kMinSeenIds; ++i) {
    ASSERT_TRUE(seen->Add("l-" + std::to_string(i)));
  }
  seen.reset();

  auto reopened = chaski::store::OpenSeenRing(root.path(), chaski::store::kMinSeenIds);
  EXPECT_TRUE(reopened->Contains("l-0"));
  EXPECT_TRUE(reopened->Contains("l-999"));
  EXPECT_FALSE(reopened->Contains("l-1000"));
}

TEST(C2, SeenRingDropsTheOldestOnceFull) {
  TempRoot root;
  auto seen = chaski::store::OpenSeenRing(root.path(), chaski::store::kMinSeenIds);
  for (std::size_t i = 0; i < chaski::store::kMinSeenIds + 10; ++i) {
    ASSERT_TRUE(seen->Add("l-" + std::to_string(i)));
  }
  EXPECT_FALSE(seen->Contains("l-0"));
  EXPECT_TRUE(seen->Contains("l-1009"));

  seen.reset();
  auto reopened = chaski::store::OpenSeenRing(root.path(), chaski::store::kMinSeenIds);
  EXPECT_FALSE(reopened->Contains("l-0"));
  EXPECT_TRUE(reopened->Contains("l-1009"));
}

// A power cut during an append can only tear the line being written. The ids
// already there survive; the torn tail is discarded rather than half-read.
TEST(Store, SeenRingSurvivesATornAppend) {
  TempRoot root;
  auto seen = chaski::store::OpenSeenRing(root.path(), chaski::store::kMinSeenIds);
  ASSERT_TRUE(seen->Add("l-aaaa000001"));
  ASSERT_TRUE(seen->Add("l-aaaa000002"));
  seen.reset();

  const std::string path = chaski::fsutil::Join(root.path(), "seen");
  const int fd = ::open(path.c_str(), O_WRONLY | O_APPEND);
  ASSERT_GE(fd, 0);
  ASSERT_GT(::write(fd, "l-aaaa0", 7), 0);  // the cut lands mid-id
  ::close(fd);

  auto reopened = chaski::store::OpenSeenRing(root.path(), chaski::store::kMinSeenIds);
  EXPECT_TRUE(reopened->Contains("l-aaaa000001"));
  EXPECT_TRUE(reopened->Contains("l-aaaa000002"));
  EXPECT_FALSE(reopened->Contains("l-aaaa0"));
}

// C-5: local ids are strictly monotonic and never reused, across reboot and
// power loss alike. Reuse would alias two letters in the server's ack ring.
TEST(C5, LocalIdsAreMonotonicAcrossRestart) {
  TempRoot root;
  std::string first;
  std::string second;
  {
    auto outbox = chaski::store::OpenOutbox(root.path());
    ASSERT_TRUE(outbox->Add("c_rosa", "s", "body one", 10, first));
    ASSERT_TRUE(outbox->Add("c_rosa", "s", "body two", 11, second));
  }
  EXPECT_LT(first, second);

  std::string third;
  {
    auto outbox = chaski::store::OpenOutbox(root.path());
    ASSERT_TRUE(outbox->Add("c_rosa", "s", "body three", 12, third));
  }
  EXPECT_LT(second, third);
  EXPECT_LE(third.size(), static_cast<std::size_t>(chaski::wire::kMaxLocalIdBytes));
}

// The entries leave the outbox on an ack, but the counter never rewinds: a
// restart after the outbox has emptied must not hand out an id twice.
TEST(C5, LocalIdsAreNeverReusedAfterEntriesLeaveTheOutbox) {
  TempRoot root;
  std::string burned;
  {
    auto outbox = chaski::store::OpenOutbox(root.path());
    ASSERT_TRUE(outbox->Add("c_rosa", "s", "body", 10, burned));
    ASSERT_TRUE(outbox->Resolve(burned, chaski::wire::AckStatus::kSent));
    EXPECT_TRUE(outbox->All().empty());
  }
  {
    auto outbox = chaski::store::OpenOutbox(root.path());
    std::string next;
    ASSERT_TRUE(outbox->Add("c_rosa", "s", "body", 11, next));
    EXPECT_GT(next, burned);
  }
}

// The high-water mark has exactly one writer (the Outbox) and is visible
// through the state store, which is where §5.1 reads it from.
TEST(C5, TheHighWaterMarkIsVisibleToTheStateStore) {
  TempRoot root;
  auto outbox = chaski::store::OpenOutbox(root.path());
  auto state = chaski::store::OpenStateStore(root.path());
  EXPECT_EQ(state->Snapshot().local_id_high_water, 0u);

  std::string id;
  ASSERT_TRUE(outbox->Add("c_rosa", "s", "body", 10, id));
  EXPECT_EQ(state->Snapshot().local_id_high_water, 1u);
}

// C-3's store half: every ack status is terminal. `sent` removes the entry; a
// reject keeps the child's text so one key can reopen it (D-5, §5.4).
TEST(C3, SentRemovesAndRejectsPreserveTheText) {
  TempRoot root;
  auto outbox = chaski::store::OpenOutbox(root.path());
  std::string sent;
  std::string rejected;
  ASSERT_TRUE(outbox->Add("c_rosa", "s", "the sent one", 10, sent));
  ASSERT_TRUE(outbox->Add("c_gone", "s", "the rejected one", 11, rejected));

  ASSERT_TRUE(outbox->Resolve(sent, chaski::wire::AckStatus::kSent));
  ASSERT_TRUE(outbox->Resolve(rejected, chaski::wire::AckStatus::kRejectedInactive));

  const std::vector<OutboxEntry> all = outbox->All();
  ASSERT_EQ(all.size(), 1u);
  EXPECT_EQ(all[0].local_id, rejected);
  EXPECT_EQ(all[0].body, "the rejected one");
  EXPECT_TRUE(all[0].rejected);
  EXPECT_EQ(all[0].reject_status, chaski::wire::AckStatus::kRejectedInactive);
  // A rejected letter is never sent again — it waits for the child.
  EXPECT_TRUE(outbox->Sendable().empty());
  EXPECT_EQ(outbox->SendableCount(), 0u);
}

TEST(C3, RejectionAndItsTextSurviveARestart) {
  TempRoot root;
  std::string id;
  {
    auto outbox = chaski::store::OpenOutbox(root.path());
    ASSERT_TRUE(outbox->Add("c_gone", "subject", "the words", 10, id));
    // kUnknown is terminal too: an ack we cannot interpret still means the
    // letter is finished with the road (wire.h).
    ASSERT_TRUE(outbox->Resolve(id, chaski::wire::AckStatus::kUnknown));
  }
  auto outbox = chaski::store::OpenOutbox(root.path());
  const std::vector<OutboxEntry> all = outbox->All();
  ASSERT_EQ(all.size(), 1u);
  EXPECT_TRUE(all[0].rejected);
  EXPECT_EQ(all[0].body, "the words");
  EXPECT_EQ(all[0].reject_status, chaski::wire::AckStatus::kUnknown);
  EXPECT_TRUE(outbox->Discard(id));
  EXPECT_TRUE(outbox->All().empty());
}

// A replayed ack is the normal consequence of a crash before the cursor write
// (§5.2 step 6) and must not read as an error.
TEST(C3, ResolvingAnUnknownLocalIdIsANoOpSuccess) {
  TempRoot root;
  auto outbox = chaski::store::OpenOutbox(root.path());
  EXPECT_TRUE(outbox->Resolve("0000000000000042", chaski::wire::AckStatus::kSent));
  EXPECT_FALSE(outbox->Discard("0000000000000042"));
}

// B.9: the bag holds twelve. Refusing here is what lets the UI park the words
// as the draft instead of losing them.
TEST(Store, OutboxRefusesBeyondTheCap) {
  TempRoot root;
  auto outbox = chaski::store::OpenOutbox(root.path());
  for (std::size_t i = 0; i < chaski::store::kOutboxCap; ++i) {
    std::string id;
    ASSERT_TRUE(outbox->Add("c_rosa", "s", "body", 10, id));
  }
  std::string overflow;
  EXPECT_FALSE(outbox->Add("c_rosa", "s", "body", 10, overflow));
  EXPECT_TRUE(overflow.empty());
  EXPECT_EQ(outbox->SendableCount(), chaski::store::kOutboxCap);
}

TEST(Store, OutboxEntriesAreOldestFirst) {
  TempRoot root;
  auto outbox = chaski::store::OpenOutbox(root.path());
  std::string a;
  std::string b;
  std::string c;
  ASSERT_TRUE(outbox->Add("c_rosa", "s", "one", 10, a));
  ASSERT_TRUE(outbox->Add("c_rosa", "s", "two", 11, b));
  ASSERT_TRUE(outbox->Add("c_rosa", "s", "three", 12, c));
  const std::vector<OutboxEntry> all = outbox->All();
  ASSERT_EQ(all.size(), 3u);
  EXPECT_EQ(all[0].local_id, a);
  EXPECT_EQ(all[1].local_id, b);
  EXPECT_EQ(all[2].local_id, c);
}

// B.12: the cursor is durable in flash, not RTC-only, so a flat battery does
// not cost a 200-letter window resync.
TEST(Store, StateSurvivesARestart) {
  TempRoot root;
  {
    auto state = chaski::store::OpenStateStore(root.path());
    ASSERT_TRUE(state->SetAylluVersion(7));
    ASSERT_TRUE(state->SetLastSyncAt(1700000000));
    ASSERT_TRUE(state->SetCursor("dWlkPTQy"));
  }
  auto state = chaski::store::OpenStateStore(root.path());
  const chaski::store::SyncState s = state->Snapshot();
  EXPECT_EQ(s.cursor, "dWlkPTQy");
  EXPECT_EQ(s.ayllu_version, 7);
  EXPECT_EQ(s.last_sync_at, 1700000000);
}

// The cursor is opaque and stored verbatim (server §4.4): whatever bytes the
// server minted come back unchanged, however unlike a cursor they look.
TEST(Store, TheCursorIsStoredVerbatim) {
  TempRoot root;
  const std::string awkward = "cursor=with\\an=sign\nand a newline";
  {
    auto state = chaski::store::OpenStateStore(root.path());
    ASSERT_TRUE(state->SetCursor(awkward));
  }
  auto state = chaski::store::OpenStateStore(root.path());
  EXPECT_EQ(state->Snapshot().cursor, awkward);
}

// The atomic-write contract: a temp file left behind by a power cut neither
// replaces the previous version nor is mistaken for it.
TEST(Store, AbandonedTempFileDoesNotDisturbThePreviousVersion) {
  TempRoot root;
  {
    auto state = chaski::store::OpenStateStore(root.path());
    ASSERT_TRUE(state->SetCursor("good-cursor"));
  }
  // Exactly what a crash between create and rename leaves on the filesystem.
  ASSERT_TRUE(chaski::fsutil::WriteAtomic(
      chaski::fsutil::Join(root.path(), ".tmp.state"), "cursor=half-writ"));

  auto state = chaski::store::OpenStateStore(root.path());
  EXPECT_EQ(state->Snapshot().cursor, "good-cursor");
}

TEST(Store, AbandonedTempFileIsNotMistakenForALetter) {
  TempRoot root;
  {
    auto letters = chaski::store::OpenLetterStore(root.path());
    ASSERT_TRUE(letters->Put(MakeLetter("l-aaaa000001", 100)));
  }
  const std::string letters_dir = chaski::fsutil::Join(root.path(), "letters");
  ASSERT_TRUE(chaski::fsutil::WriteAtomic(
      chaski::fsutil::Join(letters_dir, ".tmp.l-aaaa000002"), "id=l-aaaa000002\n"));

  auto letters = chaski::store::OpenLetterStore(root.path());
  EXPECT_EQ(letters->Count(), 1u);
  StoredLetter got;
  EXPECT_TRUE(letters->Get("l-aaaa000001", got));
  EXPECT_FALSE(letters->Get("l-aaaa000002", got));
}

// A letter file corrupted beyond parsing is skipped, never fatal: the server
// re-delivers anything the device does not have (server §4.5).
TEST(Store, ACorruptLetterFileDoesNotTakeTheStoreDown) {
  TempRoot root;
  {
    auto letters = chaski::store::OpenLetterStore(root.path());
    ASSERT_TRUE(letters->Put(MakeLetter("l-aaaa000001", 100)));
    ASSERT_TRUE(letters->Put(MakeLetter("l-aaaa000002", 200)));
  }
  ASSERT_TRUE(chaski::fsutil::WriteAtomic(
      chaski::fsutil::Join(chaski::fsutil::Join(root.path(), "letters"), "l-aaaa000002"),
      "\x01\x02 not a record at all"));

  auto letters = chaski::store::OpenLetterStore(root.path());
  EXPECT_EQ(letters->Count(), 1u);
  StoredLetter got;
  EXPECT_TRUE(letters->Get("l-aaaa000001", got));
}

TEST(Store, EvictBeyondZeroUsesTheDefaultRatherThanEmptyingTheStore) {
  TempRoot root;
  auto letters = chaski::store::OpenLetterStore(root.path());
  ASSERT_TRUE(letters->Put(MakeLetter("l-aaaa000001", 100)));
  EXPECT_EQ(letters->EvictBeyond(0), 0u);
  EXPECT_EQ(letters->Count(), 1u);
}

}  // namespace
