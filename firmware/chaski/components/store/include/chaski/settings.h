// Package settings holds the device's own knobs and the last applied server
// config (client §4.1).
//
// Why this is persisted state and not a decoded copy of the last response:
// `wire::DeviceConfig` fields are optional, and an absent field means "leave
// the current value alone" (§5.5, F-C10). A device that re-derived its config
// from each response would silently reset to the documented defaults the first
// time the server stopped sending a field — a configuration change nobody made,
// that no log records, and that the child would experience as their letters
// getting shorter. The current values live here, and the config block edits
// them field by field.
//
// The PIN's *value* is not here: it belongs in encrypted NVS with the other
// secrets (§4.2). Only whether a PIN is required is settings-shaped.
//
// F-C7: §4.1 names `settings` in the storage model but the Wave 0 scaffold
// declared no API for it. This is that API.
#pragma once

#include <memory>
#include <string>

#include "chaski/wire.h"

namespace chaski::store {

struct Settings {
  // Font size is a runtime accessibility setting, which is why layout is
  // device-owned in the first place (§8.2, A.10). Stored as a step rather than
  // a layout::FontSize so that components/store keeps depending on nothing but
  // the wire; the UI maps the step to a face.
  int font_step = 0;

  // Frontlight defaults off (client §2) — it is on a physical button, and a
  // device that lights up by itself in a bag is neither private nor cheap.
  int frontlight_step = 0;

  // Applied server config (§5.5). These are current values, not the server's
  // documented defaults: an absent field never overwrites them (F-C10).
  int max_letter_chars = wire::kDefaultMaxLetterChars;
  int sync_interval_s = wire::kDefaultSyncIntervalS;
  std::string rat;    // last pushed radio access technology (design §6.2)
  std::string cover;  // cover composition option; never adds content (B.5)

  // PIN state only (client §11.5, B.4). Disabled by default; a guardian
  // enables it remotely, and clearing it remotely is the only recovery.
  bool pin_enabled = false;
};

// Bounds the device applies to pushed values. The server owns the numbers
// (server §13) and the device trusts them — these exist so that a typo in
// wasi.toml cannot make a device in a bag unusable or flat, which is a
// different question from whether the value is authorised.
inline constexpr int kMinSyncIntervalS = 60;
inline constexpr int kFontStepCount = 2;  // at least two sizes in v1 (§8.2)

class SettingsStore {
 public:
  virtual ~SettingsStore() = default;

  virtual Settings Get() const = 0;

  virtual bool SetFontStep(int step) = 0;
  virtual bool SetFrontlightStep(int step) = 0;

  // SetPinEnabled exists because the wire does not carry PIN state: §11.5 says
  // a guardian pushes the PIN through the config block, but `config` has no
  // such field (server §4.3, §13). Until it does, this is the seam the PIN
  // flow sets, and ApplyConfig deliberately leaves pin_enabled alone.
  virtual bool SetPinEnabled(bool enabled) = 0;

  // ApplyConfig applies the pushed block field by field. Absent fields leave
  // the current value untouched; unrecognised fields never reach here at all
  // (client §5.5). Returns false only when the write failed.
  virtual bool ApplyConfig(const wire::DeviceConfig& c) = 0;
};

std::unique_ptr<SettingsStore> OpenSettingsStore(const std::string& root);

}  // namespace chaski::store
