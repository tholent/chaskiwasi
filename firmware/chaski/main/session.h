// Session is everything one boot builds before it can do any work: the flash
// mounts, the durable stores, the clock, the transport for this build variant,
// and the sync engine over them.
//
// It lives in main/ because this is the composition root and the one place
// `esp_*` is allowed (chaski-implementation-plan ground rule 3). Nothing here
// decides anything: the §5.2 order is syncengine's, the wake policy is
// wake.cpp's, the write discipline is store's. This file only says which
// implementation of each seam this image was built with.
//
// One Session per boot. It is not copyable and not thread-safe: app_main is
// the only task that touches it, and the bench control channel runs on that
// same task by design (see bench_control.h).
#pragma once

#include <cstdint>
#include <memory>
#include <string>

#include "chaski/ayllu.h"
#include "chaski/draft.h"
#include "chaski/settings.h"
#include "chaski/store.h"
#include "chaski/syncengine.h"
#include "chaski/transport.h"
// SerialLink is declared alongside the bridge transport, not in the transport
// seam: the seam is what every build has, while the byte pipe exists only where
// there is a USB link to speak over (§14).
#include "chaski/usbbridge.h"

namespace chaski::app {

// The letter partition (partitions.csv) and its mount point. Every durable
// store roots here: letters, outbox, seen-ring, sync state, draft, contacts
// (client §4.1).
inline constexpr const char* kLetterPartition = "letters";
inline constexpr const char* kLetterRoot = "/letters";

// The provisioning namespace in NVS (client §4.2). The bearer token is the
// only key this wave reads; the doorbell HMAC key and the sync URL join it in
// wave 4/5, when there is a modem to use them.
inline constexpr const char* kNvsNamespace = "chaski";
inline constexpr const char* kNvsKeyDeviceToken = "device_token";

// Clock is §5.6 as an object: wall-clock time is whatever the last sync said,
// and there is no RTC to fall back on. Until a sync happens, Valid() is false
// and dates render blank rather than wrong (C-21).
class Clock final : public syncengine::Clock {
 public:
  std::int64_t NowEpoch() const override;
  bool Valid() const override { return valid_; }
  void SetFromServer(std::int64_t epoch) override;
  std::int64_t MonotonicMs() const override;

 private:
  std::int64_t epoch_at_mark_ = 0;
  std::int64_t mono_at_mark_ = 0;
  bool valid_ = false;
};

class Session {
 public:
  Session() = default;
  Session(const Session&) = delete;
  Session& operator=(const Session&) = delete;

  // Open mounts NVS and the letter filesystem and opens every store. It
  // reports false only when the device cannot work at all; a missing bearer
  // token is not that case — it produces a 401 the child sees as "ask your
  // guardians" (§5.3), which is the correct outcome for an unprovisioned
  // device and not a boot failure.
  bool Open();

  // Reformat erases the letter partition and reopens the stores on it. The
  // bearer token is in NVS and deliberately survives: on this device a factory
  // reset means "forget the letters", not "forget who you are" (§4.2, §12).
  bool Reformat();

  // SetDeviceToken persists the bearer token and rebuilds the transport around
  // it. Wave 5's provisioning tool writes the same NVS key offline; this is
  // the path a bench session uses, since the tool is not built yet.
  bool SetDeviceToken(const std::string& token);

  bool provisioned() const { return !device_token_.empty(); }

  store::LetterStore* letters() const { return letters_.get(); }
  store::Outbox* outbox() const { return outbox_.get(); }
  store::SeenRing* seen() const { return seen_.get(); }
  store::StateStore* state() const { return state_.get(); }
  store::SettingsStore* settings() const { return settings_.get(); }
  store::DraftStore* drafts() const { return drafts_.get(); }
  ayllu::Store* contacts() const { return contacts_.get(); }
  Clock* clock() { return &clock_; }

  // link is the byte pipe to the host, null in a build with no USB transport.
  // The bench control channel shares it with the transport: both run on the
  // app_main task and never overlap, because a sync is synchronous. See
  // bench_control.h for what that costs and how it is worked within.
  transport::SerialLink* link() const { return link_.get(); }

  // engine is null when this build has no transport — which is every
  // production image until wave 4 lands ModemTransport. Callers check rather
  // than assume, so a variant with no road reports "no signal" and keeps the
  // letters instead of crashing on a null seam.
  syncengine::Engine* engine() const { return engine_.get(); }

  // pututu_counter_seen is reported on every sync so a server restored from
  // backup heals its counter over the wire (server §10.3). It is read from NVS
  // rather than from a Verifier because there is no modem to verify anything
  // with yet, and the value must still be honest across that gap.
  std::uint64_t PututuCounterSeen() const;

#if CHASKI_DEV_BUILD
  // SetCutStep arms a restart at a numbered §5.2 step, which is how the bench
  // cuts the device between sending a request and applying the response (C-4)
  // without timing a line pulse against an in-flight HTTP round trip. 0
  // disarms. Dev builds only: the hook it installs does not exist in a
  // production image.
  void SetCutStep(int step);
#endif

 private:
  bool OpenStores();
  void CloseStores();
  void BuildEngine();
  void LoadDeviceToken();

  std::unique_ptr<store::LetterStore> letters_;
  std::unique_ptr<store::Outbox> outbox_;
  std::unique_ptr<store::SeenRing> seen_;
  std::unique_ptr<store::StateStore> state_;
  std::unique_ptr<store::SettingsStore> settings_;
  std::unique_ptr<store::DraftStore> drafts_;
  std::unique_ptr<ayllu::Store> contacts_;
  Clock clock_;

  std::unique_ptr<transport::SerialLink> link_;
  std::unique_ptr<transport::Transport> transport_;
  std::unique_ptr<syncengine::Engine> engine_;

  std::string device_token_;
  bool fs_mounted_ = false;
#if CHASKI_DEV_BUILD
  int cut_step_ = 0;
#endif
};

// FaultName renders a §11.6 fault state for a log line or a bench event. The
// names are wire-stable identifiers, not user-visible text — every word the
// child reads lives in chaski_strings.c (§0, C-15).
const char* FaultName(syncengine::Fault f);

// SyncReport is what one attempt did, plus the two things the caller has to
// know that the Outcome does not carry: whether an attempt happened at all,
// and how long §5.3 says to wait before the next one.
struct SyncReport {
  bool attempted = false;
  syncengine::Outcome outcome;
  int backoff_ms = 0;  // syncengine::kBackoffHalted after a 401
};

// RunSync is the only place a sync starts, so the wake path and the bench
// control channel honour §5.3's provisioning halt identically.
//
// The halt itself belongs here rather than in the engine: RunOnce always makes
// the attempt it is asked for, and "stop retrying until the next manual
// sync-key press" is a statement about who is allowed to ask — which is a
// scheduling decision, and scheduling lives in the composition root.
SyncReport RunSync(Session& s, syncengine::Trigger t);

}  // namespace chaski::app
