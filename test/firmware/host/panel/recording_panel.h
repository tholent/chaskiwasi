// RecordingPanel and friends — test doubles that record the ORDER of what the
// wipe controller did.
//
// The wipe is the one behaviour on this device whose correctness is invisible
// from software: nothing can read the glass back, and a wipe that runs its
// steps in the wrong order still returns successfully. So the doubles record a
// single interleaved trace across panel, rail, radio and sleeper, and the tests
// assert against that trace. Recording each object separately would prove every
// step happened and nothing about the sequence, which is the only thing that
// can actually be wrong here (C-10, C-24).
#pragma once

#include <string>
#include <vector>

#include "chaski/panel.h"

namespace chaski::testing {

// Trace is the shared, ordered log. One object, many recorders.
struct Trace {
  std::vector<std::string> steps;

  void Add(const std::string& s) { steps.push_back(s); }

  bool Contains(const std::string& s) const {
    for (const auto& e : steps) {
      if (e == s) return true;
    }
    return false;
  }

  // IndexOf returns the position of the first occurrence, or -1. Tests use it
  // to state orderings as "a before b", which is what the spec actually
  // requires — exact adjacency would over-constrain the implementation.
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

class RecordingPanel final : public panel::Panel {
 public:
  explicit RecordingPanel(Trace* t) : t_(t) {}

  void PartialRefresh(const panel::Rect&, const panel::Framebuf&) override {
    t_->Add("partial");
  }
  void FastRefresh(const panel::Framebuf&) override { t_->Add("fast"); }
  void FullRefresh(const panel::Framebuf&) override { t_->Add("full"); }
  void ClearFlush(int passes) override {
    last_flush_passes = passes;
    t_->Add("flush");
  }
  void WaitBusy() override { t_->Add("busy"); }
  void DeepSleep() override { t_->Add("panel_sleep"); }

  int last_flush_passes = 0;

 private:
  Trace* t_;
};

class RecordingRail final : public panel::Rail {
 public:
  explicit RecordingRail(Trace* t) : t_(t) {}
  void PowerOn() override { t_->Add("rail_on"); }
  void Cut() override { t_->Add("rail_cut"); }

 private:
  Trace* t_;
};

class RecordingRadio final : public panel::RadioOff {
 public:
  explicit RecordingRadio(Trace* t) : t_(t) {}
  void PowerDownRadio() override { t_->Add("radio_off"); }

 private:
  Trace* t_;
};

class RecordingSleeper final : public panel::Sleeper {
 public:
  explicit RecordingSleeper(Trace* t) : t_(t) {}
  void DeepSleep(bool wake_on_charger_only) override {
    charger_only = wake_on_charger_only;
    t_->Add(wake_on_charger_only ? "mcu_sleep_charger_only" : "mcu_sleep");
  }
  bool charger_only = false;

 private:
  Trace* t_;
};

}  // namespace chaski::testing
