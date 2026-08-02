// The screens of client §11, as a state machine.
//
// Three rules shape everything below, and each of them is a promise the
// product makes rather than an implementation choice:
//
//   - A person is chosen before any text exists (design §4, §11.2). There is
//     no To: field and no way to reach the writing screen without a contact
//     id, which is why the picker is a separate Screen and not a field.
//   - Nothing the child wrote is ever lost (§11.3, D-5). Every path that
//     leaves the writing screen saves first, a rejected letter keeps its text,
//     and the one draft slot asks rather than overwrites.
//   - The device never explains a reject (§5.4). "Ask your guardians" is the
//     whole message; the distinctions between rejected_inactive,
//     rejected_unknown_contact and invalid exist for guardians, server-side.
//
// D-7 applies to this file absolutely: no letter body, subject, sender name or
// draft text is logged at any level, in any build. There are no log calls here
// at all, which is the cheapest way to keep that true.
#include "chaski/ui.h"

#include <algorithm>
#include <utility>

#include "chaski/wire.h"
#include "chaski_strings.h"
#include "text_util.h"

namespace chaski::ui {
namespace {

// Refresh names the waveform this update deserves (client §8.3). Typing is
// partial and batched; a page turn or a screen change is fast; a reading
// screen after enough partials is full, because ghosting is a privacy
// parameter on this device and not a cosmetic one (design §11).
enum class Refresh { kPartial, kFast, kFull };

// PendingKind records what the child was trying to do when the one draft slot
// turned out to be occupied (§11.3).
enum class PendingKind {
  kNone,
  kResume,      // woke with a draft; "keep writing the letter you started?"
  kNewLetter,   // picked a person while a different draft was open
  kRecompose,   // reopening a rejected letter (§5.4, D-5)
};

struct Pending {
  PendingKind kind = PendingKind::kNone;
  std::string contact_id;
  std::string subject;
  std::string body;
  std::string local_id;  // the outbox entry to retire, for kRecompose
};

// The four top-level screens, cycled left/right. Everything else is entered
// from one of them and returns to it.
constexpr Screen kTabs[] = {Screen::kInbox, Screen::kComposePick,
                            Screen::kOutbox, Screen::kSettings};
constexpr int kTabCount = 4;

// One selection index per screen, so leaving a list and coming back does not
// silently move what the child was pointing at.
constexpr int kScreenCount = static_cast<int>(Screen::kSettingsAbout) + 1;

// Nicknames are decoration, not identity: a bound keeps one from pushing the
// letter list off its own row (§4.4).
constexpr std::size_t kMaxNicknameGraphemes = 24;

// Signal, in words. 0 dBm is not a reading any modem produces, so it stands
// for "we have not been told" and reads as no signal rather than a perfect one.
int SignalStringId(int rssi) {
  if (rssi == 0) return STR_LOG_SIGNAL_NONE;
  if (rssi >= -85) return STR_LOG_SIGNAL_GOOD;
  if (rssi >= -105) return STR_LOG_SIGNAL_WEAK;
  return STR_LOG_SIGNAL_NONE;
}

class AppImpl final : public App {
 public:
  explicit AppImpl(Deps d) : d_(std::move(d)) {}

  void Start(Screen initial) override {
    last_key_ms_ = Now();
    last_save_ms_ = last_key_ms_;

    // A pushed PIN outranks everything: nothing but the cover shows until it
    // is entered (§11.5). Sync and put-away still work — neither passes
    // through this class (§10).
    if (PinRequired()) {
      Go(Screen::kPin);
      Present(Refresh::kFull);
      return;
    }
    if (initial == Screen::kCover) {
      Go(Screen::kCover);
      Present(Refresh::kFull);
      return;
    }
    if (!OfferPendingDraft()) Go(initial);
    Present(Refresh::kFull);
  }

  void Key(const input::KeyEvent& e) override {
    last_key_ms_ = Now();
    // A message is feedback about the last action, so the next one clears it.
    if (e.key != input::Key::kNone) message_.clear();

    Refresh r = Refresh::kFast;
    switch (screen_) {
      case Screen::kCover: r = KeyCover(e); break;
      case Screen::kPin: r = KeyPin(e); break;
      case Screen::kInbox: r = KeyInbox(e); break;
      case Screen::kRead: r = KeyRead(e); break;
      case Screen::kComposePick: r = KeyPick(e); break;
      case Screen::kComposeWrite: r = KeyWrite(e); break;
      case Screen::kOutbox: r = KeyOutbox(e); break;
      case Screen::kSettings: r = KeySettings(e); break;
      case Screen::kFault: r = KeyFault(e); break;
      case Screen::kDraftConflict: r = KeyConflict(e); break;
      case Screen::kSettingsContacts: r = KeyContacts(e); break;
      case Screen::kSettingsLog:
      case Screen::kSettingsAbout: r = KeyLines(e); break;
    }
    // The model is always current; only the panel update is batched, so a
    // keystroke is never invisible to anything but the glass (§8.3).
    if (deferred_paint_) {
      Rebuild();
      return;
    }
    Present(r);
  }

  void Tick() override {
    const std::int64_t now = Now();

    // A PIN cleared in Wasi's config takes effect at the next background sync,
    // which happens on the timer whether or not the device is locked. This is
    // the whole recovery story, so it must work from inside the lock (§11.5).
    if (screen_ == Screen::kPin && !PinRequired()) {
      unlocked_ = true;
      OpenAfterUnlock();
      Present(Refresh::kFull);
      return;
    }

    if (draft_open_ && now - last_save_ms_ >= store::kDraftAutosaveIntervalMs) {
      AutosaveDraft();
    }

    // The batched compose refresh: never one partial per keystroke (§8.3).
    if (deferred_paint_ && now - last_edit_ms_ >= kComposeIdleRefreshMs) {
      deferred_paint_ = false;
      Present(Refresh::kPartial);
      return;
    }

    if (screen_ == Screen::kPin && backoff_until_ms_ != 0 &&
        now >= backoff_until_ms_) {
      backoff_until_ms_ = 0;
      Present(Refresh::kFast);
    }
  }

