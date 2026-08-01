// Implementation of components/wire — see include/chaski/wire.h for the
// contract and the spec clauses it must satisfy.
//
// The server owns the wire's shape (server §4.2, §4.3); this file only mirrors
// it, and the mirror is asserted against generated fixtures rather than trusted
// (implementation-plan §2).
//
// Two rules shape everything below:
//
//   Encode omits what the Go types omit. `omitempty` on the server side is not
//   cosmetic — an empty sync is the normal heartbeat (server §4.2) and every
//   byte is billed per MB.
//
//   Decode never trusts its input. These bytes arrived from the road: unknown
//   fields are ignored for forward compatibility (client §5.5), wrong-typed
//   known fields fall back to their defaults, and a response missing something
//   the device cannot function without is rejected whole rather than applied in
//   part. Nothing here may crash on a bad byte.
#include "chaski/wire.h"

// IDF's `json` component exposes cJSON top-level; Debian ships it under
// cjson/. This spelling is the one that resolves on both, and the host test
// tree puts the Debian subdirectory on the include path to keep it that way
// (finding F-C3). One spelling, no platform #ifdef.
#include "cJSON.h"

#include <cstddef>
#include <utility>

namespace chaski::wire {
namespace {

// Wire spellings of AckStatus, from internal/protocol's constants.
constexpr const char* kAckSent = "sent";
constexpr const char* kAckRejectedInactive = "rejected_inactive";
constexpr const char* kAckRejectedUnknownContact = "rejected_unknown_contact";
constexpr const char* kAckInvalid = "invalid";
constexpr const char* kAckRejectedUndeliverable = "rejected_undeliverable";

// Owner frees a cJSON tree on every exit path. There are no exceptions here
// (implementation-plan §1), so this exists only so the early returns that
// reject malformed input cannot leak.
class Owner {
 public:
  explicit Owner(cJSON* n) : node_(n) {}
  ~Owner() { cJSON_Delete(node_); }
  Owner(const Owner&) = delete;
  Owner& operator=(const Owner&) = delete;
  cJSON* get() const { return node_; }

