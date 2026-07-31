// Implementation of components/store — see include/chaski/store.h for the
// contract and the spec clauses it must satisfy.
//
// On-disk layout under `root` (client §4.1):
//
//   letters/<id>   one delivered letter, body last in the record
//   outbox/<id>    one queued letter
//   state          cursor, ayllu_version, last_sync_at
//   seen           the seen-id ring, oldest first, one id per line
//   local_id       the local-id high-water mark, written by the Outbox alone
//
// §4.1 describes `state` as one file holding the ring and the high-water mark
// as well. They are separate files here for two reasons: the seam header hands
// them to separate objects with separate lifetimes, and folding a 12 KB ring
// into the file rewritten on every cursor advance would cost a 12 KB flash
// write per sync on a device that counts them (design §6.4).
//
// Nothing in this file logs. Letter content may not reach a log at any level in
// any build (D-7), and the cheapest way to guarantee that is for the component
// holding the content to contain no logging statement at all.
#include "chaski/store.h"

#include <algorithm>
#include <deque>
#include <string_view>
#include <utility>

#include "chaski/fsutil.h"

namespace chaski::store {
namespace {

using fsutil::Record;

constexpr char kLettersDir[] = "letters";
constexpr char kOutboxDir[] = "outbox";
constexpr char kStateFile[] = "state";
constexpr char kSeenFile[] = "seen";
constexpr char kLocalIdFile[] = "local_id";

// Local ids are zero-padded so lexical order is numeric order: the outbox is a
// directory listing, and sorting it by name must agree with the order the child
// wrote the letters in. 16 digits is far beyond the counter's reach and well
// inside the wire's 32-byte cap (wire.h kMaxLocalIdBytes).
constexpr std::size_t kLocalIdDigits = 16;

std::string FormatLocalId(std::uint64_t n) {
  const std::string s = std::to_string(n);
  return s.size() >= kLocalIdDigits ? s : std::string(kLocalIdDigits - s.size(), '0') + s;
}

std::string Bit(bool b) { return b ? "1" : "0"; }

// ParseCounter reads the one-number counter file. It goes through the record
// reader so a corrupt or empty file yields "no value" rather than a crash.
bool ParseCounter(const std::string& text, std::uint64_t& out) {
  const Record r = {{"n", text.substr(0, text.find('\n'))}};
  return fsutil::ReadU64(r, "n", out);
}

// EncodeLetter puts `body` last: everything the inbox list needs sits ahead of
// the one expensive field.
std::string EncodeLetter(const wire::Letter& l, bool unread) {
  const Record r = {
      {"id", l.id},
      {"contact_id", l.contact_id},
      {"subject", l.subject},
      {"date", std::to_string(l.date)},
      {"trimmed", Bit(l.trimmed)},
      {"truncated", Bit(l.truncated)},
      {"degraded", Bit(l.degraded)},
      {"unread", Bit(unread)},
      {"body", l.body},
  };
  return fsutil::EncodeRecord(r);
}

bool DecodeLetter(std::string_view text, StoredLetter& out) {
  const Record r = fsutil::DecodeRecord(text);
  const std::string* id = fsutil::Find(r, "id");
  if (id == nullptr || id->empty()) return false;
  out.letter.id = *id;
  if (const std::string* v = fsutil::Find(r, "contact_id")) out.letter.contact_id = *v;
  if (const std::string* v = fsutil::Find(r, "subject")) out.letter.subject = *v;
  if (const std::string* v = fsutil::Find(r, "body")) out.letter.body = *v;
  fsutil::ReadI64(r, "date", out.letter.date);
  fsutil::ReadBool(r, "trimmed", out.letter.trimmed);
  fsutil::ReadBool(r, "truncated", out.letter.truncated);
  fsutil::ReadBool(r, "degraded", out.letter.degraded);
  fsutil::ReadBool(r, "unread", out.unread);
  return true;
}

// FileLetterStore indexes everything but the bodies. Rebuilding the index at
// open costs one pass over at most `letters_keep` small files; holding the
// bodies would cost up to ~400 KB of RAM on a part with 512 KB of internal
// SRAM, for data the inbox list does not display (client §11.1).
class FileLetterStore final : public LetterStore {
 public:
  explicit FileLetterStore(std::string dir) : dir_(std::move(dir)) { Scan(); }