  Screen Current() const override { return screen_; }
  const View& CurrentView() const override { return view_; }

  void ShowFault(FaultKind k, const std::string& diagnostic) override {
    fault_ = k;
    diagnostic_ = diagnostic;
    if (k == FaultKind::kChargeMe) charge_me_ = true;
    Go(Screen::kFault);
    Present(Refresh::kFull);
  }

  // Called on every wipe trigger — timeout, put-away, low battery (§11.3).
  // Silent when there is nothing open: an empty save would replace a draft
  // from an earlier session with nothing, which is the one outcome this
  // function exists to prevent.
  void AutosaveDraft() override {
    if (!draft_open_ || d_.drafts == nullptr) return;
    store::Draft draft;
    draft.contact_id = contact_id_;
    draft.subject = subject_;
    draft.body = body_;
    draft.updated_at = ClockValid() ? NowEpoch() : 0;
    d_.drafts->Save(draft);
    last_save_ms_ = Now();
  }

  panel::CoverState Cover() const override {
    panel::CoverState s;
    s.kind = charge_me_ ? panel::CoverKind::kChargeMe : panel::CoverKind::kResting;
    // A boolean, from a count that never leaves this line (B.5).
    s.any_unread = d_.letters != nullptr && d_.letters->UnreadCount() > 0;
    if (d_.about) {
      const About a = d_.about();
      s.battery_pct = a.battery_pct;
      s.charging = a.charging;
    }
    return s;
  }

  bool IdleTimedOut() const override {
    return Now() - last_key_ms_ >= kInactivityTimeoutMs;
  }

 private:
  // ---- small helpers -------------------------------------------------------

  const char* T(int id) const { return d_.text != nullptr ? d_.text(id) : ""; }
  std::int64_t Now() const { return d_.monotonic_ms ? d_.monotonic_ms() : 0; }
  std::int64_t NowEpoch() const { return d_.now_epoch ? d_.now_epoch() : 0; }
  bool ClockValid() const { return d_.clock_valid && d_.clock_valid(); }

  bool PinRequired() const {
    if (unlocked_ || d_.settings == nullptr) return false;
    return d_.settings->Get().pin_enabled;
  }

  layout::Metrics Metrics() const {
    const int step = d_.settings != nullptr ? d_.settings->Get().font_step : 0;
    return layout::MetricsFor(step >= 1 ? layout::FontSize::kLarge
                                        : layout::FontSize::kSmall);
  }

  int& Sel() { return sel_[static_cast<int>(screen_)]; }
  int Sel() const { return sel_[static_cast<int>(screen_)]; }

  void Go(Screen s) {
    if (s != screen_) sel_[static_cast<int>(s)] = 0;
    screen_ = s;
    editing_ = false;
    if (s == Screen::kInbox) ReloadLetters();
    if (s == Screen::kOutbox) ReloadOutbox();
    if (s == Screen::kComposePick || s == Screen::kSettingsContacts) ReloadContacts();
  }

  void ReloadLetters() {
    letters_.clear();
    if (d_.letters != nullptr) letters_ = d_.letters->ListNewestFirst(store::kDefaultLettersKeep);
  }
  void ReloadOutbox() {
    queued_.clear();
    if (d_.outbox != nullptr) queued_ = d_.outbox->All();
  }
  void ReloadContacts() {
    picker_.clear();
    roster_.clear();
    if (d_.contacts == nullptr) return;
    picker_ = d_.contacts->Composable();
    roster_ = d_.contacts->Merged();
  }

  // C-13/C-14: a tombstone and the system contact still have names on their
  // old letters, and an id with no entry at all renders under the fallback
  // label — the letter is kept either way, never dropped for a lookup miss.
  std::string ContactLabel(const std::string& contact_id) const {
    ayllu::Contact c;
    if (d_.contacts != nullptr && d_.contacts->Lookup(contact_id, c) &&
        !c.name.empty()) {
      return c.name;
    }
    if (contact_id == wire::kSysContactId) return T(STR_SYSTEM_SENDER);
    return T(STR_CONTACT_UNKNOWN);
  }

  void Move(int delta, int count) {
    if (count <= 0) {
      Sel() = 0;
      return;
    }
    int n = Sel() + delta;
    if (n < 0) n = 0;
    if (n >= count) n = count - 1;
    Sel() = n;
  }

  void Tab(int delta) {
    int idx = 0;
    for (int i = 0; i < kTabCount; ++i) {
      if (kTabs[i] == screen_) idx = i;
    }
    idx = (idx + delta + kTabCount) % kTabCount;
    Go(kTabs[idx]);
  }

  // ---- drafts --------------------------------------------------------------

  bool OfferPendingDraft() {
    if (d_.drafts == nullptr || !d_.drafts->Pending()) return false;
    pending_ = Pending{};
    pending_.kind = PendingKind::kResume;
    Go(Screen::kDraftConflict);
    return true;
  }

  // StartDraft owns the §11.3 decision point. It never overwrites: a pending
  // draft turns the request into a question for the child.
  void StartDraft(const Pending& p) {
    if (d_.drafts == nullptr) {
      message_ = T(STR_STORAGE_FAILED);
      return;
    }
    store::Draft draft;
    draft.contact_id = p.contact_id;
    draft.subject = p.subject;
    draft.body = p.body;
    draft.updated_at = ClockValid() ? NowEpoch() : 0;

    switch (d_.drafts->Start(draft)) {
      case store::StartOutcome::kStarted:
        // Order matters for D-5: the text is in the draft slot before the
        // outbox entry that held it is retired, so no crash in between can
        // land on a moment where the child's words exist nowhere.
        if (p.kind == PendingKind::kRecompose && d_.outbox != nullptr) {
          d_.outbox->Discard(p.local_id);
        }
        OpenCompose(p.contact_id, p.subject, p.body);
        break;
      case store::StartOutcome::kDraftPending:
        pending_ = p;
        Go(Screen::kDraftConflict);
        break;
      case store::StartOutcome::kWriteFailed:
        message_ = T(STR_STORAGE_FAILED);
        break;
    }
  }

