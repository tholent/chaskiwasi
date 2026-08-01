// Implementation of the draft slot — see include/chaski/draft.h.
//
// One file, `draft`, in the store root. It is replaced whole on every save:
// the record is at most a letter's worth of text, and a whole-file atomic
// replace is the only shape that cannot leave a torn draft behind (client §4).
//
// Nothing here logs. This file holds the child's unsent words, which are
// letter content by any honest reading of D-7.
#include "chaski/draft.h"

#include <utility>

#include "chaski/fsutil.h"

namespace chaski::store {
namespace {

using fsutil::Record;

constexpr char kDraftFile[] = "draft";

std::string Encode(const Draft& d) {
  // body last, as in the letter record: everything cheap sits ahead of the one
  // expensive field.
  const Record r = {
      {"contact_id", d.contact_id},
      {"subject", d.subject},
      {"updated_at", std::to_string(d.updated_at)},
      {"body", d.body},
  };
  return fsutil::EncodeRecord(r);
}

// Decode returns false for anything that is not a usable draft. "Usable" means
// there is something the child wrote: a slot holding only an empty body is
// nothing to offer them on the next wake, and offering it would turn every
// abandoned picker into a prompt.
bool Decode(const std::string& text, Draft& out) {
  const Record r = fsutil::DecodeRecord(text);
  Draft d;
  if (const std::string* v = fsutil::Find(r, "contact_id")) d.contact_id = *v;
  if (const std::string* v = fsutil::Find(r, "subject")) d.subject = *v;
  if (const std::string* v = fsutil::Find(r, "body")) d.body = *v;
  fsutil::ReadI64(r, "updated_at", d.updated_at);
  if (d.body.empty() && d.subject.empty()) return false;
  out = std::move(d);
  return true;
}

class FileDraftStore final : public DraftStore {
 public:
  explicit FileDraftStore(std::string path) : path_(std::move(path)) {
    Draft d;
    pending_ = ReadSlot(d);
  }

  bool Pending() const override { return pending_; }

  bool Load(Draft& out) const override { return ReadSlot(out); }

  StartOutcome Start(const Draft& d) override {
    if (pending_) return StartOutcome::kDraftPending;
    if (!Save(d)) return StartOutcome::kWriteFailed;
    return StartOutcome::kStarted;
  }

  bool Save(const Draft& d) override {
    if (!fsutil::WriteAtomic(path_, Encode(d))) return false;
    pending_ = !d.body.empty() || !d.subject.empty();
    return true;
  }

  bool Discard() override {
    // A slot that was never written is already discarded; Remove reporting
    // "no such file" must not read as a failure to the UI.
    if (fsutil::Exists(path_) && !fsutil::Remove(path_)) return false;
    pending_ = false;
    return true;
  }

 private:
  bool ReadSlot(Draft& out) const {
    std::string text;
    if (!fsutil::ReadAll(path_, text)) return false;
    return Decode(text, out);
  }

  std::string path_;
  bool pending_ = false;
};

}  // namespace

std::unique_ptr<DraftStore> OpenDraftStore(const std::string& root) {
  fsutil::MkdirAll(root);
  return std::unique_ptr<DraftStore>(
      new FileDraftStore(fsutil::Join(root, kDraftFile)));
}

}  // namespace chaski::store
