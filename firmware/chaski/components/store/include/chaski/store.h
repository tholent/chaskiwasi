// Package store is the device's durable state: letters, outbox, seen-ring,
// sync state (client spec §4).
//
// Portability rule (implementation-plan ground rule 3): this component uses
// POSIX file APIs only — no esp_* and no FreeRTOS headers. On the target those
// calls land on LittleFS through the IDF VFS; on the host they land in a temp
// directory. That is what lets the whole letter path be host-tested (C-2…C-5).
//
// Write discipline: every mutation is atomic — write a temp file in the same
// directory, fsync it, rename over the target, fsync the directory. A power cut
// must never leave a half-written file where the previous version used to be.
// This is the same discipline internal/atomicfile enforces server-side (§4).
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>
#include <string>
#include <vector>

#include "chaski/wire.h"

namespace chaski::store {

// StoredLetter is a delivered letter plus the one piece of state the device
// owns about it. Read state is deliberately device-local: the server holds no
// engagement data and no read receipt ever crosses the wire (client §1.1).
struct StoredLetter {
  wire::Letter letter;
  bool unread = true;
};

// LetterStore holds a bounded recent window, not the archive: the mailbox is
// canonical and the device is a derived view (design Principle 5, client B.8).
class LetterStore {
 public:
  virtual ~LetterStore() = default;

  // Put is idempotent by letter id. The server MAY re-send any letter at any
  // time (server §4.5), so a repeat must overwrite identically and must not
  // resurrect the unread flag of a letter the child already read.
  virtual bool Put(const wire::Letter& l) = 0;

  virtual bool Get(const std::string& id, StoredLetter& out) const = 0;

  // ListNewestFirst returns up to `limit` letters, newest by date first. This
  // is the inbox's only read path (client §11.1).
  virtual std::vector<StoredLetter> ListNewestFirst(std::size_t limit) const = 0;

  virtual bool MarkRead(const std::string& id) = 0;

  // UnreadCount drives the cover's mail-flag glyph — a boolean question at the
  // cover, never a number shown to a bystander (client §9, B.5).
  virtual std::size_t UnreadCount() const = 0;

  // EvictBeyond drops the oldest letters past `keep`, defaulting to
  // kDefaultLettersKeep. Eviction removes the letter file and NOT its seen-ring
  // entry, so an evicted letter is never re-delivered (client §4.1).
  virtual std::size_t EvictBeyond(std::size_t keep) = 0;

  virtual std::size_t Count() const = 0;
};

// OutboxEntry is a letter the child has finished writing. It stays here,
// visible as "on the road", until an ack arrives — any ack (client §5.4).
struct OutboxEntry {
  std::string local_id;
  std::string contact_id;
  std::string subject;
  std::string body;
  std::int64_t composed_at = 0;  // 0 when the clock was not yet valid (§5.6)

  // rejected records a terminal reject so the UI can show the couldn't-send
  // state while KEEPING the child's text (client §5.4). A rejected entry is no
  // longer sent; it waits for the child to dismiss or recompose it.
  bool rejected = false;
  wire::AckStatus reject_status = wire::AckStatus::kUnknown;
};

class Outbox {
 public:
  virtual ~Outbox() = default;

  // Add allocates the next local id and persists the entry before returning.
  // Local ids are strictly monotonic and NEVER reused, across reboot and power
  // loss alike (client §5.1, C-5): the server's ack ring keys on them, so reuse
  // would alias two different letters.
  virtual bool Add(const std::string& contact_id, const std::string& subject,
                   const std::string& body, std::int64_t composed_at,
                   std::string& out_local_id) = 0;

  // Sendable returns entries eligible for the next sync: everything not
  // already terminally rejected, oldest first.
  virtual std::vector<OutboxEntry> Sendable() const = 0;

  // All includes rejected entries, for the outbox screen.
  virtual std::vector<OutboxEntry> All() const = 0;

  // Resolve applies a terminal ack. kSent removes the entry; any reject keeps
  // it with the text intact and the reject flag set. Resolving an unknown
  // local id is a no-op success — a replayed ack must not be an error.
  virtual bool Resolve(const std::string& local_id, wire::AckStatus s) = 0;

  // Discard removes a rejected entry once the child has dealt with it.
  virtual bool Discard(const std::string& local_id) = 0;

  virtual std::size_t SendableCount() const = 0;
};

// SeenRing is the device-side dedup required by the wire contract: remember at
// least kMinSeenIds recently seen letter ids and silently drop repeats. The
// server may re-deliver at any time and correctness must never depend on it
// not doing so (server §4.5).
class SeenRing {
 public:
  virtual ~SeenRing() = default;
  virtual bool Contains(const std::string& letter_id) const = 0;
  virtual bool Add(const std::string& letter_id) = 0;
  virtual std::size_t Capacity() const = 0;
};

// SyncState is the small machine-owned record. The cursor lives HERE, in
// flash, not only in RTC memory (client B.12): a flat battery would otherwise
// cost a full window resync on a per-MB bill.
struct SyncState {
  std::string cursor;  // opaque; never parsed by the device (server §4.4)
  int ayllu_version = 0;
  std::uint64_t local_id_high_water = 0;
  std::int64_t last_sync_at = 0;
};

class StateStore {
 public:
  virtual ~StateStore() = default;
  virtual SyncState Snapshot() const = 0;

  // SetCursor is called LAST in the response-application order (client §5.2
  // step 6). A crash before it leaves the old cursor standing, the server
  // re-delivers, and the seen-ring absorbs the repeats.
  virtual bool SetCursor(const std::string& cursor) = 0;

  virtual bool SetAylluVersion(int v) = 0;
  virtual bool SetLastSyncAt(std::int64_t t) = 0;
};

// Defaults, from the client spec.
inline constexpr std::size_t kDefaultLettersKeep = 200;  // §4.1, B.8
inline constexpr std::size_t kMinSeenIds = 1000;         // server §4.5
inline constexpr std::size_t kOutboxCap = 12;            // B.9

// Concrete file-backed implementations. `root` is a directory: the LittleFS
// mount point on the target, a temp dir in host tests.
std::unique_ptr<LetterStore> OpenLetterStore(const std::string& root);
std::unique_ptr<Outbox> OpenOutbox(const std::string& root);
std::unique_ptr<SeenRing> OpenSeenRing(const std::string& root, std::size_t capacity);
std::unique_ptr<StateStore> OpenStateStore(const std::string& root);

}  // namespace chaski::store