  void OpenCompose(const std::string& contact_id, const std::string& subject,
                   const std::string& body) {
    contact_id_ = contact_id;
    subject_ = subject;
    body_ = body;
    field_ = 0;
    draft_open_ = true;
    last_save_ms_ = Now();
    // §5.5: a pushed max_letter_chars takes effect for the NEXT composition,
    // so the cap is captured here and not read again while writing.
    compose_max_ = d_.settings != nullptr
                       ? static_cast<std::size_t>(std::max(0, d_.settings->Get().max_letter_chars))
                       : static_cast<std::size_t>(wire::kDefaultMaxLetterChars);
    // A resumed draft with no person yet goes back to the picker with its
    // words intact (§11.2 picks first; §11.3 loses nothing).
    Go(contact_id_.empty() ? Screen::kComposePick : Screen::kComposeWrite);
  }

  void CloseCompose() {
    contact_id_.clear();
    subject_.clear();
    body_.clear();
    draft_open_ = false;
    deferred_paint_ = false;
  }

  // ---- sending -------------------------------------------------------------

  void Send() {
    if (body_.empty()) {
      message_ = T(STR_COMPOSE_EMPTY);
      return;
    }
    if (d_.outbox == nullptr) {
      message_ = T(STR_STORAGE_FAILED);
      return;
    }
    // B.9: the cap counts letters waiting for the runner, and refusing to let
    // a child write is never the right failure — the finished letter parks as
    // the draft instead.
    if (d_.outbox->SendableCount() >= store::kOutboxCap) {
      AutosaveDraft();
      message_ = T(STR_OUTBOX_FULL);
      Go(Screen::kOutbox);
      return;
    }
    std::string local_id;
    // §5.6: an invalid clock stamps 0 rather than a wrong time; the server
    // stamps the letter on receipt anyway.
    const std::int64_t at = ClockValid() ? NowEpoch() : 0;
    if (!d_.outbox->Add(contact_id_, subject_, body_, at, local_id)) {
      message_ = T(STR_STORAGE_FAILED);
      return;
    }
    if (d_.drafts != nullptr) d_.drafts->Discard();
    CloseCompose();
    message_ = T(STR_COMPOSE_SENT);
    Go(Screen::kOutbox);
  }

  // ---- per-screen keys -----------------------------------------------------

  Refresh KeyCover(const input::KeyEvent& e) {
    // Any key opens the session — unless a PIN is pushed, in which case the
    // PIN screen is what "opening" means (§11.5).
    if (e.key == input::Key::kNone) return Refresh::kFast;
    if (PinRequired()) {
      Go(Screen::kPin);
      return Refresh::kFull;
    }
    if (!OfferPendingDraft()) Go(Screen::kInbox);
    return Refresh::kFull;
  }

  Refresh KeyPin(const input::KeyEvent& e) {
    // Backoff prices guessing (§11.5, B.4). It never destroys anything and
    // never locks out permanently: flash encryption already protects the
    // letters, and a forgotten PIN three states away must not brick the inbox.
    if (backoff_until_ms_ != 0 && Now() < backoff_until_ms_) {
      message_ = T(STR_PIN_TRY_AGAIN_SOON);
      return Refresh::kFast;
    }
    backoff_until_ms_ = 0;

    switch (e.key) {
      case input::Key::kChar:
        if (e.codepoint >= '0' && e.codepoint <= '9' &&
            pin_entry_.size() < kPinMaxDigits) {
          pin_entry_.push_back(static_cast<char>(e.codepoint));
        }
        break;
      case input::Key::kBack:
        if (!pin_entry_.empty()) pin_entry_.pop_back();
        break;
      case input::Key::kEnter: {
        const bool ok = d_.verify_pin && d_.verify_pin(pin_entry_);
        pin_entry_.clear();
        if (ok) {
          unlocked_ = true;
          wrong_attempts_ = 0;
          backoff_until_ms_ = 0;
          OpenAfterUnlock();
          return Refresh::kFull;
        }
        ++wrong_attempts_;
        backoff_until_ms_ = Now() + BackoffMs(wrong_attempts_);
        message_ = T(STR_PIN_WRONG);
        break;
      }
      default:
        break;
    }
    return Refresh::kFast;
  }

  static int BackoffMs(int attempts) {
    long long ms = kPinBackoffFirstMs;
    for (int i = 1; i < attempts && ms < kPinBackoffMaxMs; ++i) ms *= 2;
    return static_cast<int>(std::min<long long>(ms, kPinBackoffMaxMs));
  }

  void OpenAfterUnlock() {
    if (!OfferPendingDraft()) Go(Screen::kInbox);
  }

  Refresh KeyInbox(const input::KeyEvent& e) {
    switch (e.key) {
      case input::Key::kUp: Move(-1, static_cast<int>(letters_.size())); break;
      case input::Key::kDown: Move(1, static_cast<int>(letters_.size())); break;
      case input::Key::kLeft: Tab(-1); break;
      case input::Key::kRight: Tab(1); break;
      case input::Key::kEnter: return OpenSelectedLetter();
      default: break;
    }
    return Refresh::kFast;
  }