  // §4.1: write is idempotent by id — re-delivery overwrites identically. The
  // server MAY re-send any letter at any time (server §4.5), so this is a
  // normal path and not an error path.
  bool Put(const wire::Letter& l) override {
    if (!fsutil::NameIsSafe(l.id)) return false;
    Meta* existing = FindMeta(l.id);
    // A repeat must never resurrect the unread flag: the child has read this
    // letter and the server has no way to know (client §1.1).
    const bool unread = existing == nullptr ? true : existing->unread;
    if (!fsutil::WriteAtomic(PathFor(l.id), EncodeLetter(l, unread))) return false;
    Meta m = MetaOf(l, unread);
    if (existing != nullptr) {
      *existing = std::move(m);
    } else {
      index_.push_back(std::move(m));
    }
    return true;
  }

  bool Get(const std::string& id, StoredLetter& out) const override {
    if (FindMeta(id) == nullptr) return false;
    return Load(id, out);
  }

  std::vector<StoredLetter> ListNewestFirst(std::size_t limit) const override {
    std::vector<StoredLetter> out;
    for (const Meta* m : Ordered()) {
      if (out.size() >= limit) break;
      StoredLetter sl;
      if (Load(m->id, sl)) out.push_back(std::move(sl));
    }
    return out;
  }

  bool MarkRead(const std::string& id) override {
    Meta* m = FindMeta(id);
    if (m == nullptr) return false;
    if (!m->unread) return true;  // no flash write for a no-op (design §6.4)
    StoredLetter sl;
    if (!Load(id, sl)) return false;
    if (!fsutil::WriteAtomic(PathFor(id), EncodeLetter(sl.letter, false))) return false;
    m->unread = false;
    return true;
  }

  std::size_t UnreadCount() const override {
    std::size_t n = 0;
    for (const Meta& m : index_) {
      if (m.unread) ++n;
    }
    return n;
  }

  // §4.1: eviction removes the letter file and NOT its seen-ring entry, so an
  // evicted letter is never re-delivered. The ring is a separate object for
  // exactly that reason — there is no code path from here to it.
  std::size_t EvictBeyond(std::size_t keep) override {
    // The header documents kDefaultLettersKeep as the default; a virtual with
    // no default argument can only say so by reading 0 as "unspecified".
    // Emptying the store is not something a caller asks for by accident.
    if (keep == 0) keep = kDefaultLettersKeep;
    const std::vector<const Meta*> ordered = Ordered();
    if (ordered.size() <= keep) return 0;
    std::vector<std::string> doomed;
    for (std::size_t i = keep; i < ordered.size(); ++i) doomed.push_back(ordered[i]->id);
    std::size_t removed = 0;
    for (const std::string& id : doomed) {
      if (!fsutil::Remove(PathFor(id))) continue;
      index_.erase(std::remove_if(index_.begin(), index_.end(),
                                  [&id](const Meta& m) { return m.id == id; }),
                   index_.end());
      ++removed;
    }
    return removed;
  }

  std::size_t Count() const override { return index_.size(); }

 private:
  struct Meta {
    std::string id;
    std::int64_t date = 0;
    bool unread = true;
  };

  static Meta MetaOf(const wire::Letter& l, bool unread) {
    Meta m;
    m.id = l.id;
    m.date = l.date;
    m.unread = unread;
    return m;
  }

  std::string PathFor(const std::string& id) const { return fsutil::Join(dir_, id); }

  void Scan() {
    fsutil::MkdirAll(dir_);
    std::vector<std::string> names;
    if (!fsutil::ListNames(dir_, names)) return;
    for (const std::string& name : names) {
      if (!fsutil::NameIsSafe(name)) continue;
      StoredLetter sl;
      // An unreadable or malformed file is skipped, never fatal: the server
      // re-delivers anything the device does not have (server §4.5). A file
      // whose record disagrees with its name is skipped for the same reason —
      // the index addresses letters by name, so the two must agree.
      if (!Load(name, sl) || sl.letter.id != name) continue;
      index_.push_back(MetaOf(sl.letter, sl.unread));
    }
  }

  bool Load(const std::string& id, StoredLetter& out) const {
    std::string text;
    if (!fsutil::ReadAll(PathFor(id), text)) return false;
    return DecodeLetter(text, out);
  }

  Meta* FindMeta(const std::string& id) {
    for (Meta& m : index_) {
      if (m.id == id) return &m;
    }
    return nullptr;
  }

