// In-memory doubles for the seams the sync engine runs on.
//
// They are deliberately dumb: the assertions in syncengine_test.cpp are about
// the engine's ordering and its use of the seams, not about how a store lays
// bytes on flash — that is agent 1A's suite, against the real file-backed
// implementations.
//
// The one piece of behaviour they do model is a power cut. `Flash::cut` makes
// every durable write fail from that moment on, which is what a dropped rail
// looks like from above the seam, and it is how C-4 checks that a crash at each
// numbered step of §5.2 recovers cleanly.
#pragma once

#include <algorithm>
#include <cstdint>
#include <functional>
#include <map>
#include <string>
#include <vector>

#include "chaski/ayllu.h"
#include "chaski/store.h"
#include "chaski/syncengine.h"
#include "chaski/transport.h"

namespace chaski::testing {

struct Flash {
  bool cut = false;
  bool Writable() const { return !cut; }
};

class FakeClock : public syncengine::Clock {
 public:
  std::int64_t NowEpoch() const override { return epoch_; }
  bool Valid() const override { return valid_; }
  void SetFromServer(std::int64_t epoch) override {
    epoch_ = epoch;
    valid_ = true;
    ++disciplined;
  }
  std::int64_t MonotonicMs() const override { return mono_ms; }

  std::int64_t mono_ms = 0;
  int disciplined = 0;

 private:
  std::int64_t epoch_ = 0;
  bool valid_ = false;
};

// FakeTransport answers with whatever the script says. Requests are kept so a
// test can assert what the device actually put on the wire.
class FakeTransport : public transport::Transport {
 public:
  using Responder = std::function<transport::Result(int round, const std::string& body)>;

  transport::Result Sync(const std::string& request_json) override {
    requests.push_back(request_json);
    return responder ? responder(static_cast<int>(requests.size()) - 1, request_json)
                     : transport::Result{};
  }
  const char* Name() const override { return "fake"; }

  Responder responder;
  std::vector<std::string> requests;
};

// Ok builds a 200 with a body; the other helpers build the failure shapes of
// server §4.1.
inline transport::Result Ok(const std::string& body) {
  transport::Result r;
  r.outcome = transport::Outcome::kOk;
  r.http_status = 200;
  r.body = body;
  return r;
}

inline transport::Result Status(int code, int retry_after_s = 0) {
  transport::Result r;
  r.outcome = transport::Outcome::kOk;
  r.http_status = code;
  r.retry_after_s = retry_after_s;
  return r;
}

inline transport::Result TransportFail() {
  transport::Result r;
  r.outcome = transport::Outcome::kTransportFail;
  return r;
}

inline transport::Result TlsTrustFail() {
  transport::Result r;
  r.outcome = transport::Outcome::kTlsTrustFail;
  return r;
}

class FakeLetterStore : public store::LetterStore {
 public:
  explicit FakeLetterStore(Flash* f) : flash_(f) {}

  bool Put(const wire::Letter& l) override {
    if (!flash_->Writable()) return false;
    if (put_budget >= 0) {
      if (put_budget == 0) return false;
      --put_budget;
    }
    auto it = by_id_.find(l.id);
    if (it != by_id_.end()) {
      // Idempotent by id, and a repeat never resurrects the unread flag of a
      // letter the child already read (store.h, server §4.5).
      it->second.letter = l;
      return true;
    }
    store::StoredLetter s;
    s.letter = l;
    s.unread = true;
    by_id_[l.id] = s;
    order_.push_back(l.id);
    return true;
  }

  bool Get(const std::string& id, store::StoredLetter& out) const override {
    auto it = by_id_.find(id);
    if (it == by_id_.end()) return false;
    out = it->second;
    return true;
  }

  std::vector<store::StoredLetter> ListNewestFirst(std::size_t limit) const override {
    std::vector<store::StoredLetter> out;
    for (auto it = order_.rbegin(); it != order_.rend() && out.size() < limit; ++it) {
      out.push_back(by_id_.at(*it));
    }
    return out;
  }