  Refresh OpenSelectedLetter() {
    if (letters_.empty()) return Refresh::kFast;
    const std::size_t i = static_cast<std::size_t>(Sel());
    if (i >= letters_.size()) return Refresh::kFast;
    open_letter_ = letters_[i];
    // Read state is device-local and never crosses the wire: the server holds
    // no engagement data (§1.1, §11.1).
    if (d_.letters != nullptr) d_.letters->MarkRead(open_letter_.letter.id);
    letters_[i].unread = false;
    open_letter_.unread = false;
    page_ = 0;
    Paginate();
    Go(Screen::kRead);
    // Entering a reading screen is where accumulated partials get cleared
    // (§8.3): ghosting is bounded here rather than left to the wipe.
    return Refresh::kFull;
  }

  void Paginate() {
    pages_ = layout::Paginate(open_letter_.letter.body, Metrics());
    if (pages_.empty()) pages_.push_back(layout::Page{});
  }

  Refresh KeyRead(const input::KeyEvent& e) {
    switch (e.key) {
      case input::Key::kLeft:
      case input::Key::kUp:
        if (page_ > 0) --page_;
        break;
      case input::Key::kRight:
      case input::Key::kDown:
        if (page_ + 1 < static_cast<int>(pages_.size())) ++page_;
        break;
      case input::Key::kBack:
      case input::Key::kEnter:
        Go(Screen::kInbox);
        break;
      default:
        break;
    }
    return Refresh::kFast;
  }

  Refresh KeyPick(const input::KeyEvent& e) {
    switch (e.key) {
      case input::Key::kUp: Move(-1, static_cast<int>(picker_.size())); break;
      case input::Key::kDown: Move(1, static_cast<int>(picker_.size())); break;
      case input::Key::kLeft: Tab(-1); break;
      case input::Key::kRight: Tab(1); break;
      case input::Key::kEnter: {
        if (picker_.empty()) break;
        const std::size_t i = static_cast<std::size_t>(Sel());
        if (i >= picker_.size()) break;
        const std::string id = picker_[i].id;
        if (draft_open_) {
          // A resumed draft that had no person yet: attach and keep writing.
          contact_id_ = id;
          AutosaveDraft();
          Go(Screen::kComposeWrite);
          break;
        }
        Pending p;
        p.kind = PendingKind::kNewLetter;
        p.contact_id = id;
        StartDraft(p);
        break;
      }
      case input::Key::kBack:
        Go(Screen::kInbox);
        break;
      default:
        break;
    }
    return Refresh::kFast;
  }

  Refresh KeyWrite(const input::KeyEvent& e) {
    std::string& field = field_ == 0 ? body_ : subject_;
    const std::size_t cap =
        field_ == 0 ? compose_max_
                    : static_cast<std::size_t>(wire::kMaxSubjectGraphemes);

    switch (e.key) {
      case input::Key::kChar: {
        // The counter counts GRAPHEMES, against the server's cap. Bytes and
        // code points silently disagree with what the panel renders and with
        // what the server measures the moment an emoji appears (server §0,
        // B.7) — and a letter the compose screen allowed but the server
        // refuses is a "couldn't send" the child did nothing to deserve.
        if (layout::CountGraphemes(field) >= cap) {
          message_ = T(STR_COMPOSE_TOO_LONG);
          return Refresh::kFast;
        }
        AppendCodepoint(field, e.codepoint);
        // A combining mark joins the previous cluster instead of adding one;
        // that is not an overflow, so only a cluster that actually appeared
        // past the cap is rolled back.
        if (layout::CountGraphemes(field) > cap) {
          field = layout::TruncateGraphemes(field, cap);
          message_ = T(STR_COMPOSE_TOO_LONG);
          return Refresh::kFast;
        }
        last_edit_ms_ = Now();
        // Batch at word boundaries, idle otherwise — never one refresh per
        // keystroke (§8.3, design §7's largest lever).
        if (e.codepoint == ' ' || e.codepoint == '\n') {
          deferred_paint_ = false;
          return Refresh::kPartial;
        }
        deferred_paint_ = true;
        return Refresh::kPartial;
      }
      // kErase deletes, kBack leaves. Overloading one key with both meanings
      // makes "go back" silently destructive while there is text and
      // navigational once the field empties — the child cannot predict which
      // they will get, and the difference is a sentence they wrote. Erase
      // repeats at the input layer; leaving must not (input.h, F-C13).
      case input::Key::kErase:
        if (!field.empty()) {
          EraseLastGrapheme(field);
          last_edit_ms_ = Now();
          deferred_paint_ = true;
          return Refresh::kPartial;
        }
        return Refresh::kFast;
      case input::Key::kBack:
        // The draft survives leaving the screen: the child stepped out of the
        // letter, they did not throw it away (§11.3).
        AutosaveDraft();
        Go(Screen::kComposePick);
        return Refresh::kFast;
      case input::Key::kUp:
      case input::Key::kDown:
        field_ = field_ == 0 ? 1 : 0;
        return Refresh::kFast;
      case input::Key::kEnter:
        Send();
        return Refresh::kFull;
      default:
        break;
    }
    return Refresh::kFast;
  }

  Refresh KeyOutbox(const input::KeyEvent& e) {
    switch (e.key) {
      case input::Key::kUp: Move(-1, static_cast<int>(queued_.size())); break;
      case input::Key::kDown: Move(1, static_cast<int>(queued_.size())); break;
      case input::Key::kLeft: Tab(-1); break;
      case input::Key::kRight: Tab(1); break;
      case input::Key::kEnter: {
        // §5.4: one key reopens a rejected letter as a new draft, with the
        // child's text intact. Nothing else on this screen is actionable —
        // a letter with the runner is not something to poke at.
        if (queued_.empty()) break;
        const std::size_t i = static_cast<std::size_t>(Sel());
        if (i >= queued_.size() || !queued_[i].rejected) break;
        Pending p;
        p.kind = PendingKind::kRecompose;
        p.contact_id = queued_[i].contact_id;
        p.subject = queued_[i].subject;
        p.body = queued_[i].body;
        p.local_id = queued_[i].local_id;
        StartDraft(p);
        break;
      }
      case input::Key::kBack:
        Go(Screen::kInbox);
        break;
      default:
        break;
    }
    return Refresh::kFast;
  }

