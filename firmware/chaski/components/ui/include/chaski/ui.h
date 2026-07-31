// Package ui owns the screens and the words on them.
//
// Register: game mail, not email (design §4). A letter is WAITING (in the
// outbox), ON THE ROAD (with the runner), or ARRIVED. "Queued — no connection"
// reads as broken; "on the road" reads as how the world works, and that is the
// difference between a device a child trusts and one they stop opening.
//
// Every string rendered here comes from strings.c. Nothing in this component
// may contain a user-visible literal, and none of pututu/ayllu/kipu may appear
// in any string it renders (client §0, C-15).
#pragma once

#include <memory>
#include <string>

namespace chaski::ui {

enum class Screen {
  kCover,        // what sleep leaves behind (client §9)
  kPin,          // only when a guardian pushed one (client §11.5, B.4)
  kInbox,
  kRead,
  kComposePick,  // pick a person FIRST — never a To: field (design §4)
  kComposeWrite,
  kOutbox,
  kSettings,
  kFault,        // the §11.6 states, each naming an action a child can take
};

// Fault mirrors syncengine::Fault at the presentation layer. Each has its own
// visible state on purpose: a silently dead device is the failure the design
// spec warns about in hardware form (D-6, client §11.6).
enum class FaultKind {
  kNone,
  kCantReachHome,
  kAskYourGuardians,
  kRoadBusy,
  kChargeMe,
};

class App {
 public:
  virtual ~App() = default;
  virtual void Start(Screen initial) = 0;
  virtual void Tick() = 0;
  virtual Screen Current() const = 0;
  virtual void ShowFault(FaultKind k) = 0;
};

}  // namespace chaski::ui
