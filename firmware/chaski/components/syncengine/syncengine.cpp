// Implementation of components/syncengine — see include/chaski/syncengine.h
// for the contract and the spec clauses it must satisfy.
//
// Nothing in this file logs. The request and response bodies carry letter
// content, and D-7 admits no build, level, or "just while debugging" exception;
// correlation, when it is needed, uses letter ids and local ids.
#include "chaski/syncengine.h"

#include <utility>

namespace chaski::syncengine {
namespace {

// The §5.3 ladder, in milliseconds. Three steps and then the next scheduled
// wake: a device on a per-MB link may not spin, and a child whose letter is
// waiting is better served by a sync that arrives than by ten that fail.
constexpr int kBackoffLadderMs[] = {30 * 1000, 2 * 60 * 1000, 10 * 60 * 1000};
constexpr int kBackoffLadderSteps = 3;

// A Retry-After far in the future is honoured up to a day; past that it is
// clamped, because the value is server-supplied and int milliseconds overflow.
constexpr int kMaxRetryAfterS = 24 * 60 * 60;

// FaultFor maps one transport result onto the device-visible states of §11.6,
// per the coarse status table of server §4.1. The three transport outcomes stay
// distinct all the way to the screen: "can't reach home" (a trust failure, D-6)
// is a different sentence to a child than "no signal", which is not an error
// state at all — letters simply wait for the runner.
Fault FaultFor(const transport::Result& r) {
  switch (r.outcome) {
    case transport::Outcome::kTlsTrustFail:
      return Fault::kCantReachHome;
    case transport::Outcome::kTransportFail:
      return Fault::kNoSignal;
    case transport::Outcome::kOk:
      break;
  }
  switch (r.http_status) {
    case 200:
      return Fault::kNone;
    case 401:
      return Fault::kProvisioningFault;
    case 503:
      return Fault::kRoadBusy;
    default:
      return Fault::kServerFault;
  }
}

class EngineImpl : public Engine {
 public:
  EngineImpl(const Deps& d, const Options& o) : d_(d), o_(o) {}

  Outcome RunOnce(Trigger t) override;
  wire::Request BuildRequest() const override;
  Outcome ApplyResponse(const wire::Response& r) override;
  int NextBackoffMs() const override;
  void ResetBackoff() override;

 private:
  // Hook marks a numbered boundary of §5.2. Tests cut power here and assert
  // the recovery; production leaves the hook null.
  void Hook(int step) const {
    if (d_.fault_hook) d_.fault_hook(step);
  }

  bool SeamsWired() const {
    return d_.transport != nullptr && d_.letters != nullptr && d_.outbox != nullptr &&
           d_.seen != nullptr && d_.state != nullptr && d_.contacts != nullptr &&
           d_.clock != nullptr;
  }

  void NoteFailure(Fault f, int retry_after_s);