  Refresh KeySettings(const input::KeyEvent& e) {
    constexpr int kSettingsRows = 5;
    switch (e.key) {
      case input::Key::kUp: Move(-1, kSettingsRows); break;
      case input::Key::kDown: Move(1, kSettingsRows); break;
      case input::Key::kLeft: Tab(-1); break;
      case input::Key::kRight: Tab(1); break;
      case input::Key::kBack: Go(Screen::kInbox); break;
      case input::Key::kEnter:
        switch (Sel()) {
          case 0: CycleFont(); return Refresh::kFull;  // repaginates (§8.2)
          case 1: CycleFrontlight(); break;
          case 2: Go(Screen::kSettingsContacts); break;
          case 3: Go(Screen::kSettingsLog); break;
          default: Go(Screen::kSettingsAbout); break;
        }
        break;
      default:
        break;
    }
    return Refresh::kFast;
  }

  void CycleFont() {
    if (d_.settings == nullptr) return;
    const int step = (d_.settings->Get().font_step + 1) % store::kFontStepCount;
    d_.settings->SetFontStep(step);
    // Changing size repaginates instantly and locally; nothing is
    // re-downloaded, which is why layout is device-owned at all (§8.2, A.10).
    if (!open_letter_.letter.id.empty()) {
      Paginate();
      if (page_ >= static_cast<int>(pages_.size())) page_ = static_cast<int>(pages_.size()) - 1;
    }
  }

  void CycleFrontlight() {
    if (d_.settings == nullptr) return;
    const int step = (d_.settings->Get().frontlight_step + 1) % kFrontlightStepCount;
    d_.settings->SetFrontlightStep(step);
  }

  Refresh KeyContacts(const input::KeyEvent& e) {
    if (editing_) return KeyNicknameEdit(e);
    switch (e.key) {
      case input::Key::kUp: Move(-1, static_cast<int>(roster_.size())); break;
      case input::Key::kDown: Move(1, static_cast<int>(roster_.size())); break;
      case input::Key::kRight: TogglePinned(); break;
      case input::Key::kBack: Go(Screen::kSettings); break;
      case input::Key::kEnter: {
        const std::size_t i = static_cast<std::size_t>(Sel());
        if (i >= roster_.size()) break;
        editing_ = true;
        edit_buf_ = roster_[i].name;
        break;
      }
      default:
        break;
    }
    return Refresh::kFast;
  }

  Refresh KeyNicknameEdit(const input::KeyEvent& e) {
    switch (e.key) {
      case input::Key::kChar:
        if (layout::CountGraphemes(edit_buf_) < kMaxNicknameGraphemes) {
          AppendCodepoint(edit_buf_, e.codepoint);
        }
        break;
      case input::Key::kBack:
        if (edit_buf_.empty()) {
          editing_ = false;
        } else {
          EraseLastGrapheme(edit_buf_);
        }
        break;
      case input::Key::kEnter:
        CommitNickname();
        editing_ = false;
        break;
      default:
        break;
    }
    return Refresh::kFast;
  }

  // The overlay is device-local and never sent anywhere: the wire has no
  // device->server mutation, and adding engagement state to the server is a
  // spec reversal (§4.4, B.3, server §1.1).
  void TogglePinned() {
    const std::size_t i = static_cast<std::size_t>(Sel());
    if (i >= roster_.size() || d_.contacts == nullptr) return;
    ayllu::Overlay o = OverlayOf(roster_[i]);
    o.pinned = !roster_[i].pinned;
    d_.contacts->SetOverlay(roster_[i].id, o);
    ReloadContacts();
  }

  void CommitNickname() {
    const std::size_t i = static_cast<std::size_t>(Sel());
    if (i >= roster_.size() || d_.contacts == nullptr) return;
    ayllu::Overlay o = OverlayOf(roster_[i]);
    // An empty or unchanged nickname clears the override, so a guardian's
    // rename shows through again (§4.4).
    if (!edit_buf_.empty() && edit_buf_ != roster_[i].server_name) {
      o.nickname = edit_buf_;
    } else {
      o.nickname.reset();
    }
    d_.contacts->SetOverlay(roster_[i].id, o);
    ReloadContacts();
  }

  // Reconstructs the stored overlay from the merged view, so editing one
  // field does not silently clear another.
  static ayllu::Overlay OverlayOf(const ayllu::Contact& c) {
    ayllu::Overlay o;
    if (c.name != c.server_name) o.nickname = c.name;
    o.pinned = c.pinned;
    return o;
  }

  Refresh KeyConflict(const input::KeyEvent& e) {
    switch (e.key) {
      case input::Key::kUp: Move(-1, 2); break;
      case input::Key::kDown: Move(1, 2); break;
      case input::Key::kEnter:
        if (Sel() == 0) {
          KeepPendingDraft();
        } else {
          DropPendingDraft();
        }
        return Refresh::kFull;
      default:
        break;
    }
    return Refresh::kFast;
  }

  void KeepPendingDraft() {
    store::Draft draft;
    if (d_.drafts == nullptr || !d_.drafts->Load(draft)) {
      pending_ = Pending{};
      Go(Screen::kInbox);
      return;
    }
    pending_ = Pending{};
    OpenCompose(draft.contact_id, draft.subject, draft.body);
  }

  void DropPendingDraft() {
    if (d_.drafts != nullptr) d_.drafts->Discard();
    const Pending p = pending_;
    pending_ = Pending{};
    CloseCompose();
    if (p.kind == PendingKind::kResume) {
      Go(Screen::kInbox);
      return;
    }
    StartDraft(p);
  }