  const Meta* FindMeta(const std::string& id) const {
    return const_cast<FileLetterStore*>(this)->FindMeta(id);
  }

  // Newest first, ties broken by id so the order is total and identical on
  // every boot — an inbox that reshuffles itself is a bug report.
  std::vector<const Meta*> Ordered() const {
    std::vector<const Meta*> ordered;
    ordered.reserve(index_.size());
    for (const Meta& m : index_) ordered.push_back(&m);
    std::sort(ordered.begin(), ordered.end(), [](const Meta* a, const Meta* b) {
      if (a->date != b->date) return a->date > b->date;
      return a->id > b->id;
    });
    return ordered;
  }

  const std::string dir_;
  std::vector<Meta> index_;
};

class FileOutbox final : public Outbox {
 public:
  FileOutbox(std::string dir, std::string counter_path)
      : dir_(std::move(dir)), counter_path_(std::move(counter_path)) {
    fsutil::MkdirAll(dir_);
    LoadCounter();
    Scan();
  }

  // §5.1, C-5: the counter is persisted BEFORE the entry. A crash in between
  // burns a local id, which costs nothing; the opposite order would reuse one,
  // and the server's 4096-entry ack ring would then alias two different
  // letters (server §4.7).
  bool Add(const std::string& contact_id, const std::string& subject,
           const std::string& body, std::int64_t composed_at,
           std::string& out_local_id) override {
    // B.9: at the cap the UI parks the child's text as the draft and shows
    // "the bag is full" — refusing to let a kid write is never the right
    // failure, so the refusal happens here and the words survive up there.
    //
    // The cap counts SENDABLE entries only. It models the runner's bag: a
    // terminally rejected letter is not waiting for the runner, it is waiting
    // for the child (§5.4). Counting rejects would turn a stuck state into a
    // lockout — twelve undismissed rejects would block composing forever, since
    // only the child clears them — and would make the on-screen explanation a
    // lie, because syncing cannot clear a reject.
    if (SendableCount() >= kOutboxCap) return false;
    const std::uint64_t next = high_water_ + 1;
    if (!fsutil::WriteAtomic(counter_path_, std::to_string(next) + "\n")) return false;
    high_water_ = next;

    OutboxEntry e;
    e.local_id = FormatLocalId(next);
    e.contact_id = contact_id;
    e.subject = subject;
    e.body = body;
    e.composed_at = composed_at;
    if (!fsutil::WriteAtomic(PathFor(e.local_id), Encode(e))) return false;
    out_local_id = e.local_id;
    entries_.push_back(std::move(e));
    return true;
  }

  std::vector<OutboxEntry> Sendable() const override {
    std::vector<OutboxEntry> out;
    for (const OutboxEntry& e : Ordered()) {
      if (!e.rejected) out.push_back(e);
    }
    return out;
  }

  std::vector<OutboxEntry> All() const override { return Ordered(); }

  // §5.4 / D-5: every ack status is terminal. `sent` removes the entry; a
  // reject keeps the child's text so one key can reopen it as a draft.
  bool Resolve(const std::string& local_id, wire::AckStatus s) override {
    OutboxEntry* e = Find(local_id);
    // A replayed ack for an already-resolved letter is not an error: the
    // device may have applied it and crashed before the cursor (§5.2 step 6).
    if (e == nullptr) return true;
    if (s == wire::AckStatus::kSent) return Erase(local_id);
    e->rejected = true;  // kSent is the only non-reject status (wire.h)
    e->reject_status = s;
    return fsutil::WriteAtomic(PathFor(local_id), Encode(*e));
  }

  bool Discard(const std::string& local_id) override {
    if (Find(local_id) == nullptr) return false;
    return Erase(local_id);
  }

  std::size_t SendableCount() const override {
    std::size_t n = 0;
    for (const OutboxEntry& e : entries_) {
      if (!e.rejected) ++n;
    }
    return n;
  }

 private:
  std::string PathFor(const std::string& local_id) const {
    return fsutil::Join(dir_, local_id);
  }

  static std::string Encode(const OutboxEntry& e) {
    const Record r = {
        {"local_id", e.local_id},
        {"contact_id", e.contact_id},
        {"subject", e.subject},
        {"body", e.body},
        {"composed_at", std::to_string(e.composed_at)},
        {"rejected", Bit(e.rejected)},
        {"reject_status", std::to_string(static_cast<int>(e.reject_status))},
    };
    return fsutil::EncodeRecord(r);
  }

