// Test doubles for components/input, recording the ORDER of what happened.
//
// Same shape and same reason as test/firmware/host/panel/recording_panel.h: the
// property under test is a sequence, so the doubles write into one interleaved
// trace rather than each keeping its own tally. "The wipe ran and the letter
// 'a' was delivered" is true of both a correct dispatcher and one that hands
// put-away to a screen first; only the order tells them apart (C-11).
//
// The Trace is duplicated rather than shared with the panel suite on purpose —
// that one is welded to chaski/panel.h, and an input test that had to link the
// panel component to say "putaway before ui:a" would be paying a dependency for
// a struct with four methods.
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

#include "chaski/input.h"
#include "chaski/keebdeck.h"

namespace chaski_test {

struct Trace {
  std::vector<std::string> steps;

  void Add(const std::string& s) { steps.push_back(s); }

  bool Contains(const std::string& s) const {
    for (const auto& e : steps) {
      if (e == s) return true;
    }
    return false;
  }

  // IndexOf returns the position of the first occurrence, or -1, so tests can
  // state "a before b" without over-constraining to adjacency.
  int IndexOf(const std::string& s) const {
    for (std::size_t i = 0; i < steps.size(); ++i) {
      if (steps[i] == s) return static_cast<int>(i);
    }
    return -1;
  }

  int Count(const std::string& s) const {
    int n = 0;
    for (const auto& e : steps) {
      if (e == s) ++n;
    }
    return n;
  }
};

// Label names an event for the trace. Keys get their own token; characters
// carry their codepoint, because "ui:char" would make an ASCII round trip and a
// mangled one look identical.
inline std::string Label(const chaski::input::KeyEvent& e) {
  using chaski::input::Key;
  switch (e.key) {
    case Key::kUp: return "ui:up";
    case Key::kDown: return "ui:down";
    case Key::kLeft: return "ui:left";
    case Key::kRight: return "ui:right";
    case Key::kEnter: return "ui:enter";
    case Key::kBack: return "ui:back";
    case Key::kFrontlight: return "ui:frontlight";
    case Key::kPutAway: return "ui:PUTAWAY";  // must never appear in a trace
    case Key::kSync: return "ui:SYNC";        // likewise
    case Key::kChar: return "ui:char:" + std::to_string(e.codepoint);
    case Key::kNone: break;
  }
  return "ui:none";
}

class FakeClock {
 public:
  std::int64_t Now() const { return now_ms_; }
  void Advance(std::int64_t ms) { now_ms_ += ms; }

 private:
  std::int64_t now_ms_ = 0;
};

// ScriptedSource replays a fixed queue of events, which is what a source looks
// like from the dispatcher's side: one event per Poll, false when drained.
class ScriptedSource final : public chaski::input::Source {
 public:
  void Push(chaski::input::Key k) {
    chaski::input::KeyEvent e;
    e.key = k;
    queue_.push_back(e);
  }

  void PushChar(unsigned int codepoint) {
    chaski::input::KeyEvent e;
    e.key = chaski::input::Key::kChar;
    e.codepoint = codepoint;
    queue_.push_back(e);
  }

  bool Poll(chaski::input::KeyEvent& out) override {
    ++polls;
    if (next_ >= queue_.size()) return false;
    out = queue_[next_++];
    return true;
  }

  int polls = 0;

 private:
  std::vector<chaski::input::KeyEvent> queue_;
  std::size_t next_ = 0;
};

// StuckSource asserts one key forever — a chattering bus, or a cap held down by
// a book on top of the device. It exists to prove Pump still returns.
class StuckSource final : public chaski::input::Source {
 public:
  explicit StuckSource(chaski::input::Key k) : key_(k) {}

  bool Poll(chaski::input::KeyEvent& out) override {
    ++polls;
    out = chaski::input::KeyEvent{};
    out.key = key_;
    return true;
  }

  int polls = 0;

 private:
  chaski::input::Key key_;
};

// HungConsumer is the screen handler C-11 is about: it takes whatever it is
// handed and does nothing with it, ever. In a real device the hang is a task
// that never returns to its loop; a single-threaded test models that by simply
// not pumping again, which is the same observable fact — no further Pump call
// happens. What the dispatcher must guarantee is that neither this object's
// behaviour nor its existence can delay put-away, so tests hand it events and
// then assert against the trace, never against it.
class HungConsumer {
 public:
  void Handle(const chaski::input::KeyEvent& e) {
    ++handled;
    last = e;
    // A wedged screen returns nothing, decides nothing, and acknowledges
    // nothing. Deliberately empty beyond the bookkeeping above.
  }

  int handled = 0;
  chaski::input::KeyEvent last;
};

// FakeKeebDeckBus is the I2C part that does not exist in this environment. It
// replays a scripted sequence of level readings, one per ReadPressed, so a
// bouncing contact is expressible as exactly what the wire would show.
class FakeKeebDeckBus final : public chaski::input::KeebDeckBus {
 public:
  // Script pushes readings; kFail marks a bus error at that step.
  static constexpr int kFail = -1;

  void Push(int reading, int times = 1) {
    for (int i = 0; i < times; ++i) readings_.push_back(reading);
  }

  bool ReadPressed(std::uint8_t& scancode) override {
    ++reads;
    if (next_ >= readings_.size()) {
      // Past the script the key is simply not held. Sources are polled far more
      // often than a test cares to script.
      scancode = chaski::input::kScanNone;
      return true;
    }
    const int r = readings_[next_++];
    if (r == kFail) return false;
    scancode = static_cast<std::uint8_t>(r);
    return true;
  }

  int reads = 0;

 private:
  std::vector<int> readings_;
  std::size_t next_ = 0;
};

}  // namespace chaski_test
