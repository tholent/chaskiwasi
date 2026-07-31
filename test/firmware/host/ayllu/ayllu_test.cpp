// Host tests for components/ayllu: the server's membership snapshot and the
// child's device-local decoration, and the rule that neither can erase the
// other (client §4.4, B.3).
#include "chaski/ayllu.h"

#include <cstdlib>
#include <string>
#include <vector>

#include <unistd.h>

#include <gtest/gtest.h>

#include "chaski/fsutil.h"

namespace {

using chaski::ayllu::Contact;
using chaski::ayllu::Overlay;

class TempRoot {
 public:
  TempRoot() {
    char tmpl[] = "/tmp/chaski-ayllu-XXXXXX";
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

chaski::wire::AylluContact MakeContact(const std::string& id, const std::string& name,
                                       bool active, int order = 0) {
  chaski::wire::AylluContact c;
  c.id = id;
  c.name = name;
  c.active = active;
  c.order = order;
  return c;
}

// A list the way it arrives from the server: two people, one tombstone, and
// the reserved system contact — which ships inactive (server A.15, A.16).
chaski::wire::Ayllu SampleAyllu(int version) {
  chaski::wire::Ayllu a;
  a.version = version;
  a.contacts.push_back(MakeContact("c_rosa", "Rosa", true, 1));
  a.contacts.push_back(MakeContact("c_dad", "Dad", true, 0));
  a.contacts.push_back(MakeContact("c_gone", "Tia", false, 2));
  a.contacts.push_back(MakeContact(chaski::wire::kSysContactId, "Wasi", false, 3));
  return a;
}

const Contact* FindById(const std::vector<Contact>& cs, const std::string& id) {
  for (const Contact& c : cs) {
    if (c.id == id) return &c;
  }
  return nullptr;
}

TEST(Ayllu, OverlayFieldsAreAllOptional) {
  Overlay o;
  // An unset field means "use the server's value", which is what makes a
  // guardian's rename still reach the child (client §4.4, B.3).
  EXPECT_FALSE(o.nickname.has_value());
  EXPECT_FALSE(o.pinned.has_value());
  EXPECT_FALSE(o.order.has_value());
  EXPECT_FALSE(o.portrait.has_value());
}

TEST(Ayllu, SnapshotAndOverlaySurviveARestart) {
  TempRoot root;
  {
    auto store = chaski::ayllu::Open(root.path());
    ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(4)));
    Overlay o;
    o.nickname = "Ro";
    o.pinned = true;
    o.order = 9;
    o.portrait = "fox";
    ASSERT_TRUE(store->SetOverlay("c_rosa", o));
  }
  auto store = chaski::ayllu::Open(root.path());
  EXPECT_EQ(store->Version(), 4);
  Contact rosa;
  ASSERT_TRUE(store->Lookup("c_rosa", rosa));
  EXPECT_EQ(rosa.name, "Ro");
  EXPECT_EQ(rosa.server_name, "Rosa");
  EXPECT_TRUE(rosa.pinned);
  EXPECT_EQ(rosa.order, 9);
  EXPECT_EQ(rosa.portrait, "fox");
}

// B.3: membership and decoration are separate files precisely so a guardian's
// edit cannot silently erase the child's nicknames.
TEST(Ayllu, SnapshotReplacementPreservesTheOverlay) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));
  Overlay o;
  o.nickname = "Ro";
  ASSERT_TRUE(store->SetOverlay("c_rosa", o));

  // The guardian renames Rosa and bumps the version.
  chaski::wire::Ayllu next = SampleAyllu(2);
  next.contacts[0].name = "Rosa Maria";
  ASSERT_TRUE(store->ApplySnapshot(next));

  Contact rosa;
  ASSERT_TRUE(store->Lookup("c_rosa", rosa));
  EXPECT_EQ(rosa.name, "Ro");                  // the child's nickname stands
  EXPECT_EQ(rosa.server_name, "Rosa Maria");   // the guardian's edit still lands
  EXPECT_EQ(store->Version(), 2);
}