 private:
  cJSON* node_;
};

// Field readers. Each is tolerant by design: a missing or wrong-typed field
// yields the default rather than failing the parse, so a server that adds a
// field or changes an unrelated type cannot brick a device in a pocket.
std::string StringOr(const cJSON* o, const char* key, const char* def) {
  const cJSON* it = cJSON_GetObjectItemCaseSensitive(o, key);
  if (!cJSON_IsString(it) || it->valuestring == nullptr) return def;
  return it->valuestring;
}

std::int64_t Int64Or(const cJSON* o, const char* key, std::int64_t def) {
  const cJSON* it = cJSON_GetObjectItemCaseSensitive(o, key);
  if (!cJSON_IsNumber(it)) return def;
  return static_cast<std::int64_t>(it->valuedouble);
}

int IntOr(const cJSON* o, const char* key, int def) {
  return static_cast<int>(Int64Or(o, key, def));
}

// Counters cross the wire as JSON numbers and therefore carry double
// precision: exact below 2^53, which is every doorbell counter this device
// will ever see (server §10.3 — one per SMS).
std::uint64_t Uint64Or(const cJSON* o, const char* key, std::uint64_t def) {
  const cJSON* it = cJSON_GetObjectItemCaseSensitive(o, key);
  if (!cJSON_IsNumber(it) || it->valuedouble < 0) return def;
  return static_cast<std::uint64_t>(it->valuedouble);
}

bool BoolOr(const cJSON* o, const char* key, bool def) {
  const cJSON* it = cJSON_GetObjectItemCaseSensitive(o, key);
  if (!cJSON_IsBool(it)) return def;
  return cJSON_IsTrue(it) != 0;
}

// RequiredString distinguishes absent from empty: the cursor is legitimately
// empty (window resync, server §4.4) but must be present.
bool RequiredString(const cJSON* o, const char* key, std::string& out) {
  const cJSON* it = cJSON_GetObjectItemCaseSensitive(o, key);
  if (!cJSON_IsString(it) || it->valuestring == nullptr) return false;
  out = it->valuestring;
  return true;
}

bool DecodeAck(const cJSON* o, Ack& out) {
  std::string status;
  if (!RequiredString(o, "local_id", out.local_id) || out.local_id.empty()) return false;
  if (!RequiredString(o, "status", status)) return false;
  out.status = ParseAckStatus(status);
  return true;
}

bool DecodeLetter(const cJSON* o, Letter& out) {
  // A letter with no id cannot be deduped (server §4.5) or addressed in the
  // store, so an id-less letter is a malformed response, not a droppable one.
  if (!RequiredString(o, "id", out.id) || out.id.empty()) return false;
  out.contact_id = StringOr(o, "contact_id", "");
  out.subject = StringOr(o, "subject", "");
  out.date = Int64Or(o, "date", 0);
  out.body = StringOr(o, "body", "");
  out.trimmed = BoolOr(o, "trimmed", false);
  out.truncated = BoolOr(o, "truncated", false);
  out.degraded = BoolOr(o, "degraded", false);
  return true;
}

bool DecodeContact(const cJSON* o, AylluContact& out) {
  if (!RequiredString(o, "id", out.id) || out.id.empty()) return false;
  out.name = StringOr(o, "name", "");
  out.active = BoolOr(o, "active", false);
  out.pinned = BoolOr(o, "pinned", false);
  out.order = IntOr(o, "order", 0);
  out.portrait = StringOr(o, "portrait", "");
  return true;
}

// DecodeConfig reports only what the block actually carried. An absent field
// stays absent so the caller leaves the device's current value alone (§5.5);
// filling it from a default here would turn a server that stopped sending a
// field into a silent reset on every device.
DeviceConfig DecodeConfig(const cJSON* o) {
  DeviceConfig c;
  const cJSON* mlc = cJSON_GetObjectItemCaseSensitive(o, "max_letter_chars");
  if (cJSON_IsNumber(mlc)) c.max_letter_chars = mlc->valueint;
  const cJSON* si = cJSON_GetObjectItemCaseSensitive(o, "sync_interval_s");
  if (cJSON_IsNumber(si)) c.sync_interval_s = si->valueint;
  const cJSON* rat = cJSON_GetObjectItemCaseSensitive(o, "rat");
  if (cJSON_IsString(rat) && rat->valuestring != nullptr) c.rat = rat->valuestring;
  const cJSON* cover = cJSON_GetObjectItemCaseSensitive(o, "cover");
  if (cJSON_IsString(cover) && cover->valuestring != nullptr) c.cover = cover->valuestring;
  return c;
}

// EncodeKipu mirrors the tier-1 block (server §4.8, client §13). Health only:
// no position, no behaviour, nothing about what the child wrote.
cJSON* EncodeKipu(const Kipu& k) {
  cJSON* o = cJSON_CreateObject();
  if (o == nullptr) return nullptr;
  cJSON_AddNumberToObject(o, "battery_pct", k.battery_pct);
  cJSON_AddBoolToObject(o, "charging", k.charging);
  cJSON_AddStringToObject(o, "fw", k.fw.c_str());
  cJSON_AddNumberToObject(o, "queue_depth", k.queue_depth);
  cJSON_AddStringToObject(o, "rat", k.rat.c_str());
  cJSON_AddNumberToObject(o, "rssi", k.rssi);
  return o;
}

// KipuFits keeps the device from sending a block the server would only drop
// (server §4.8 caps it at kMaxKipuBytes, and internal/kipu drops an oversized
// one rather than failing the sync). Health telemetry is the one part of the
// request that may be sacrificed; the letters still go.
bool KipuFits(cJSON* block) {
  char* text = cJSON_PrintUnformatted(block);
  if (text == nullptr) return false;
  const bool fits = std::string(text).size() <= static_cast<std::size_t>(kMaxKipuBytes);
  cJSON_free(text);
  return fits;
}

}  // namespace

// kSent is the only status that is not a reject, kUnknown very much included
// (client §5.4).
bool AckIsReject(AckStatus s) { return s != AckStatus::kSent; }

AckStatus ParseAckStatus(const std::string& s) {
  if (s == kAckSent) return AckStatus::kSent;
  if (s == kAckRejectedInactive) return AckStatus::kRejectedInactive;
  if (s == kAckRejectedUnknownContact) return AckStatus::kRejectedUnknownContact;
  if (s == kAckInvalid) return AckStatus::kInvalid;
  if (s == kAckRejectedUndeliverable) return AckStatus::kRejectedUndeliverable;
  return AckStatus::kUnknown;
}

std::string EncodeRequest(const Request& r) {
  Owner root(cJSON_CreateObject());
  if (root.get() == nullptr) return std::string();

  // cursor and ayllu_version are the only unconditional fields (server §4.2).
  // The cursor is echoed exactly as it arrived — the device never parses it,
  // and "" is a meaningful value: window resync (server §4.4).
  cJSON_AddStringToObject(root.get(), "cursor", r.cursor.c_str());
  cJSON_AddNumberToObject(root.get(), "ayllu_version", r.ayllu_version);

  if (r.pututu_counter_seen != 0) {
    cJSON_AddNumberToObject(root.get(), "pututu_counter_seen",
                            static_cast<double>(r.pututu_counter_seen));
  }

  if (r.kipu.has_value()) {
    cJSON* block = EncodeKipu(*r.kipu);
    if (block != nullptr && KipuFits(block)) {
      cJSON_AddItemToObject(root.get(), "kipu", block);
    } else {
      cJSON_Delete(block);
    }
  }

  if (!r.outbound.empty()) {
    cJSON* arr = cJSON_AddArrayToObject(root.get(), "outbound");
    for (const Outbound& o : r.outbound) {
      cJSON* item = cJSON_CreateObject();
      if (arr == nullptr || item == nullptr) {
        cJSON_Delete(item);
        break;
      }
      cJSON_AddStringToObject(item, "local_id", o.local_id.c_str());
      cJSON_AddStringToObject(item, "contact_id", o.contact_id.c_str());
      // An absent subject is how the device asks the server to generate one
      // (server §6.2). Match the Go encoder and omit rather than send "".
      if (!o.subject.empty()) {
        cJSON_AddStringToObject(item, "subject", o.subject.c_str());
      }
      cJSON_AddStringToObject(item, "body", o.body.c_str());
      cJSON_AddItemToArray(arr, item);
    }
  }

  char* text = cJSON_PrintUnformatted(root.get());
  if (text == nullptr) return std::string();
  std::string out(text);
  cJSON_free(text);
  return out;
}

bool DecodeResponse(const std::string& json, Response& out) {
  // ParseWithLength, not Parse: the body is bytes off the road and may contain
  // anything, an embedded NUL included.
  Owner root(cJSON_ParseWithLength(json.data(), json.size()));
  if (!cJSON_IsObject(root.get())) return false;

  Response r;
  // The cursor is the one field whose absence is fatal: defaulting it to ""
  // would silently ask for a window resync (server §4.4) on the next sync, and
  // guessing is worse than retrying the identical request.
  if (!RequiredString(root.get(), "cursor", r.cursor)) return false;

  r.server_time = Int64Or(root.get(), "server_time", 0);
  r.pututu_counter = Uint64Or(root.get(), "pututu_counter", 0);
  r.more = BoolOr(root.get(), "more", false);

  const cJSON* acks = cJSON_GetObjectItemCaseSensitive(root.get(), "acks");
  if (cJSON_IsArray(acks)) {
    const cJSON* it = nullptr;
    cJSON_ArrayForEach(it, acks) {
      Ack a;
      // An ack that cannot be keyed to a local id is unusable, and applying
      // the rest of the response around it would drop a terminal outcome for a
      // letter still sitting in the outbox (D-5).
      if (!cJSON_IsObject(it) || !DecodeAck(it, a)) return false;
      r.acks.push_back(std::move(a));
    }
  }

  const cJSON* letters = cJSON_GetObjectItemCaseSensitive(root.get(), "letters");
  if (cJSON_IsArray(letters)) {
    const cJSON* it = nullptr;
    cJSON_ArrayForEach(it, letters) {
      Letter l;
      if (!cJSON_IsObject(it) || !DecodeLetter(it, l)) return false;
      r.letters.push_back(std::move(l));
    }
  }

  const cJSON* ayllu = cJSON_GetObjectItemCaseSensitive(root.get(), "ayllu");
  if (cJSON_IsObject(ayllu)) {
    Ayllu a;
    a.version = IntOr(ayllu, "version", 0);
    const cJSON* contacts = cJSON_GetObjectItemCaseSensitive(ayllu, "contacts");
    if (cJSON_IsArray(contacts)) {
      const cJSON* it = nullptr;
      cJSON_ArrayForEach(it, contacts) {
        AylluContact c;
        // The snapshot replaces membership wholesale (client §4.4), so half a
        // contact list is not something the device may apply.
        if (!cJSON_IsObject(it) || !DecodeContact(it, c)) return false;
        a.contacts.push_back(std::move(c));
      }
    }
    r.ayllu = std::move(a);
  }

  const cJSON* config = cJSON_GetObjectItemCaseSensitive(root.get(), "config");
  if (cJSON_IsObject(config)) r.config = DecodeConfig(config);

  out = std::move(r);
  return true;
}

}  // namespace chaski::wire
