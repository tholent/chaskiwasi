// Package syncengine is the device half of the sync protocol (client §5,
// server §4). It assembles a request, hands it to the transport seam, and
// applies the response in an order that is safe to interrupt at any point.
//
// The ordering in ApplyResponse is the whole contract and is NOT an
// implementation detail (client §5.2):
//
//   1. apply server_time to the clock
//   2. process acks (durable) — every ack is terminal
//   3. apply the ayllu block (durable) — BEFORE letters, so names resolve
//   4. per letter: seen-ring check -> write letter -> add id to ring
//   5. apply the config block
//   6. write the cursor (durable) — LAST
//
// A crash before step 6 leaves the old cursor standing: the server re-delivers
// and the seen-ring absorbs the repeats. A crash inside step 4 costs at most a
// repeat of one letter. There is no step whose interruption loses a letter or
// double-sends one — which is D-5 stated as an algorithm.
//
// Portability: no esp_* headers (implementation-plan rule 3). Time and
// randomness arrive through seams so the whole engine is host-testable (C-3,
// C-4, C-6, C-21).
#pragma once

#include <cstdint>
#include <functional>
#include <memory>
#include <string>

#include "chaski/ayllu.h"
#include "chaski/store.h"
#include "chaski/transport.h"
#include "chaski/wire.h"

namespace chaski::syncengine {

// Trigger records why this sync is happening. It changes nothing about the
// request; it decides whether a failure is worth showing the child (client
// §5.3: background retries never wake the screen).
enum class Trigger {
  kUserKey,       // the sync key was pressed — failures are visible
  kScheduled,     // the interval elapsed
  kPututu,        // the doorbell rang (client §7)
  kOutboundQueued,// a letter is waiting and we woke anyway
};

// Fault is the device-visible outcome of a sync attempt, mapping the coarse
// HTTP semantics of server §4.1 onto the states of client §11.6.
enum class Fault {
  kNone,
  kNoSignal,          // transport failure: letters wait; NOT an error screen
  kCantReachHome,     // TLS trust failure (D-6) — its own visible state
  kProvisioningFault, // 401: bad/missing token; do not retry hot
  kRoadBusy,          // 503 with Retry-After
  kServerFault,       // anything else: retry the identical request later
};

struct Outcome {
  Fault fault = Fault::kNone;
  int letters_stored = 0;
  int letters_deduped = 0;
  int acks_applied = 0;
  bool more = false;          // server has more above the new cursor
  int rounds = 0;             // drain rounds actually performed
  bool ayllu_updated = false;
  bool config_updated = false;
};

// Clock is a seam because the device has no RTC: wall-clock time is valid only
// after the first sync of a power cycle, and until then dates render blank
// rather than wrong (client §5.6, C-21).
class Clock {
 public:
  virtual ~Clock() = default;
  virtual std::int64_t NowEpoch() const = 0;   // 0 when not yet valid
  virtual bool Valid() const = 0;
  virtual void SetFromServer(std::int64_t epoch) = 0;
  virtual std::int64_t MonotonicMs() const = 0;
};

// Config the engine needs that does not come from the server.
struct Options {
  std::string firmware_version;
  std::size_t letters_keep = store::kDefaultLettersKeep;
  int max_drain_rounds = 10;  // server §4.6 hard cap (C-6)
};

// Deps are the seams the engine runs on. Every one of them has a host
// implementation, which is why the entire letter path is testable with no
// hardware (implementation-plan §4 wave 1).
struct Deps {
  transport::Transport* transport = nullptr;
  store::LetterStore* letters = nullptr;
  store::Outbox* outbox = nullptr;
  store::SeenRing* seen = nullptr;
  store::StateStore* state = nullptr;
  ayllu::Store* contacts = nullptr;
  Clock* clock = nullptr;

  // on_config is called when the server pushes config: the caller applies the
  // parts that live outside the engine (RAT to the modem, PIN state, sync
  // interval). Unknown fields are ignored on purpose (client §5.5).
  std::function<void(const wire::DeviceConfig&)> on_config;

  // kipu supplies the tier-1 health block for the request (client §13).
  std::function<std::optional<wire::Kipu>()> kipu;

  // pututu_counter_seen reports the highest accepted doorbell counter, so a
  // rolled-back server heals over the wire (server §10.3).
  std::function<std::uint64_t()> pututu_counter_seen;

  // fault_hook exists for tests: it is invoked at each numbered step of
  // ApplyResponse so C-4 can cut power at every boundary and assert recovery.
  // Production leaves it null.
  std::function<void(int step)> fault_hook;
};

class Engine {
 public:
  virtual ~Engine() = default;

  // RunOnce performs one request/response exchange plus, when the server sets
  // more=true, up to max_drain_rounds immediate follow-ups. A drain loop counts
  // as ONE sync event for doorbell coalescing (server §4.6, §10.1) — callers
  // must not report it as several.
  virtual Outcome RunOnce(Trigger t) = 0;

  // BuildRequest is exposed for tests and for the bench harness; production
  // paths go through RunOnce.
  virtual wire::Request BuildRequest() const = 0;

  // ApplyResponse performs steps 1-6 above. Exposed so host tests can drive
  // the ordering directly with the fault hook (C-4).
  virtual Outcome ApplyResponse(const wire::Response& r) = 0;

  // NextBackoffMs reports how long to wait after a failed attempt. The
  // schedule is capped and never hot-loops; 401 does not back off at all, it
  // stops until a deliberate key press (client §5.3).
  virtual int NextBackoffMs() const = 0;
  virtual void ResetBackoff() = 0;
};

std::unique_ptr<Engine> New(const Deps& d, const Options& o);

}  // namespace chaski::syncengine
