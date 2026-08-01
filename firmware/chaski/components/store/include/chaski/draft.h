// Drafts never die (client §11.3).
//
// The in-progress composition is autosaved on every wipe trigger and every
// ~30 s of typing, so a timeout, a put-away key, or a flat battery mid-sentence
// costs the child nothing. This is D-5's promise — an unacked letter is never
// dropped — extended to words that have not been sent yet.
//
// One slot in v1, and the slot is the reason this is an API rather than a
// std::string in the UI: starting a new letter while a draft is pending is a
// question the child answers (STR_DRAFT_CONFLICT_PROMPT), never a silent
// overwrite. Start() refuses; Save() is the autosave path for the draft that
// is already open.
//
// Same portability and write discipline as the rest of components/store: POSIX
// only, every write atomic through fsutil (implementation-plan ground rule 3).
//
// F-C7: §4.1 names `draft` in the storage model but the Wave 0 scaffold
// declared no API for it, so C-17 had no owner. This is that API.
#pragma once

#include <cstdint>
#include <memory>
#include <string>

namespace chaski::store {

// Draft is what the compose screen holds. `contact_id` may be empty: the
// person is picked first (client §11.2), but the child can abandon the picker
// with text already typed after a recompose, and losing that text would be the
// exact failure §11.3 forbids.
struct Draft {
  std::string contact_id;
  std::string subject;
  std::string body;
  std::int64_t updated_at = 0;  // 0 while the clock is not yet valid (§5.6)
};

// StartOutcome is the decision point of §11.3. kDraftPending means the UI must
// ask which draft to keep; it is not an error.
enum class StartOutcome {
  kStarted,
  kDraftPending,
  kWriteFailed,
};

class DraftStore {
 public:
  virtual ~DraftStore() = default;

  // Pending reports whether there is a draft worth offering on the next wake.
  // An empty or undecodable slot is not pending: a draft that cannot be read
  // must not lock the child out of writing, the same reasoning that keeps
  // rejected letters from filling the outbox cap (B.9).
  virtual bool Pending() const = 0;

  virtual bool Load(Draft& out) const = 0;

  // Start refuses to overwrite a pending draft. The caller resolves the
  // conflict — keep writing (Load) or start new (Discard then Start).
  virtual StartOutcome Start(const Draft& d) = 0;

  // Save is the autosave path: it replaces the slot unconditionally, because
  // the draft being written is the draft that is already open. Atomic, so a
  // power cut mid-save leaves the previous autosave intact rather than a
  // half-written one (client §4).
  virtual bool Save(const Draft& d) = 0;

  // Discard clears the slot. Called when the letter reaches the outbox, and
  // when the child chooses "start new" at the conflict prompt.
  virtual bool Discard() = 0;
};

// Autosave cadence (client §11.3). The other trigger is every wipe — timeout,
// put-away, and the low-battery paths alike — which the UI drives; this is the
// idle-typing floor between them. Every save is a flash write, and flash
// writes are what this device spends its wear and battery budget on (design
// §6.4), so the number is a compromise and not a tuning knob.
inline constexpr int kDraftAutosaveIntervalMs = 30000;

std::unique_ptr<DraftStore> OpenDraftStore(const std::string& root);

}  // namespace chaski::store
