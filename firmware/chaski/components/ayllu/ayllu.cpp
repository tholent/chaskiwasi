// Implementation of components/ayllu — see include/chaski/ayllu.h for the
// contract and the spec clauses it must satisfy.
//
// Two files under `root`, never one (client §4.4):
//
//   ayllu          the server's snapshot: version + contacts, membership truth
//   ayllu_overlay  the child's decoration, keyed by contact id
//
// The split is the whole point of B.3. A snapshot arrives whenever the guardian
// changes the list and replaces membership wholesale; if decoration lived in
// the same file, every guardian edit would silently erase the child's nicknames
// and ordering. Separate files make that impossible rather than unlikely.
//
// The overlay never leaves the device: there is no device->server mutation on
// this wire, and adding engagement state to the server is a spec reversal
// (server §1.1).
#include "chaski/ayllu.h"

#include <algorithm>
#include <string_view>
#include <utility>

#include "chaski/fsutil.h"

namespace chaski::ayllu {
namespace {

using fsutil::Record;

constexpr char kSnapshotFile[] = "ayllu";
constexpr char kOverlayFile[] = "ayllu_overlay";

// A repeated "contact.id" opens a new record; the keys after it belong to it.
// Same shape for the overlay, where an absent key means "unset" — which is what
// makes a guardian's rename still reach the child (client §4.4).
constexpr char kContactId[] = "contact.id";
constexpr char kOverlayId[] = "overlay.id";

std::string Bit(bool b) { return b ? "1" : "0"; }

bool ParseBit(const std::string& s, bool& out) {
  if (s != "0" && s != "1") return false;
  out = s == "1";
  return true;
}

bool ParseInt(const std::string& s, int& out) {
  const Record r = {{"n", s}};
  return fsutil::ReadInt(r, "n", out);
}

std::string EncodeSnapshot(const wire::Ayllu& a) {
  Record r = {{"version", std::to_string(a.version)}};
  for (const wire::AylluContact& c : a.contacts) {
    r.emplace_back(kContactId, c.id);
    r.emplace_back("contact.name", c.name);
    r.emplace_back("contact.active", Bit(c.active));
    r.emplace_back("contact.pinned", Bit(c.pinned));
    r.emplace_back("contact.order", std::to_string(c.order));
    r.emplace_back("contact.portrait", c.portrait);
  }
  return fsutil::EncodeRecord(r);
}

void DecodeSnapshot(std::string_view text, wire::Ayllu& out) {
  const Record r = fsutil::DecodeRecord(text);
  fsutil::ReadInt(r, "version", out.version);
  for (const auto& [key, value] : r) {
    if (key == kContactId) {
      wire::AylluContact c;
      c.id = value;
      out.contacts.push_back(std::move(c));
      continue;
    }
    if (out.contacts.empty()) continue;  // a stray field before any contact
    wire::AylluContact& c = out.contacts.back();
    if (key == "contact.name") {
      c.name = value;
    } else if (key == "contact.active") {
      ParseBit(value, c.active);
    } else if (key == "contact.pinned") {
      ParseBit(value, c.pinned);
    } else if (key == "contact.order") {
      ParseInt(value, c.order);
    } else if (key == "contact.portrait") {
      c.portrait = value;
    }
  }
}

using OverlayRow = std::pair<std::string, Overlay>;

std::string EncodeOverlays(const std::vector<OverlayRow>& rows) {
  Record r;
  for (const auto& [id, o] : rows) {
    r.emplace_back(kOverlayId, id);
    if (o.nickname) r.emplace_back("overlay.nickname", *o.nickname);
    if (o.pinned) r.emplace_back("overlay.pinned", Bit(*o.pinned));
    if (o.order) r.emplace_back("overlay.order", std::to_string(*o.order));
    if (o.portrait) r.emplace_back("overlay.portrait", *o.portrait);
  }
  return fsutil::EncodeRecord(r);
}

std::vector<OverlayRow> DecodeOverlays(std::string_view text) {
  std::vector<OverlayRow> rows;
  for (const auto& [key, value] : fsutil::DecodeRecord(text)) {
    if (key == kOverlayId) {
      rows.emplace_back(value, Overlay{});
      continue;
    }
    if (rows.empty()) continue;
    Overlay& o = rows.back().second;
    if (key == "overlay.nickname") {
      o.nickname = value;
    } else if (key == "overlay.pinned") {
      bool b = false;
      if (ParseBit(value, b)) o.pinned = b;
    } else if (key == "overlay.order") {
      int n = 0;
      if (ParseInt(value, n)) o.order = n;
    } else if (key == "overlay.portrait") {
      o.portrait = value;
    }
  }
  return rows;
}

class FileStore final : public Store {
 public:
  explicit FileStore(const std::string& root)
      : snapshot_path_(fsutil::Join(root, kSnapshotFile)),
        overlay_path_(fsutil::Join(root, kOverlayFile)) {
    fsutil::MkdirAll(root);
    std::string text;
    if (fsutil::ReadAll(snapshot_path_, text)) DecodeSnapshot(text, snapshot_);
    if (fsutil::ReadAll(overlay_path_, text)) overlays_ = DecodeOverlays(text);
  }

