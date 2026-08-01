// Package wire mirrors the Go type `internal/protocol` on the device side.
//
// The server owns the wire's shape (wasi-server-plan §4.2 request, §4.3
// response); this header only mirrors it. The mirror is kept honest by host
// tests that round-trip canonical fixtures generated from the Go structs into
// test/firmware/host/testdata/wire/ — drift fails a test rather than a device
// (chaski-implementation-plan §2).
//
// Glossary (design-spec §0.1): ayllu = contact list, kipu = health telemetry,
// pututu = the SMS doorbell. Wire field names may carry these words because the
// wire is machine-facing; strings.c may not (client spec §0, C-15).
//
// Deliberate absences, per server §1.1 and client §1.1: no addresses, no
// threading or reply linkage, no read state, no Held counts, no layout numbers.
// Adding any of them here is a spec reversal, not a feature.
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace chaski::wire {

// AckStatus is the outcome of one outbound letter. EVERY status is terminal:
// on any ack the device removes the letter from its outbox and never resends
// it (server §4.7, client D-5). A letter left unacked is retried forever, which
// is the deliberate trade — losing a letter is the one failure the system
// refuses to buy.
enum class AckStatus {
  kSent,                     // handed to SMTP
  kRejectedInactive,         // contact is a tombstone (server §7.2)
  kRejectedUnknownContact,   // contact id not in the ayllu
  kInvalid,                  // empty body, over the grapheme cap, bad fields
  kRejectedUndeliverable,    // permanent SMTP refusal (server A.11)
  kUnknown,                  // forward compatibility: treat as terminal too
};

// AckIsReject reports whether this status should surface the couldn't-send
// state to the child (client §5.4). kSent is the only non-reject. kUnknown
// counts as a reject: an ack we cannot interpret is still terminal, and telling
// the child to ask their guardians is the safe reading.
bool AckIsReject(AckStatus s);

// ParseAckStatus maps a wire string to the enum; unrecognised values become
// kUnknown rather than failing the whole response.
AckStatus ParseAckStatus(const std::string& s);

struct Ack {
  std::string local_id;
  AckStatus status = AckStatus::kUnknown;
};

// Letter is one inbound letter. The body is a single string and the device
// reflows it: pagination and line breaking are device-owned (server §4.9,
// A.10). There is no page count on this wire and there must never be one.
struct Letter {
  std::string id;          // "l-" + 10 hex chars, stable (server §4.5)
  std::string contact_id;
  std::string subject;     // normalised server-side, <=100 graphemes
  std::int64_t date = 0;   // epoch seconds
  std::string body;        // <= max_letter_chars graphemes
  bool trimmed = false;    // quoted tail removed
  bool truncated = false;  // body cut at the cap; the archive keeps the rest
  bool degraded = false;   // strip fallback used; next sync re-derives cleanly
};

// AylluContact carries identity, display name, the active flag, and the
// guardian-set cosmetic defaults. No address, ever (I-2, client D-2).
//
// Tombstones arrive with active=false and MUST still render on old letters
// while staying out of the compose picker (server §7.2). The system contact
// c_sys ships the same way, named "Wasi" (server A.15, A.16).
struct AylluContact {
  std::string id;
  std::string name;
  bool active = false;
  bool pinned = false;
  int order = 0;
  std::string portrait;  // glyph id from the built-in set; never image bytes
};

struct Ayllu {
  int version = 0;
  std::vector<AylluContact> contacts;
};

// DeviceConfig is pushed configuration (server §4.3, §13). Content knobs only:
// no chars_per_page, no page counts, no layout numbers of any kind (A.10).
//
// Every field is optional so that ABSENT is distinguishable from PRESENT.
// §5.5 applies the block field-by-field and ignores what it does not
// recognise; an absent field is the same case, and must leave the device's
// current value alone. Decoding it to the server's documented default instead
// would mean a server that stopped sending `max_letter_chars` silently reset
// every device to 500 — a config change nobody made and no log records
// (raised during the Wave 1 build).
struct DeviceConfig {
  std::optional<int> max_letter_chars;
  std::optional<int> sync_interval_s;
  std::optional<std::string> rat;    // pushed to the modem (design §6.2)
  std::optional<std::string> cover;  // never adds content to the cover (B.5)
};

// Documented server defaults (server §13), for a device that has never been
// told otherwise. These are a starting point, NOT what an absent field means.
inline constexpr int kDefaultMaxLetterChars = 500;
inline constexpr int kDefaultSyncIntervalS = 21600;

// Outbound is one child-authored letter awaiting submission.
struct Outbound {
  std::string local_id;    // device-assigned, <=32 bytes, NEVER reused (C-5)
  std::string contact_id;
  std::string subject;     // optional; empty means the server generates one
  std::string body;
};

// Kipu is the tier-1 health block (server §4.8). Health only in v1 — no
// position, no behaviour, capped at kMaxKipuBytes when serialised (§13).
struct Kipu {
  int battery_pct = 0;
  bool charging = false;
  std::string rat;
  int rssi = 0;
  int queue_depth = 0;
  std::string fw;
};

struct Request {
  std::string cursor;  // opaque; stored verbatim, echoed, NEVER parsed (§4.4)
  int ayllu_version = 0;
  std::uint64_t pututu_counter_seen = 0;
  std::optional<Kipu> kipu;
  std::vector<Outbound> outbound;
};

struct Response {
  std::int64_t server_time = 0;
  std::string cursor;
  std::uint64_t pututu_counter = 0;
  std::vector<Ack> acks;
  std::vector<Letter> letters;
  bool more = false;
  std::optional<Ayllu> ayllu;   // present only on version change
  std::optional<DeviceConfig> config;
};

// Serialisation. Encode never emits a field the server would reject; Decode
// tolerates unknown fields (forward compatibility) and reports malformed JSON
// rather than throwing — this runs on a device that must not crash on a bad
// byte from the road.
std::string EncodeRequest(const Request& r);
bool DecodeResponse(const std::string& json, Response& out);

// Caps that belong to the wire, mirrored from internal/protocol.
inline constexpr int kMaxKipuBytes = 512;
inline constexpr int kMaxSubjectGraphemes = 100;
inline constexpr int kMaxLocalIdBytes = 32;

// The reserved system contact notice letters come from (server §7.4).
inline constexpr const char* kSysContactId = "c_sys";

}  // namespace chaski::wire
