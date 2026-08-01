// Host tests for the settings file (client §4.1, §5.5).
//
// The interesting assertion is F-C10's: an absent config field leaves the
// current value alone. A device that re-derived config from each response
// would reset to the documented defaults the moment the server stopped
// sending a field, and the child would experience that as their letters
// getting shorter for no reason anybody could explain.
#include "chaski/settings.h"

#include <memory>
#include <string>

#include <gtest/gtest.h>

#include "chaski/fsutil.h"
#include "temp_root.h"

namespace {

using chaski::store::OpenSettingsStore;
using chaski::store::Settings;
using chaski_test::TempRoot;

TEST(Settings, DefaultsAreTheServersDocumentedOnesUntilToldOtherwise) {
  TempRoot root;
  auto s = OpenSettingsStore(root.path());
  const Settings got = s->Get();
  EXPECT_EQ(got.max_letter_chars, chaski::wire::kDefaultMaxLetterChars);
  EXPECT_EQ(got.sync_interval_s, chaski::wire::kDefaultSyncIntervalS);
  EXPECT_EQ(got.font_step, 0);
  EXPECT_EQ(got.frontlight_step, 0);  // default off (client §2)
  EXPECT_FALSE(got.pin_enabled);
  EXPECT_TRUE(got.rat.empty());
}

TEST(Settings, RoundTripsAcrossPowerLoss) {
  TempRoot root;
  {
    auto s = OpenSettingsStore(root.path());
    ASSERT_TRUE(s->SetFontStep(1));
    ASSERT_TRUE(s->SetFrontlightStep(3));
    ASSERT_TRUE(s->SetPinEnabled(true));
    chaski::wire::DeviceConfig c;
    c.max_letter_chars = 800;
    c.sync_interval_s = 900;
    c.rat = "nbiot";
    c.cover = "road";
    ASSERT_TRUE(s->ApplyConfig(c));
  }

  auto s = OpenSettingsStore(root.path());
  const Settings got = s->Get();
  EXPECT_EQ(got.font_step, 1);
  EXPECT_EQ(got.frontlight_step, 3);
  EXPECT_TRUE(got.pin_enabled);
  EXPECT_EQ(got.max_letter_chars, 800);
  EXPECT_EQ(got.sync_interval_s, 900);
  EXPECT_EQ(got.rat, "nbiot");
  EXPECT_EQ(got.cover, "road");
}

// F-C10, the whole reason this file exists.
TEST(Settings, AbsentConfigFieldsLeaveCurrentValuesAlone) {
  TempRoot root;
  auto s = OpenSettingsStore(root.path());
  chaski::wire::DeviceConfig first;
  first.max_letter_chars = 800;
  first.sync_interval_s = 900;
  first.rat = "ltem";
  first.cover = "wordmark";
  ASSERT_TRUE(s->ApplyConfig(first));

  chaski::wire::DeviceConfig only_rat;  // everything else absent
  only_rat.rat = "nbiot";
  ASSERT_TRUE(s->ApplyConfig(only_rat));

  const Settings got = s->Get();
  EXPECT_EQ(got.rat, "nbiot");
  EXPECT_EQ(got.max_letter_chars, 800);
  EXPECT_EQ(got.sync_interval_s, 900);
  EXPECT_EQ(got.cover, "wordmark");
}

// An empty config block is the steady state: the server sends `config` only
// when it has something to say, and a block with nothing set must be a no-op.
TEST(Settings, EmptyConfigBlockChangesNothing) {
  TempRoot root;
  auto s = OpenSettingsStore(root.path());
  ASSERT_TRUE(s->SetFontStep(1));
  ASSERT_TRUE(s->ApplyConfig(chaski::wire::DeviceConfig{}));
  const Settings got = s->Get();
  EXPECT_EQ(got.font_step, 1);
  EXPECT_EQ(got.max_letter_chars, chaski::wire::kDefaultMaxLetterChars);
}

// The PIN is guardian-pushed per §11.5, but the wire has no field for it
// (server §4.3, §13). Until it does, applying a config block must not disturb
// the local PIN state — least of all unlock a device by omission.
TEST(Settings, ApplyConfigDoesNotTouchPinState) {
  TempRoot root;
  auto s = OpenSettingsStore(root.path());
  ASSERT_TRUE(s->SetPinEnabled(true));
  chaski::wire::DeviceConfig c;
  c.max_letter_chars = 300;
  ASSERT_TRUE(s->ApplyConfig(c));
  EXPECT_TRUE(s->Get().pin_enabled);
}

// A corrupt file must not brick the device: settings fall back to defaults and
// the next write repairs the file.
TEST(Settings, CorruptFileFallsBackToDefaultsAndRepairs) {
  TempRoot root;
  const std::string path = chaski::fsutil::Join(root.path(), "settings");
  ASSERT_TRUE(chaski::fsutil::WriteAtomic(path, std::string("\xff\xfe garbage", 12)));

  {
    auto s = OpenSettingsStore(root.path());
    const Settings got = s->Get();
    EXPECT_EQ(got.max_letter_chars, chaski::wire::kDefaultMaxLetterChars);
    EXPECT_EQ(got.font_step, 0);
    ASSERT_TRUE(s->SetFontStep(1));
  }

  auto s = OpenSettingsStore(root.path());
  EXPECT_EQ(s->Get().font_step, 1);
}

// A half-readable file keeps what it can. Every key is independent, so one
// mangled line costs one setting, not all of them.
TEST(Settings, PartiallyReadableFileKeepsWhatItCan) {
  TempRoot root;
  const std::string path = chaski::fsutil::Join(root.path(), "settings");
  ASSERT_TRUE(chaski::fsutil::WriteAtomic(
      path, "font_step=1\nmax_letter_chars=notanumber\nrat=ltem\n"));

  auto s = OpenSettingsStore(root.path());
  const Settings got = s->Get();
  EXPECT_EQ(got.font_step, 1);
  EXPECT_EQ(got.rat, "ltem");
  EXPECT_EQ(got.max_letter_chars, chaski::wire::kDefaultMaxLetterChars);
}

// The server owns these numbers, but a typo in wasi.toml must not be able to
// make a device unusable or flat: a few-second sync interval keeps the radio
// up permanently, and a zero character cap means the child cannot write.
TEST(Settings, UnusablePushedValuesAreBounded) {
  TempRoot root;
  auto s = OpenSettingsStore(root.path());
  chaski::wire::DeviceConfig c;
  c.sync_interval_s = 5;
  c.max_letter_chars = 0;
  ASSERT_TRUE(s->ApplyConfig(c));
  const Settings got = s->Get();
  EXPECT_EQ(got.sync_interval_s, chaski::store::kMinSyncIntervalS);
  EXPECT_EQ(got.max_letter_chars, chaski::wire::kDefaultMaxLetterChars);
}

TEST(Settings, FontStepIsClampedToTheAvailableFaces) {
  TempRoot root;
  auto s = OpenSettingsStore(root.path());
  ASSERT_TRUE(s->SetFontStep(99));
  EXPECT_EQ(s->Get().font_step, chaski::store::kFontStepCount - 1);
  ASSERT_TRUE(s->SetFontStep(-1));
  EXPECT_EQ(s->Get().font_step, 0);
}

}  // namespace