  // §4.4: the snapshot replaces membership wholesale on every version change.
  // It writes only the snapshot file, so decoration cannot be collateral
  // damage; contacts that vanish keep their overlay row, which is why a re-add
  // restores the child's nickname (server §7.2 reuses the original id).
  bool ApplySnapshot(const wire::Ayllu& a) override {
    if (!fsutil::WriteAtomic(snapshot_path_, EncodeSnapshot(a))) return false;
    snapshot_ = a;
    return true;
  }

  int Version() const override { return snapshot_.version; }

  // §7.2: read-time resolution sees the full table, tombstones included, so an
  // old letter still renders Rosa's name after she is deactivated.
  std::vector<Contact> Merged() const override {
    std::vector<Contact> out;
    out.reserve(snapshot_.contacts.size());
    for (const wire::AylluContact& c : snapshot_.contacts) out.push_back(Merge(c));
    std::sort(out.begin(), out.end(), ChildOrder);
    return out;
  }

  // §11.2 / server A.15: the compose picker sees active contacts only, which
  // is what keeps tombstones and c_sys out of it — c_sys ships as a tombstone
  // precisely so no special case is needed. The explicit id test is a backstop
  // for the day a server bug ships it active; writing to Wasi is never a thing
  // a child can do.
  std::vector<Contact> Composable() const override {
    std::vector<Contact> out;
    for (const wire::AylluContact& c : snapshot_.contacts) {
      if (!c.active || c.id == wire::kSysContactId) continue;
      out.push_back(Merge(c));
    }
    std::sort(out.begin(), out.end(), ChildOrder);
    return out;
  }

  // C-14: a miss is not an error. `out` comes back carrying the id and nothing
  // else, so a caller that ignores the return value renders a blank label
  // rather than a raw contact id — and the letter is kept either way.
  bool Lookup(const std::string& id, Contact& out) const override {
    for (const wire::AylluContact& c : snapshot_.contacts) {
      if (c.id != id) continue;
      out = Merge(c);
      return true;
    }
    out = Contact{};
    out.id = id;
    return false;
  }

  // Decoration is device-local and unknown ids are accepted: a snapshot that
  // reintroduces someone should find their nickname already waiting (§4.4).
  bool SetOverlay(const std::string& id, const Overlay& o) override {
    if (id.empty()) return false;
    for (OverlayRow& row : overlays_) {
      if (row.first != id) continue;
      const Overlay previous = row.second;
      row.second = o;
      if (Persist()) return true;
      row.second = previous;
      return false;
    }
    overlays_.emplace_back(id, o);
    if (Persist()) return true;
    overlays_.pop_back();
    return false;
  }

  // Clearing decoration writes the overlay file alone: membership is the
  // server's and nothing the child does here may touch it (I-4).
  bool ClearOverlay(const std::string& id) override {
    const auto it = std::find_if(overlays_.begin(), overlays_.end(),
                                 [&id](const OverlayRow& r) { return r.first == id; });
    if (it == overlays_.end()) return true;  // already clear
    const OverlayRow removed = *it;
    overlays_.erase(it);
    if (Persist()) return true;
    overlays_.push_back(removed);
    return false;
  }

 private:
  const Overlay* OverlayFor(const std::string& id) const {
    for (const OverlayRow& row : overlays_) {
      if (row.first == id) return &row.second;
    }
    return nullptr;
  }

  Contact Merge(const wire::AylluContact& c) const {
    Contact m;
    m.id = c.id;
    m.server_name = c.name;
    m.name = c.name;
    m.active = c.active;
    m.pinned = c.pinned;
    m.order = c.order;
    m.portrait = c.portrait;
    const Overlay* o = OverlayFor(c.id);
    if (o == nullptr) return m;
    // Server values are guardian-set defaults; an unset overlay field leaves
    // them standing, which is how a guardian's rename still shows up (B.3).
    if (o->nickname && !o->nickname->empty()) m.name = *o->nickname;
    if (o->pinned) m.pinned = *o->pinned;
    if (o->order) m.order = *o->order;
    if (o->portrait) m.portrait = *o->portrait;
    return m;
  }

  // §11.2: pinned first, then the child's order, then name. Ties break on id
  // so the picker is in the same order every time it opens.
  static bool ChildOrder(const Contact& a, const Contact& b) {
    if (a.pinned != b.pinned) return a.pinned;
    if (a.order != b.order) return a.order < b.order;
    if (a.name != b.name) return a.name < b.name;
    return a.id < b.id;
  }

  bool Persist() const { return fsutil::WriteAtomic(overlay_path_, EncodeOverlays(overlays_)); }

  const std::string snapshot_path_;
  const std::string overlay_path_;
  wire::Ayllu snapshot_;
  std::vector<OverlayRow> overlays_;
};

}  // namespace

// C-14: the label itself lives in chaski_strings.c, which this component must
// not include — components own logic, main/ owns words (client §0). Returning
// the id lets the UI resolve the text without a literal escaping the table.
const char* FallbackLabelKey() { return "STR_CONTACT_UNKNOWN"; }

std::unique_ptr<Store> Open(const std::string& root) {
  return std::make_unique<FileStore>(root);
}

}  // namespace chaski::ayllu