  Refresh KeyFault(const input::KeyEvent& e) {
    // Charge me refuses to open content until charging (§9, design §4.1).
    // Every other fault is informational: the child can carry on.
    if (fault_ == FaultKind::kChargeMe) return Refresh::kFast;
    if (e.key == input::Key::kBack || e.key == input::Key::kEnter) {
      fault_ = FaultKind::kNone;
      diagnostic_.clear();
      Go(Screen::kInbox);
      return Refresh::kFull;
    }
    return Refresh::kFast;
  }

  Refresh KeyLines(const input::KeyEvent& e) {
    switch (e.key) {
      case input::Key::kUp: Move(-1, static_cast<int>(view_.lines.size())); break;
      case input::Key::kDown: Move(1, static_cast<int>(view_.lines.size())); break;
      case input::Key::kBack:
      case input::Key::kEnter: Go(Screen::kSettings); break;
      default: break;
    }
    return Refresh::kFast;
  }

  // ---- view assembly -------------------------------------------------------

  void Rebuild() {
    const layout::Metrics m = Metrics();
    view_ = View{};
    view_.screen = screen_;
    view_.fault = fault_;
    view_.editing = editing_;
    if (!message_.empty()) view_.message = layout::BreakLines(message_, m.CharsPerLine());

    switch (screen_) {
      case Screen::kCover: break;  // the cover is rendered from CoverState only
      case Screen::kPin: BuildPin(m); break;
      case Screen::kInbox: BuildInbox(); break;
      case Screen::kRead: BuildRead(m); break;
      case Screen::kComposePick: BuildPick(); break;
      case Screen::kComposeWrite: BuildWrite(m); break;
      case Screen::kOutbox: BuildOutbox(); break;
      case Screen::kSettings: BuildSettings(); break;
      case Screen::kFault: BuildFault(m); break;
      case Screen::kDraftConflict: BuildConflict(m); break;
      case Screen::kSettingsContacts: BuildContacts(); break;
      case Screen::kSettingsLog: BuildLog(); break;
      case Screen::kSettingsAbout: BuildAbout(); break;
    }

    // Clamp last: a list that shrank under the selection (an acked letter, a
    // contact that went away) must not leave the cursor pointing past the end.
    const int count = static_cast<int>(view_.rows.size());
    if (Sel() >= count) Sel() = count > 0 ? count - 1 : 0;
    view_.selected = Sel();
  }

  void BuildPin(const layout::Metrics& m) {
    view_.title = T(STR_APP_NAME);
    view_.editing = true;
    if (view_.message.empty()) {
      view_.message = layout::BreakLines(T(STR_PIN_PROMPT), m.CharsPerLine());
    }
    // The digits are never echoed, only counted.
    std::string mask;
    for (std::size_t i = 0; i < pin_entry_.size(); ++i) mask += T(STR_PIN_DIGIT_MARK);
    view_.lines.push_back(mask);
  }

  void BuildInbox() {
    view_.title = T(STR_INBOX_TITLE);
    if (letters_.empty()) {
      view_.lines.push_back(T(STR_INBOX_EMPTY));
      return;
    }
    for (const store::StoredLetter& l : letters_) {
      Row r;
      r.primary = ContactLabel(l.letter.contact_id);
      r.secondary = l.letter.subject;
      r.meta = TimeLabel(l.letter.date, ClockValid(), d_.text);
      r.unread = l.unread;
      view_.rows.push_back(std::move(r));
    }
  }

  void BuildRead(const layout::Metrics& m) {
    view_.title = ContactLabel(open_letter_.letter.contact_id);
    view_.page = page_;
    view_.page_count = static_cast<int>(pages_.size());
    if (page_ >= 0 && page_ < static_cast<int>(pages_.size())) {
      view_.lines = pages_[static_cast<std::size_t>(page_)].lines;
    }
    const bool last = page_ + 1 >= static_cast<int>(pages_.size());
    if (last && open_letter_.letter.trimmed) view_.lines.push_back(T(STR_READ_TRIMMED));
    // The archive at home continues where the device stops — the device is a
    // view, the mailbox is canonical (§8.2, B.8).
    if (last && open_letter_.letter.truncated) {
      for (const std::string& l : layout::BreakLines(T(STR_READ_TRUNCATED), m.CharsPerLine())) {
        view_.lines.push_back(l);
      }
    }
    // `degraded` renders nothing at all: it is server-side bookkeeping and the
    // next sync cleans it up (§8.2).
  }

  void BuildPick() {
    view_.title = T(STR_COMPOSE_PICK_TITLE);
    if (picker_.empty()) {
      view_.lines.push_back(T(STR_COMPOSE_PICK_EMPTY));
      return;
    }
    for (const ayllu::Contact& c : picker_) {
      Row r;
      r.primary = c.name;
      r.marked = c.pinned;
      view_.rows.push_back(std::move(r));
    }
  }

  void BuildWrite(const layout::Metrics& m) {
    view_.title = ContactLabel(contact_id_);
    view_.editing = true;
    view_.graphemes_used = layout::CountGraphemes(body_);
    view_.graphemes_max = compose_max_;
    Row subject;
    subject.primary = subject_.empty() ? T(STR_COMPOSE_SUBJECT_HINT) : subject_;
    subject.marked = field_ == 1;
    view_.rows.push_back(std::move(subject));
    view_.lines = layout::BreakLines(body_, m.CharsPerLine());
  }

  void BuildOutbox() {
    view_.title = T(STR_OUTBOX_TITLE);
    if (queued_.empty()) {
      view_.lines.push_back(T(STR_OUTBOX_EMPTY));
      return;
    }
    bool rejected_selected = false;
    for (std::size_t i = 0; i < queued_.size(); ++i) {
      const store::OutboxEntry& q = queued_[i];
      Row r;
      r.primary = ContactLabel(q.contact_id);
      r.secondary = q.subject;
      // The child sees "waiting" or "couldn't send" and nothing finer. Which
      // reject it was is a guardian's question (§5.4).
      r.meta = q.rejected ? T(STR_STATE_COULDNT_SEND) : T(STR_STATE_WAITING);
      r.marked = q.rejected;
      view_.rows.push_back(std::move(r));
      if (static_cast<int>(i) == Sel() && q.rejected) rejected_selected = true;
    }
    if (rejected_selected && message_.empty()) {
      view_.message = layout::BreakLines(T(STR_SEND_FAILED), Metrics().CharsPerLine());
      view_.lines.push_back(T(STR_SEND_FAILED_ACTION));
    }
  }