// §4.4: server values are guardian-set defaults. Without a nickname, a rename
// reaches the child; that is the whole reason the overlay is per-field.
TEST(Ayllu, AGuardianRenameShowsWhenThereIsNoNickname) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));
  chaski::wire::Ayllu next = SampleAyllu(2);
  next.contacts[0].name = "Rosa Maria";
  ASSERT_TRUE(store->ApplySnapshot(next));

  Contact rosa;
  ASSERT_TRUE(store->Lookup("c_rosa", rosa));
  EXPECT_EQ(rosa.name, "Rosa Maria");
}

// A contact who leaves and returns keeps the original id (server §7.2), so the
// decoration retained through the gap is exactly what the child expects back.
TEST(Ayllu, OverlaySurvivesAContactLeavingAndReturning) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));
  Overlay o;
  o.nickname = "Ro";
  ASSERT_TRUE(store->SetOverlay("c_rosa", o));

  chaski::wire::Ayllu without;
  without.version = 2;
  without.contacts.push_back(MakeContact("c_dad", "Dad", true, 0));
  ASSERT_TRUE(store->ApplySnapshot(without));
  Contact gone;
  EXPECT_FALSE(store->Lookup("c_rosa", gone));

  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(3)));
  Contact rosa;
  ASSERT_TRUE(store->Lookup("c_rosa", rosa));
  EXPECT_EQ(rosa.name, "Ro");
}

// I-4: nothing the child does to decoration may touch membership.
TEST(Ayllu, ClearingTheOverlayDoesNotTouchMembership) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));
  Overlay o;
  o.nickname = "Ro";
  ASSERT_TRUE(store->SetOverlay("c_rosa", o));
  ASSERT_TRUE(store->ClearOverlay("c_rosa"));

  Contact rosa;
  ASSERT_TRUE(store->Lookup("c_rosa", rosa));
  EXPECT_EQ(rosa.name, "Rosa");
  EXPECT_EQ(store->Merged().size(), SampleAyllu(1).contacts.size());
  EXPECT_EQ(store->Version(), 1);
  // Clearing something already clear is success, not an error.
  EXPECT_TRUE(store->ClearOverlay("c_rosa"));
}

// Decoration may legitimately precede the snapshot that reintroduces someone.
TEST(Ayllu, OverlayForAnUnknownIdIsAcceptedAndInvisibleUntilTheyExist) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  Overlay o;
  o.nickname = "Ro";
  ASSERT_TRUE(store->SetOverlay("c_rosa", o));
  EXPECT_TRUE(store->Merged().empty());  // membership is the server's alone

  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));
  Contact rosa;
  ASSERT_TRUE(store->Lookup("c_rosa", rosa));
  EXPECT_EQ(rosa.name, "Ro");
}

// C-13: a tombstone still names the sender of an old letter (server §7.2) and
// never appears in the compose picker (client §11.2).
TEST(C13, TombstonesRenderButAreNotComposable) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));

  const std::vector<Contact> merged = store->Merged();
  const Contact* tia = FindById(merged, "c_gone");
  ASSERT_NE(tia, nullptr);
  EXPECT_EQ(tia->name, "Tia");
  EXPECT_FALSE(tia->active);

  Contact looked_up;
  ASSERT_TRUE(store->Lookup("c_gone", looked_up));
  EXPECT_EQ(looked_up.name, "Tia");

  EXPECT_EQ(FindById(store->Composable(), "c_gone"), nullptr);
}

// C-13: the system contact names the sender of a notice letter and can never
// be written to. It ships as a tombstone so this needs no special case.
TEST(C13, TheSystemContactNamesLettersAndIsNeverComposable) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));

  Contact sys;
  ASSERT_TRUE(store->Lookup(chaski::wire::kSysContactId, sys));
  EXPECT_EQ(sys.name, "Wasi");
  EXPECT_EQ(FindById(store->Composable(), chaski::wire::kSysContactId), nullptr);
}