  bool MarkRead(const std::string& id) override {
    auto it = by_id_.find(id);
    if (it == by_id_.end()) return false;
    it->second.unread = false;
    return true;
  }

  std::size_t UnreadCount() const override {
    std::size_t n = 0;
    for (const auto& kv : by_id_) n += kv.second.unread ? 1 : 0;
    return n;
  }

  std::size_t EvictBeyond(std::size_t keep) override {
    std::size_t dropped = 0;
    while (order_.size() > keep) {
      by_id_.erase(order_.front());
      order_.erase(order_.begin());
      ++dropped;
    }
    evict_calls.push_back(keep);
    return dropped;
  }

  std::size_t Count() const override { return by_id_.size(); }

  bool Has(const std::string& id) const { return by_id_.count(id) != 0; }

  // put_budget >= 0 fails every Put past the budget, so a test can cut power
  // in the middle of step 4 rather than only at its boundary.
  int put_budget = -1;
  std::vector<std::size_t> evict_calls;

 private:
  Flash* flash_;
  std::map<std::string, store::StoredLetter> by_id_;
  std::vector<std::string> order_;
};

class FakeOutbox : public store::Outbox {
 public:
  explicit FakeOutbox(Flash* f) : flash_(f) {}

  bool Add(const std::string& contact_id, const std::string& subject, const std::string& body,
           std::int64_t composed_at, std::string& out_local_id) override {
    if (!flash_->Writable()) return false;
    out_local_id = "o-" + std::to_string(++high_water_);
    store::OutboxEntry e;
    e.local_id = out_local_id;
    e.contact_id = contact_id;
    e.subject = subject;
    e.body = body;
    e.composed_at = composed_at;
    entries.push_back(e);
    return true;
  }

  std::vector<store::OutboxEntry> Sendable() const override {
    std::vector<store::OutboxEntry> out;
    for (const store::OutboxEntry& e : entries) {
      if (!e.rejected) out.push_back(e);
    }
    return out;
  }

  std::vector<store::OutboxEntry> All() const override { return entries; }

  bool Resolve(const std::string& local_id, wire::AckStatus s) override {
    if (!flash_->Writable()) return false;
    ++resolve_calls;
    for (auto it = entries.begin(); it != entries.end(); ++it) {
      if (it->local_id != local_id) continue;
      if (s == wire::AckStatus::kSent) {
        entries.erase(it);
      } else {
        it->rejected = true;
        it->reject_status = s;
      }
      return true;
    }
    // A replayed ack for a local id already resolved is a no-op success: the
    // server may repeat any ack (server §4.7).
    return true;
  }

  bool Discard(const std::string& local_id) override {
    if (!flash_->Writable()) return false;
    for (auto it = entries.begin(); it != entries.end(); ++it) {
      if (it->local_id == local_id) {
        entries.erase(it);
        return true;
      }
    }
    return false;
  }

  std::size_t SendableCount() const override { return Sendable().size(); }

  // Seed puts a letter in the outbox without going through the clock.
  void Seed(const std::string& local_id, const std::string& contact_id,
            const std::string& subject, const std::string& body) {
    store::OutboxEntry e;
    e.local_id = local_id;
    e.contact_id = contact_id;
    e.subject = subject;
    e.body = body;
    entries.push_back(e);
  }

  const store::OutboxEntry* Find(const std::string& local_id) const {
    for (const store::OutboxEntry& e : entries) {
      if (e.local_id == local_id) return &e;
    }
    return nullptr;
  }

  std::vector<store::OutboxEntry> entries;
  int resolve_calls = 0;

 private:
  Flash* flash_;
  std::uint64_t high_water_ = 0;
};

class FakeSeenRing : public store::SeenRing {
 public:
  explicit FakeSeenRing(Flash* f) : flash_(f) {}

  bool Contains(const std::string& letter_id) const override {
    return std::find(ids.begin(), ids.end(), letter_id) != ids.end();
  }