  void BuildSettings() {
    view_.title = T(STR_SETTINGS_TITLE);
    const store::Settings s =
        d_.settings != nullptr ? d_.settings->Get() : store::Settings{};

    Row font;
    font.primary = T(STR_SETTINGS_FONT_SIZE);
    font.secondary = s.font_step >= 1 ? T(STR_SETTINGS_FONT_BIGGER) : T(STR_SETTINGS_FONT_NORMAL);
    view_.rows.push_back(std::move(font));

    Row light;
    light.primary = T(STR_SETTINGS_FRONTLIGHT);
    light.secondary = s.frontlight_step == 0
                          ? std::string(T(STR_SETTINGS_LIGHT_OFF))
                          : Format(T(STR_SETTINGS_LIGHT_STEP_FMT), {Int(s.frontlight_step)});
    view_.rows.push_back(std::move(light));

    Row contacts;
    contacts.primary = T(STR_SETTINGS_CONTACTS);
    view_.rows.push_back(std::move(contacts));

    Row log;
    log.primary = T(STR_SETTINGS_WHAT_IT_TELLS);
    view_.rows.push_back(std::move(log));

    Row about;
    about.primary = T(STR_SETTINGS_ABOUT);
    view_.rows.push_back(std::move(about));
  }

  void BuildContacts() {
    view_.title = T(STR_SETTINGS_CONTACTS);
    for (const ayllu::Contact& c : roster_) {
      Row r;
      r.primary = c.name;
      r.secondary = c.pinned ? T(STR_SETTINGS_KEEP_AT_TOP) : std::string();
      r.marked = c.pinned;
      view_.rows.push_back(std::move(r));
    }
    if (editing_) {
      view_.lines.push_back(T(STR_SETTINGS_NICKNAME));
      view_.lines.push_back(edit_buf_);
      view_.editing = true;
    }
  }

  // The plain-language health log: the child can answer "what does it know
  // about me" without asking anyone (design §3.7's third mechanism, §13).
  // Sentences, never a hex dump — a dump is disclosure that cannot be read,
  // which is the same as no disclosure.
  void BuildLog() {
    view_.title = T(STR_SETTINGS_WHAT_IT_TELLS);
    if (d_.health_log == nullptr) {
      view_.lines.push_back(T(STR_SETTINGS_LOG_EMPTY));
      return;
    }
    const std::vector<kipu::Entry> recent = d_.health_log->Recent(kipu::kLogCapacity);
    if (recent.empty()) {
      view_.lines.push_back(T(STR_SETTINGS_LOG_EMPTY));
      return;
    }
    for (const kipu::Entry& e : recent) view_.lines.push_back(LogLine(e));
  }

  std::string LogLine(const kipu::Entry& e) const {
    const std::string sep = T(STR_LIST_SEPARATOR);
    std::string parts = Format(T(STR_LOG_BATTERY_FMT), {Int(e.block.battery_pct)});
    if (e.block.charging) parts += sep + T(STR_LOG_CHARGING);
    parts += sep;
    parts += T(SignalStringId(e.block.rssi));
    parts += sep;
    if (e.block.queue_depth <= 0) {
      parts += T(STR_LOG_NOTHING_WAITING);
    } else if (e.block.queue_depth == 1) {
      parts += T(STR_LOG_ONE_WAITING);
    } else {
      parts += Format(T(STR_LOG_MANY_WAITING_FMT), {Int(e.block.queue_depth)});
    }
    const std::string when = TimeLabel(e.at, ClockValid(), d_.text);
    if (when.empty()) return parts;
    return Format(T(STR_LOG_LINE_FMT), {when, parts});
  }

  void BuildAbout() {
    view_.title = T(STR_SETTINGS_ABOUT);
    const About a = d_.about ? d_.about() : About{};
    view_.lines.push_back(Format(T(STR_ABOUT_VERSION_FMT), {a.firmware_version}));
    view_.lines.push_back(Format(T(STR_ABOUT_BATTERY_FMT), {Int(a.battery_pct)}));
    view_.lines.push_back(Format(T(STR_ABOUT_SIGNAL_FMT), {T(SignalStringId(a.rssi))}));
  }

  void BuildFault(const layout::Metrics& m) {
    view_.title = T(STR_APP_NAME);
    int id = STR_FAULT_ASK_GUARDIANS;
    switch (fault_) {
      case FaultKind::kCantReachHome: id = STR_FAULT_CANT_REACH_HOME; break;
      case FaultKind::kAskYourGuardians: id = STR_FAULT_ASK_GUARDIANS; break;
      case FaultKind::kRoadBusy: id = STR_FAULT_ROAD_BUSY; break;
      case FaultKind::kChargeMe: id = STR_FAULT_CHARGE_ME; break;
      case FaultKind::kNone: id = STR_FAULT_ASK_GUARDIANS; break;
    }
    view_.message = layout::BreakLines(T(id), m.CharsPerLine());
    // The code goes on a separate line for a guardian's eyes and is never the
    // primary text: a child needs an action they can take (§11.6).
    if (!diagnostic_.empty()) {
      view_.diagnostic = Format(T(STR_FAULT_DIAGNOSTIC_FMT), {diagnostic_});
    }
  }