  static bool Decode(std::string_view text, OutboxEntry& out) {
    const Record r = fsutil::DecodeRecord(text);
    const std::string* id = fsutil::Find(r, "local_id");
    if (id == nullptr || id->empty()) return false;
    out.local_id = *id;
    if (const std::string* v = fsutil::Find(r, "contact_id")) out.contact_id = *v;
    if (const std::string* v = fsutil::Find(r, "subject")) out.subject = *v;
    if (const std::string* v = fsutil::Find(r, "body")) out.body = *v;
    fsutil::ReadI64(r, "composed_at", out.composed_at);
    fsutil::ReadBool(r, "rejected", out.rejected);
    int status = 0;
    if (fsutil::ReadInt(r, "reject_status", status) && status >= 0 &&
        status <= static_cast<int>(wire::AckStatus::kUnknown)) {
      out.reject_status = static_cast<wire::AckStatus>(status);
    }
    return true;
  }

  void LoadCounter() {
    std::string text;
    if (fsutil::ReadAll(counter_path_, text)) ParseCounter(text, high_water_);
  }

  void Scan() {
    std::vector<std::string> names;
    if (!fsutil::ListNames(dir_, names)) return;
    for (const std::string& name : names) {
      if (!fsutil::NameIsSafe(name)) continue;
      std::string text;
      OutboxEntry e;
      if (!fsutil::ReadAll(fsutil::Join(dir_, name), text) || !Decode(text, e)) continue;
      if (e.local_id != name) continue;  // the entry is addressed by its name
      entries_.push_back(std::move(e));
    }
  }

  std::vector<OutboxEntry> Ordered() const {
    std::vector<OutboxEntry> out = entries_;
    std::sort(out.begin(), out.end(), [](const OutboxEntry& a, const OutboxEntry& b) {
      return a.local_id < b.local_id;  // zero-padded, so lexical order is age
    });
    return out;
  }

  OutboxEntry* Find(const std::string& local_id) {
    for (OutboxEntry& e : entries_) {
      if (e.local_id == local_id) return &e;
    }
    return nullptr;
  }

  bool Erase(const std::string& local_id) {
    if (!fsutil::Remove(PathFor(local_id))) return false;
    entries_.erase(std::remove_if(entries_.begin(), entries_.end(),
                                  [&local_id](const OutboxEntry& e) {
                                    return e.local_id == local_id;
                                  }),
                   entries_.end());
    return true;
  }

  const std::string dir_;
  const std::string counter_path_;
  std::uint64_t high_water_ = 0;
  std::vector<OutboxEntry> entries_;
};

// FileSeenRing is append-mostly: one short line per letter, compacted only once
// the file has drifted past the ring by more than the slack. Rewriting 1000 ids
// on every letter would be a 12 KB flash write to record 13 bytes.
class FileSeenRing final : public SeenRing {
 public:
  FileSeenRing(std::string path, std::size_t capacity)
      // server §4.5 states a floor, not a suggestion: "at least 1000".
      : path_(std::move(path)), capacity_(std::max(capacity, kMinSeenIds)) {
    Load();
  }

  // A linear scan over 1000 short ids costs microseconds and no extra memory;
  // a hash index would cost ~50 KB of RAM to save them. On a part whose whole
  // job is one sync every few hours, that is the wrong way round.
  bool Contains(const std::string& letter_id) const override {
    return std::find(ring_.begin(), ring_.end(), letter_id) != ring_.end();
  }

  bool Add(const std::string& letter_id) override {
    if (!fsutil::NameIsSafe(letter_id)) return false;
    if (Contains(letter_id)) return true;
    ring_.push_back(letter_id);
    if (ring_.size() > capacity_) ring_.pop_front();
    if (lines_on_disk_ + 1 > capacity_ + Slack()) return Compact();
    if (!fsutil::AppendLine(path_, letter_id)) return false;
    ++lines_on_disk_;
    return true;
  }

  std::size_t Capacity() const override { return capacity_; }

 private:
  std::size_t Slack() const { return capacity_ / 8; }