  Deps d_;
  Options o_;
  int consecutive_failures_ = 0;
  int retry_after_ms_ = 0;
  bool halted_ = false;  // 401 seen: no ladder, wait for a deliberate press
};

// §5.1: cursor verbatim, the version the device actually holds, the doorbell
// counter it has accepted, tier-1 health, and the outbox.
wire::Request EngineImpl::BuildRequest() const {
  wire::Request req;
  if (!SeamsWired()) return req;

  // The cursor is opaque: stored as it arrived, echoed as it was stored, never
  // parsed (server §4.4). "" is not a missing value, it is a window resync.
  req.cursor = d_.state->Snapshot().cursor;

  // The ayllu store is authoritative for the version because it is what holds
  // the contacts: claiming a version whose snapshot never landed would stop the
  // server from ever resending it (§5.2 step 3).
  req.ayllu_version = d_.contacts->Version();

  if (d_.pututu_counter_seen) req.pututu_counter_seen = d_.pututu_counter_seen();
  if (d_.kipu) req.kipu = d_.kipu();

  // Sendable() excludes terminally rejected entries: an ack is an ack, and a
  // rejected letter is never sent again (D-5, §5.4).
  for (const store::OutboxEntry& e : d_.outbox->Sendable()) {
    wire::Outbound o;
    o.local_id = e.local_id;
    o.contact_id = e.contact_id;
    o.subject = e.subject;
    o.body = e.body;
    req.outbound.push_back(std::move(o));
  }
  return req;
}

// ApplyResponse is §5.2 steps 1-6 in that order, and the order is the contract.
// Every durable write is checked: a failed write is treated exactly like a
// power cut — abandon the rest, leave the old cursor standing, let the server
// re-deliver into the seen-ring next time.
Outcome EngineImpl::ApplyResponse(const wire::Response& r) {
  Outcome out;
  if (!SeamsWired()) {
    out.apply_incomplete = true;
    return out;
  }

  // Step 1: the clock. The device has no RTC, so wall-clock time is whatever
  // the last sync said (§5.6). A response with no server_time leaves the clock
  // invalid rather than pretending it is 1970 (C-21).
  Hook(1);
  if (r.server_time > 0) d_.clock->SetFromServer(r.server_time);

  // Step 2: acks, durable, before anything else touches the outbox. Every
  // status is terminal (server §4.7, D-5); Resolve keeps a rejected letter's
  // text and drops a sent one, and resolving an already-resolved local id is a
  // no-op, which is what makes a replayed response harmless.
  Hook(2);
  for (const wire::Ack& a : r.acks) {
    if (!d_.outbox->Resolve(a.local_id, a.status)) {
      out.apply_incomplete = true;
      return out;
    }
    ++out.acks_applied;
  }

  // Step 3: the ayllu, before the letters, so a letter from a contact
  // introduced in this same response resolves to a name (§5.2). The snapshot
  // lands first and the version mirror second: a crash between them costs a
  // redundant snapshot next sync, whereas the reverse order would cost a
  // contact list that never arrives.
  Hook(3);
  if (r.ayllu.has_value()) {
    if (!d_.contacts->ApplySnapshot(*r.ayllu) || !d_.state->SetAylluVersion(r.ayllu->version)) {
      out.apply_incomplete = true;
      return out;
    }
    out.ayllu_updated = true;
  }

  // Step 4: letters, one at a time, check-write-record. A crash after the write
  // and before the ring entry costs one re-delivered letter, which Put absorbs
  // idempotently; the reverse order would lose it (server §4.5).
  Hook(4);
  for (const wire::Letter& l : r.letters) {
    if (d_.seen->Contains(l.id)) {
      ++out.letters_deduped;
      continue;
    }
    // An unknown contact_id is not a reason to drop a letter — it renders under
    // the fallback label (§4.4, C-14).
    if (!d_.letters->Put(l) || !d_.seen->Add(l.id)) {
      out.apply_incomplete = true;
      return out;
    }
    ++out.letters_stored;
  }
  // Eviction drops letter files and never seen-ring entries, so an evicted
  // letter is not re-delivered (§4.1, B.8). Only worth walking the store when
  // something was actually added — a heartbeat should not touch flash.
  if (out.letters_stored > 0) d_.letters->EvictBeyond(o_.letters_keep);

  // Step 5: config, field by field, unknown fields already ignored by the
  // decoder (§5.5). Nothing here is durable in the engine — settings own it.
  Hook(5);
  if (r.config.has_value()) {
    if (d_.on_config) d_.on_config(*r.config);
    out.config_updated = true;
  }

  // Step 6: the cursor, LAST, durable. Everything above is now on flash, so
  // advancing delivery is safe; if this write is the one that fails, the whole
  // response is simply delivered again.
  Hook(6);
  if (!d_.state->SetCursor(r.cursor)) {
    out.apply_incomplete = true;
    return out;
  }

  out.more = r.more;
  return out;
}

Outcome EngineImpl::RunOnce(Trigger t) {
  Outcome out;
  if (!SeamsWired()) {
    out.apply_incomplete = true;
    return out;
  }

  // The deliberate press is the thing that clears a provisioning halt (§5.3):
  // nothing else about a 401 changes until a person acts.
  if (t == Trigger::kUserKey) ResetBackoff();

  const int cap = o_.max_drain_rounds > 0 ? o_.max_drain_rounds : 1;
  bool applied_any = false;

  // §5.2 step 7 / server §4.6: on more=true sync again immediately, hard-capped
  // at cap rounds as a defence against a server that never stops saying more.
  for (int round = 0; round < cap; ++round) {
    const transport::Result res = d_.transport->Sync(wire::EncodeRequest(BuildRequest()));
    out.rounds = round + 1;

    const Fault f = FaultFor(res);
    if (f != Fault::kNone) {
      out.fault = f;
      NoteFailure(f, res.retry_after_s);
      break;
    }

    wire::Response resp;
    if (!wire::DecodeResponse(res.body, resp)) {
      // A 200 the device cannot parse is a server fault, not a road fault: the
      // identical request is retried later and the cursor has not moved.
      out.fault = Fault::kServerFault;
      NoteFailure(Fault::kServerFault, 0);
      break;
    }

    const Outcome applied = ApplyResponse(resp);
    out.letters_stored += applied.letters_stored;
    out.letters_deduped += applied.letters_deduped;
    out.acks_applied += applied.acks_applied;
    out.ayllu_updated = out.ayllu_updated || applied.ayllu_updated;
    out.config_updated = out.config_updated || applied.config_updated;
    out.more = resp.more;
    applied_any = true;

    if (applied.apply_incomplete) {
      // The cursor did not move, so draining further would re-fetch what was
      // just refused. Stop and let the next sync repeat the round.
      out.apply_incomplete = true;
      break;
    }
    if (!resp.more) break;
  }

  if (out.fault == Fault::kNone) ResetBackoff();

  // A drain loop is ONE sync event, not one per round: the doorbell's
  // skip-if-recently-synced check would otherwise count a busy sync as many
  // (server §4.6, §10.1).
  if (applied_any && d_.clock->Valid()) d_.state->SetLastSyncAt(d_.clock->NowEpoch());

  return out;
}

void EngineImpl::NoteFailure(Fault f, int retry_after_s) {
  if (f == Fault::kProvisioningFault) {
    // 401 does not back off, it stops: the token is wrong, which is a guardian
    // problem no amount of retrying fixes (§5.3).
    halted_ = true;
    retry_after_ms_ = 0;
    return;
  }
  if (f == Fault::kRoadBusy && retry_after_s > 0) {
    const int s = retry_after_s < kMaxRetryAfterS ? retry_after_s : kMaxRetryAfterS;
    retry_after_ms_ = s * 1000;
    return;
  }
  retry_after_ms_ = 0;
  ++consecutive_failures_;
}

int EngineImpl::NextBackoffMs() const {
  if (halted_) return kBackoffHalted;
  if (retry_after_ms_ > 0) return retry_after_ms_;  // 503 said when (§5.3)
  if (consecutive_failures_ <= 0 || consecutive_failures_ > kBackoffLadderSteps) {
    return kBackoffNextScheduledWake;
  }
  return kBackoffLadderMs[consecutive_failures_ - 1];
}

void EngineImpl::ResetBackoff() {
  consecutive_failures_ = 0;
  retry_after_ms_ = 0;
  halted_ = false;
}

}  // namespace

std::unique_ptr<Engine> New(const Deps& d, const Options& o) {
  return std::unique_ptr<Engine>(new EngineImpl(d, o));
}

}  // namespace chaski::syncengine