  void BuildConflict(const layout::Metrics& m) {
    view_.title = T(STR_COMPOSE_WRITE_TITLE);
    const bool resume = pending_.kind == PendingKind::kResume;
    view_.message = layout::BreakLines(
        resume ? T(STR_DRAFT_RESUME) : T(STR_DRAFT_CONFLICT_PROMPT), m.CharsPerLine());
    Row keep;
    keep.primary = T(STR_DRAFT_KEEP_WRITING);
    view_.rows.push_back(std::move(keep));
    Row other;
    other.primary = resume ? T(STR_DRAFT_LET_IT_GO) : T(STR_DRAFT_START_NEW);
    view_.rows.push_back(std::move(other));
  }

  // ---- painting ------------------------------------------------------------

  void Present(Refresh r) {
    Rebuild();
    if (d_.painter == nullptr || d_.panel == nullptr) return;

    const layout::Metrics m = Metrics();
    if (r == Refresh::kPartial) {
      if (++partials_since_full_ >= kPartialsBeforeFullRefresh) r = Refresh::kFull;
    }
    if (r == Refresh::kFull) partials_since_full_ = 0;

    Paint(m);
    switch (r) {
      case Refresh::kPartial: {
        panel::Rect all{0, 0, m.panel_w_px, m.panel_h_px};
        d_.panel->PartialRefresh(all, fb_);
        break;
      }
      case Refresh::kFast: d_.panel->FastRefresh(fb_); break;
      case Refresh::kFull: d_.panel->FullRefresh(fb_); break;
    }
  }

  void Paint(const layout::Metrics& m) {
    d_.painter->Clear(fb_, m.panel_w_px, m.panel_h_px);
    const int cols = m.CharsPerLine();
    const int rows_avail = m.LinesPerPage();
    int row = 0;

    if (!view_.title.empty() && row < rows_avail) {
      d_.painter->DrawText(fb_, row++, 0, Ellipsize(view_.title, static_cast<std::size_t>(cols)));
    }
    for (const std::string& line : view_.message) {
      if (row >= rows_avail) break;
      d_.painter->DrawText(fb_, row++, 0, line);
    }

    // The list window follows the selection so the chosen row is always on
    // screen; a selection you cannot see is a key press into the dark.
    const int list_rows = std::max(0, rows_avail - row - 1);
    const int count = static_cast<int>(view_.rows.size());
    int first = 0;
    if (list_rows > 0 && view_.selected >= list_rows) first = view_.selected - list_rows + 1;
    for (int i = first; i < count && row < rows_avail - 1; ++i) {
      const Row& item = view_.rows[static_cast<std::size_t>(i)];
      if (i == view_.selected) d_.painter->DrawGlyph(fb_, row, 0, Glyph::kSelected);
      if (item.unread) d_.painter->DrawGlyph(fb_, row, 1, Glyph::kUnread);
      if (item.marked) d_.painter->DrawGlyph(fb_, row, 1, Glyph::kMark);
      d_.painter->DrawText(fb_, row, 2, Ellipsize(RowText(item), static_cast<std::size_t>(std::max(0, cols - 2))));
      ++row;
    }

    for (const std::string& line : view_.lines) {
      if (row >= rows_avail - 1) break;
      d_.painter->DrawText(fb_, row++, 0, line);
    }

    PaintFooter(m, rows_avail - 1);
  }

  std::string RowText(const Row& r) const {
    const std::string sep = T(STR_LIST_SEPARATOR);
    std::string out = r.primary;
    if (!r.secondary.empty()) out += sep + r.secondary;
    if (!r.meta.empty()) out += sep + r.meta;
    return out;
  }

  void PaintFooter(const layout::Metrics& m, int row) {
    if (row < 0) return;
    (void)m;
    if (!view_.diagnostic.empty()) {
      d_.painter->DrawText(fb_, row, 0, view_.diagnostic);
      return;
    }
    if (view_.screen == Screen::kComposeWrite) {
      d_.painter->DrawText(fb_, row, 0,
                           Format(T(STR_COMPOSE_COUNTER_FMT),
                                  {Int(static_cast<long long>(view_.graphemes_used)),
                                   Int(static_cast<long long>(view_.graphemes_max))}));
      return;
    }
    if (view_.screen == Screen::kRead && view_.page_count > 0) {
      d_.painter->DrawText(fb_, row, 0,
                           Format(T(STR_READ_PAGE_FMT),
                                  {Int(view_.page + 1), Int(view_.page_count)}));
    }
  }

  // ---- state ---------------------------------------------------------------

  Deps d_;
  View view_;
  panel::Framebuf fb_;

  Screen screen_ = Screen::kCover;
  int sel_[kScreenCount] = {0};
  std::string message_;

  std::vector<store::StoredLetter> letters_;
  std::vector<store::OutboxEntry> queued_;
  std::vector<ayllu::Contact> picker_;
  std::vector<ayllu::Contact> roster_;

  store::StoredLetter open_letter_;
  std::vector<layout::Page> pages_;
  int page_ = 0;

  std::string contact_id_;
  std::string subject_;
  std::string body_;
  int field_ = 0;  // 0 = body, 1 = subject
  bool draft_open_ = false;
  std::size_t compose_max_ = static_cast<std::size_t>(wire::kDefaultMaxLetterChars);
  Pending pending_;

  bool editing_ = false;
  std::string edit_buf_;

  std::string pin_entry_;
  int wrong_attempts_ = 0;
  std::int64_t backoff_until_ms_ = 0;
  bool unlocked_ = false;

  FaultKind fault_ = FaultKind::kNone;
  std::string diagnostic_;
  bool charge_me_ = false;

  std::int64_t last_key_ms_ = 0;
  std::int64_t last_save_ms_ = 0;
  std::int64_t last_edit_ms_ = 0;
  bool deferred_paint_ = false;
  int partials_since_full_ = 0;
};

}  // namespace

std::unique_ptr<App> New(const Deps& d) { return std::make_unique<AppImpl>(d); }

}  // namespace chaski::ui