  void Load() {
    std::string text;
    if (!fsutil::ReadAll(path_, text)) return;
    // A final line with no newline is a torn append (fsutil::AppendLine) and is
    // dropped. Losing a seen id costs at most one re-delivered letter, which
    // Put absorbs idempotently; keeping half an id would cost more.
    const std::vector<std::string> lines = fsutil::SplitCompleteLines(text);
    lines_on_disk_ = lines.size();
    const std::size_t start = lines.size() > capacity_ ? lines.size() - capacity_ : 0;
    for (std::size_t i = start; i < lines.size(); ++i) {
      if (lines[i].empty() || Contains(lines[i])) continue;
      ring_.push_back(lines[i]);
    }
    if (lines_on_disk_ > capacity_ + Slack()) Compact();
  }

  bool Compact() {
    std::string text;
    for (const std::string& id : ring_) {
      text += id;
      text += '\n';
    }
    if (!fsutil::WriteAtomic(path_, text)) return false;
    lines_on_disk_ = ring_.size();
    return true;
  }

  const std::string path_;
  const std::size_t capacity_;
  std::size_t lines_on_disk_ = 0;
  std::deque<std::string> ring_;
};

class FileStateStore final : public StateStore {
 public:
  FileStateStore(std::string path, std::string counter_path)
      : path_(std::move(path)), counter_path_(std::move(counter_path)) {
    std::string text;
    if (!fsutil::ReadAll(path_, text)) return;
    const Record r = fsutil::DecodeRecord(text);
    if (const std::string* v = fsutil::Find(r, "cursor")) state_.cursor = *v;
    fsutil::ReadInt(r, "ayllu_version", state_.ayllu_version);
    fsutil::ReadI64(r, "last_sync_at", state_.last_sync_at);
  }

  SyncState Snapshot() const override {
    SyncState s = state_;
    // The Outbox is the only writer of the high-water mark — C-5 depends on a
    // single writer — so this reads it fresh rather than caching a copy that
    // would be behind the id a just-composed letter already burned.
    std::string text;
    if (fsutil::ReadAll(counter_path_, text)) ParseCounter(text, s.local_id_high_water);
    return s;
  }

  // §5.2 step 6: the cursor is written LAST, after the letters it covers are
  // durable. A crash before this leaves the old cursor standing, the server
  // re-delivers, and the seen ring absorbs the repeats.
  bool SetCursor(const std::string& cursor) override {
    std::string previous = state_.cursor;
    state_.cursor = cursor;
    if (Persist()) return true;
    state_.cursor = std::move(previous);
    return false;
  }

  bool SetAylluVersion(int v) override {
    const int previous = state_.ayllu_version;
    state_.ayllu_version = v;
    if (Persist()) return true;
    state_.ayllu_version = previous;
    return false;
  }

  bool SetLastSyncAt(std::int64_t t) override {
    const std::int64_t previous = state_.last_sync_at;
    state_.last_sync_at = t;
    if (Persist()) return true;
    state_.last_sync_at = previous;
    return false;
  }

 private:
  // The whole record is rewritten every time, so no setter can drop a field
  // another setter owns. On failure the caller's in-memory view is rolled back
  // to what is actually on flash.
  bool Persist() const {
    const Record r = {
        {"cursor", state_.cursor},
        {"ayllu_version", std::to_string(state_.ayllu_version)},
        {"last_sync_at", std::to_string(state_.last_sync_at)},
    };
    return fsutil::WriteAtomic(path_, fsutil::EncodeRecord(r));
  }

  const std::string path_;
  const std::string counter_path_;
  SyncState state_;
};

}  // namespace

std::unique_ptr<LetterStore> OpenLetterStore(const std::string& root) {
  fsutil::MkdirAll(root);
  return std::make_unique<FileLetterStore>(fsutil::Join(root, kLettersDir));
}

std::unique_ptr<Outbox> OpenOutbox(const std::string& root) {
  fsutil::MkdirAll(root);
  return std::make_unique<FileOutbox>(fsutil::Join(root, kOutboxDir),
                                      fsutil::Join(root, kLocalIdFile));
}

std::unique_ptr<SeenRing> OpenSeenRing(const std::string& root, std::size_t capacity) {
  fsutil::MkdirAll(root);
  return std::make_unique<FileSeenRing>(fsutil::Join(root, kSeenFile), capacity);
}

std::unique_ptr<StateStore> OpenStateStore(const std::string& root) {
  fsutil::MkdirAll(root);
  return std::make_unique<FileStateStore>(fsutil::Join(root, kStateFile),
                                          fsutil::Join(root, kLocalIdFile));
}

}  // namespace chaski::store
