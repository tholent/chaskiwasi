// A whole device's UI, wired to real stores in a temp directory and to
// recording doubles for the two things a host has none of: a panel and glyphs.
//
// The doubles record a trace in the same style as the wipe tests
// (../panel/recording_panel.h), because the same argument applies here: what
// can be wrong about a screen is WHAT IT SHOWED and WHEN IT REFRESHED, and
// neither is visible from a framebuffer full of zeroes on a machine with no
// font. Recording the text and marks each screen asked for is therefore a
// stronger statement about content leakage than comparing pixels would be.
#pragma once

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "../draft/temp_root.h"
#include "../panel/recording_panel.h"
#include "chaski/ui.h"
#include "chaski_strings.h"

namespace chaski_test {

using chaski::testing::Trace;

class RecordingPainter final : public chaski::ui::Painter {
 public:
  explicit RecordingPainter(Trace* t) : t_(t) {}

  void Clear(chaski::panel::Framebuf& fb, int w, int h) override {
    fb.w = w;
    fb.h = h;
    fb.bits.assign(static_cast<std::size_t>((w + 7) / 8) * static_cast<std::size_t>(h), 0);
    // Each frame starts here, so `text` and `glyphs` describe the CURRENT
    // screen while the trace keeps the whole session.
    text.clear();
    glyphs.clear();
    t_->Add("clear");
  }

  void DrawText(chaski::panel::Framebuf&, int, int, const std::string& utf8) override {
    text.push_back(utf8);
    t_->Add("text:" + utf8);
  }

  void DrawGlyph(chaski::panel::Framebuf&, int, int, chaski::ui::Glyph g) override {
    glyphs.push_back(g);
    t_->Add("glyph:" + std::to_string(static_cast<int>(g)));
  }

  bool Drew(const std::string& s) const {
    for (const std::string& t : text) {
      if (t.find(s) != std::string::npos) return true;
    }
    return false;
  }

  std::vector<std::string> text;
  std::vector<chaski::ui::Glyph> glyphs;

 private:
  Trace* t_;
};

// Env owns one device: real stores over a temp directory, recording panel and
// painter, and a clock the test moves by hand.
class Env {
 public:
  Env() {
    letters = chaski::store::OpenLetterStore(root.path());
    outbox = chaski::store::OpenOutbox(root.path());
    drafts = chaski::store::OpenDraftStore(root.path());
    settings = chaski::store::OpenSettingsStore(root.path());
    contacts = chaski::ayllu::Open(root.path());
    health = chaski::kipu::NewLog();
  }

  // Build is separate from the constructor so a test can seed stores and
  // settings first: the app reads a pushed PIN and a pending draft at Start.
  void Build() {
    chaski::ui::Deps d;
    d.letters = letters.get();
    d.outbox = outbox.get();
    d.drafts = drafts.get();
    d.settings = settings.get();
    d.contacts = contacts.get();
    d.health_log = health.get();
    d.panel = &panel;
    d.painter = &painter;
    d.text = &chaski_text;
    d.now_epoch = [this] { return epoch; };
    d.clock_valid = [this] { return clock_valid; };
    d.monotonic_ms = [this] { return now_ms; };
    d.verify_pin = [this](const std::string& entry) {
      ++pin_checks;
      return entry == pin;
    };
    d.request_sync = [this] { ++sync_calls; };
    d.about = [this] {
      chaski::ui::About a;
      a.firmware_version = fw;
      a.battery_pct = battery_pct;
      a.charging = charging;
      a.rssi = rssi;
      return a;
    };
    app = chaski::ui::New(d);
  }

  void Press(chaski::input::Key k) {
    chaski::input::KeyEvent e;
    e.key = k;
    app->Key(e);
  }

  void TypeCp(unsigned int cp) {
    chaski::input::KeyEvent e;
    e.key = chaski::input::Key::kChar;
    e.codepoint = cp;
    app->Key(e);
  }

  void Type(const std::string& ascii) {
    for (char c : ascii) TypeCp(static_cast<unsigned char>(c));
  }

  void Advance(std::int64_t ms) {
    now_ms += ms;
    app->Tick();
  }

  const chaski::ui::View& View() const { return app->CurrentView(); }

  bool RowSays(const std::string& needle) const {
    for (const chaski::ui::Row& r : View().rows) {
      if (r.primary.find(needle) != std::string::npos) return true;
      if (r.secondary.find(needle) != std::string::npos) return true;
      if (r.meta.find(needle) != std::string::npos) return true;
    }
    return false;
  }

  bool LineSays(const std::string& needle) const {
    for (const std::string& l : View().lines) {
      if (l.find(needle) != std::string::npos) return true;
    }
    return false;
  }

  static std::string Joined(const std::vector<std::string>& lines) {
    std::string out;
    for (const std::string& l : lines) {
      if (!out.empty()) out += " ";
      out += l;
    }
    return out;
  }

  TempRoot root;
  std::unique_ptr<chaski::store::LetterStore> letters;
  std::unique_ptr<chaski::store::Outbox> outbox;
  std::unique_ptr<chaski::store::DraftStore> drafts;
  std::unique_ptr<chaski::store::SettingsStore> settings;
  std::unique_ptr<chaski::ayllu::Store> contacts;
  std::unique_ptr<chaski::kipu::Log> health;

  Trace trace;
  chaski::testing::RecordingPanel panel{&trace};
  RecordingPainter painter{&trace};

  std::int64_t now_ms = 1000;
  std::int64_t epoch = 1700000000;  // Tue 14 Nov 2023 22:13:20 UTC
  bool clock_valid = true;
  std::string pin = "1234";
  int pin_checks = 0;
  int sync_calls = 0;
  int battery_pct = 64;
  bool charging = false;
  int rssi = -70;
  std::string fw = "0.1.0";

  std::unique_ptr<chaski::ui::App> app;
};

// The contact list every test starts from: two people to write to, one who
// left, and the system sender. The last two exist to keep C-13 honest — they
// must be nameable on old letters and absent from the picker.
inline chaski::wire::Ayllu SampleAyllu() {
  chaski::wire::Ayllu a;
  a.version = 7;
  chaski::wire::AylluContact rosa;
  rosa.id = "c_rosa";
  rosa.name = "Rosa";
  rosa.active = true;
  chaski::wire::AylluContact dad;
  dad.id = "c_dad";
  dad.name = "Dad";
  dad.active = true;
  dad.pinned = true;
  chaski::wire::AylluContact gone;
  gone.id = "c_gone";
  gone.name = "Tio Beto";
  gone.active = false;  // a tombstone: readable, not writable (server §7.2)
  chaski::wire::AylluContact sys;
  sys.id = "c_sys";
  sys.name = "Wasi";
  sys.active = false;  // ships as a tombstone on purpose (server A.15)
  a.contacts = {rosa, dad, gone, sys};
  return a;
}

inline chaski::wire::Letter SampleLetter(const std::string& id,
                                         const std::string& contact_id,
                                         const std::string& subject,
                                         const std::string& body,
                                         std::int64_t date) {
  chaski::wire::Letter l;
  l.id = id;
  l.contact_id = contact_id;
  l.subject = subject;
  l.body = body;
  l.date = date;
  return l;
}

}  // namespace chaski_test