// Belt and braces: should a server bug ever ship c_sys active, writing to Wasi
// is still not a thing a child can do.
TEST(C13, TheSystemContactIsNotComposableEvenIfItArrivesActive) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  chaski::wire::Ayllu a;
  a.version = 1;
  a.contacts.push_back(MakeContact(chaski::wire::kSysContactId, "Wasi", true));
  ASSERT_TRUE(store->ApplySnapshot(a));
  EXPECT_TRUE(store->Composable().empty());
}

// C-14: an unknown contact id is not an error. The caller keeps the letter and
// renders it under the neutral fallback label.
TEST(C14, AnUnknownContactIdIsNotAnError) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));

  Contact out;
  EXPECT_FALSE(store->Lookup("c_nobody", out));
  // Nothing usable as a name comes back, so a caller that ignores the return
  // value renders a blank label rather than a raw contact id.
  EXPECT_EQ(out.id, "c_nobody");
  EXPECT_TRUE(out.name.empty());
  EXPECT_FALSE(out.active);

  const char* key = chaski::ayllu::FallbackLabelKey();
  ASSERT_NE(key, nullptr);
  EXPECT_STRNE(key, "");
}

TEST(C14, LookupMissAgainstAnEmptyStoreStillAnswers) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  Contact out;
  EXPECT_FALSE(store->Lookup("c_rosa", out));
  EXPECT_EQ(out.id, "c_rosa");
}

// §11.2: pinned first, then the child's order, then name.
TEST(Ayllu, MergedOrderIsPinnedThenOrderThenName) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  chaski::wire::Ayllu a;
  a.version = 1;
  a.contacts.push_back(MakeContact("c_ana", "Ana", true, 5));
  a.contacts.push_back(MakeContact("c_bo", "Bo", true, 5));
  a.contacts.push_back(MakeContact("c_cy", "Cy", true, 1));
  ASSERT_TRUE(store->ApplySnapshot(a));

  Overlay pin;
  pin.pinned = true;
  ASSERT_TRUE(store->SetOverlay("c_bo", pin));

  const std::vector<Contact> ordered = store->Composable();
  ASSERT_EQ(ordered.size(), 3u);
  EXPECT_EQ(ordered[0].id, "c_bo");   // pinned by the child
  EXPECT_EQ(ordered[1].id, "c_cy");   // lowest order
  EXPECT_EQ(ordered[2].id, "c_ana");
}

// The overlay's order wins over the server's, which is what "the child owns
// decoration" means in practice.
TEST(Ayllu, OverlayOrderOverridesTheServerOrder) {
  TempRoot root;
  auto store = chaski::ayllu::Open(root.path());
  ASSERT_TRUE(store->ApplySnapshot(SampleAyllu(1)));
  Overlay first;
  first.order = -1;
  ASSERT_TRUE(store->SetOverlay("c_rosa", first));

  const std::vector<Contact> ordered = store->Composable();
  ASSERT_EQ(ordered.size(), 2u);
  EXPECT_EQ(ordered[0].id, "c_rosa");
  EXPECT_EQ(ordered[1].id, "c_dad");
}

// Names arrive from a mailbox and may carry anything a person's name can carry.
TEST(Ayllu, NamesWithAwkwardBytesRoundTrip) {
  TempRoot root;
  const std::string awkward = "Rosa=\\ Ma\nria";
  {
    auto store = chaski::ayllu::Open(root.path());
    chaski::wire::Ayllu a;
    a.version = 1;
    a.contacts.push_back(MakeContact("c_rosa", awkward, true));
    ASSERT_TRUE(store->ApplySnapshot(a));
  }
  auto store = chaski::ayllu::Open(root.path());
  Contact rosa;
  ASSERT_TRUE(store->Lookup("c_rosa", rosa));
  EXPECT_EQ(rosa.server_name, awkward);
}

}  // namespace