  bool Add(const std::string& letter_id) override {
    if (!flash_->Writable()) return false;
    if (!Contains(letter_id)) ids.push_back(letter_id);
    if (ids.size() > store::kMinSeenIds) ids.erase(ids.begin());
    return true;
  }

  std::size_t Capacity() const override { return store::kMinSeenIds; }

  std::vector<std::string> ids;

 private:
  Flash* flash_;
};

class FakeStateStore : public store::StateStore {
 public:
  explicit FakeStateStore(Flash* f) : flash_(f) {}

  store::SyncState Snapshot() const override { return state; }

  bool SetCursor(const std::string& cursor) override {
    if (!flash_->Writable()) return false;
    state.cursor = cursor;
    ++cursor_writes;
    return true;
  }

  bool SetAylluVersion(int v) override {
    if (!flash_->Writable()) return false;
    state.ayllu_version = v;
    return true;
  }

  bool SetLastSyncAt(std::int64_t t) override {
    if (!flash_->Writable()) return false;
    state.last_sync_at = t;
    ++last_sync_writes;
    return true;
  }

  store::SyncState state;
  int cursor_writes = 0;
  int last_sync_writes = 0;

 private:
  Flash* flash_;
};

class FakeAyllu : public ayllu::Store {
 public:
  explicit FakeAyllu(Flash* f) : flash_(f) {}

  bool ApplySnapshot(const wire::Ayllu& a) override {
    if (!flash_->Writable()) return false;
    snapshot = a;
    ++snapshots_applied;
    return true;
  }

  int Version() const override { return snapshot.version; }

  std::vector<ayllu::Contact> Merged() const override {
    std::vector<ayllu::Contact> out;
    for (const wire::AylluContact& c : snapshot.contacts) {
      ayllu::Contact m;
      m.id = c.id;
      m.name = c.name;
      m.server_name = c.name;
      m.active = c.active;
      m.pinned = c.pinned;
      m.order = c.order;
      m.portrait = c.portrait;
      out.push_back(m);
    }
    return out;
  }

  std::vector<ayllu::Contact> Composable() const override {
    std::vector<ayllu::Contact> out;
    for (const ayllu::Contact& c : Merged()) {
      if (c.active && c.id != wire::kSysContactId) out.push_back(c);
    }
    return out;
  }

  bool Lookup(const std::string& id, ayllu::Contact& out) const override {
    for (const ayllu::Contact& c : Merged()) {
      if (c.id == id) {
        out = c;
        return true;
      }
    }
    return false;
  }

  bool SetOverlay(const std::string&, const ayllu::Overlay&) override { return true; }
  bool ClearOverlay(const std::string&) override { return true; }

  wire::Ayllu snapshot;
  int snapshots_applied = 0;

 private:
  Flash* flash_;
};

// Rig wires one engine to one set of doubles, which is all any test here needs.
struct Rig {
  Flash flash;
  FakeTransport transport;
  FakeClock clock;
  FakeLetterStore letters{&flash};
  FakeOutbox outbox{&flash};
  FakeSeenRing seen{&flash};
  FakeStateStore state{&flash};
  FakeAyllu contacts{&flash};
  std::vector<int> steps;                  // fault-hook trace (§5.2)
  std::vector<wire::DeviceConfig> configs;  // what on_config received

  syncengine::Deps Deps() {
    syncengine::Deps d;
    d.transport = &transport;
    d.letters = &letters;
    d.outbox = &outbox;
    d.seen = &seen;
    d.state = &state;
    d.contacts = &contacts;
    d.clock = &clock;
    d.on_config = [this](const wire::DeviceConfig& c) { configs.push_back(c); };
    d.fault_hook = [this](int step) {
      steps.push_back(step);
      if (cut_at_step > 0 && step == cut_at_step) flash.cut = true;
    };
    return d;
  }

  std::unique_ptr<syncengine::Engine> Engine(syncengine::Options o = {}) {
    return syncengine::New(Deps(), o);
  }

  // cut_at_step > 0 drops the rail as that numbered step begins, so the step
  // and everything after it fails to write.
  int cut_at_step = 0;
};

}  // namespace chaski::testing
