// Implementation of the settings file — see include/chaski/settings.h.
//
// One file, `settings`, replaced whole on every change. Changes are rare —
// a font-size press, a frontlight step, a config push — so the simplest
// durable shape is the right one (client §4).
//
// A settings file that will not decode falls back to the defaults instead of
// refusing to open. Losing a font preference is a nuisance; a device that
// cannot reach its settings because one byte flipped is a brick in a bag.
#include "chaski/settings.h"

#include <algorithm>
#include <utility>

#include "chaski/fsutil.h"

namespace chaski::store {
namespace {

using fsutil::Record;

constexpr char kSettingsFile[] = "settings";

std::string Encode(const Settings& s) {
  const Record r = {
      {"font_step", std::to_string(s.font_step)},
      {"frontlight_step", std::to_string(s.frontlight_step)},
      {"max_letter_chars", std::to_string(s.max_letter_chars)},
      {"sync_interval_s", std::to_string(s.sync_interval_s)},
      {"rat", s.rat},
      {"cover", s.cover},
      {"pin_enabled", s.pin_enabled ? "1" : "0"},
  };
  return fsutil::EncodeRecord(r);
}

// Decode is per-field tolerant: a missing or malformed key keeps the default
// that is already in `s`, so a partially readable file still yields a usable
// device.
void Decode(const std::string& text, Settings& s) {
  const Record r = fsutil::DecodeRecord(text);
  fsutil::ReadInt(r, "font_step", s.font_step);
  fsutil::ReadInt(r, "frontlight_step", s.frontlight_step);
  fsutil::ReadInt(r, "max_letter_chars", s.max_letter_chars);
  fsutil::ReadInt(r, "sync_interval_s", s.sync_interval_s);
  if (const std::string* v = fsutil::Find(r, "rat")) s.rat = *v;
  if (const std::string* v = fsutil::Find(r, "cover")) s.cover = *v;
  fsutil::ReadBool(r, "pin_enabled", s.pin_enabled);
}

class FileSettingsStore final : public SettingsStore {
 public:
  explicit FileSettingsStore(std::string path) : path_(std::move(path)) {
    std::string text;
    if (fsutil::ReadAll(path_, text)) Decode(text, s_);
    Sanitise(s_);
  }

  Settings Get() const override { return s_; }

  bool SetFontStep(int step) override {
    Settings next = s_;
    next.font_step = step;
    return Commit(next);
  }

  bool SetFrontlightStep(int step) override {
    Settings next = s_;
    next.frontlight_step = step;
    return Commit(next);
  }

  bool SetPinEnabled(bool enabled) override {
    Settings next = s_;
    next.pin_enabled = enabled;
    return Commit(next);
  }

  // §5.5, F-C10: field by field, and absent means untouched. `max_letter_chars`
  // is stored now and takes effect for the NEXT composition — the UI reads it
  // when the child starts writing, so a push mid-letter cannot shorten the
  // letter under them.
  bool ApplyConfig(const wire::DeviceConfig& c) override {
    Settings next = s_;
    if (c.max_letter_chars) next.max_letter_chars = *c.max_letter_chars;
    if (c.sync_interval_s) next.sync_interval_s = *c.sync_interval_s;
    if (c.rat) next.rat = *c.rat;
    if (c.cover) next.cover = *c.cover;
    return Commit(next);
  }

 private:
  // Sanitise bounds what the device will act on. The server owns these numbers
  // and the device trusts them (server §13); what it will not do is act on a
  // value that makes it unusable — a pushed sync interval of a few seconds
  // keeps the radio up permanently and flattens the pack in an afternoon, and
  // a non-positive character cap means the child cannot write at all.
  static void Sanitise(Settings& s) {
    if (s.max_letter_chars <= 0) s.max_letter_chars = wire::kDefaultMaxLetterChars;
    if (s.sync_interval_s < kMinSyncIntervalS) s.sync_interval_s = kMinSyncIntervalS;
    s.font_step = std::min(std::max(s.font_step, 0), kFontStepCount - 1);
    if (s.frontlight_step < 0) s.frontlight_step = 0;
  }

  bool Commit(Settings next) {
    Sanitise(next);
    if (!fsutil::WriteAtomic(path_, Encode(next))) return false;
    s_ = std::move(next);
    return true;
  }

  std::string path_;
  Settings s_;
};

}  // namespace

std::unique_ptr<SettingsStore> OpenSettingsStore(const std::string& root) {
  fsutil::MkdirAll(root);
  return std::unique_ptr<SettingsStore>(
      new FileSettingsStore(fsutil::Join(root, kSettingsFile)));
}

}  // namespace chaski::store
