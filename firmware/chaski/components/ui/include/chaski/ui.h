// Package ui owns the screens and the words on them.
//
// Register: game mail, not email (design §4). A letter is WAITING (in the
// outbox), ON THE ROAD (with the runner), or ARRIVED. "Queued — no connection"
// reads as broken; "on the road" reads as how the world works, and that is the
// difference between a device a child trusts and one they stop opening.
//
// Every string rendered here comes from strings.c. Nothing in this component
// may contain a user-visible literal, and none of pututu/ayllu/kipu may appear
// in any string it renders (client §0, C-15).
//
// How the words get here (F-C12): the strings table lives in main/, and an
// ESP-IDF component cannot depend on main. So the table is INJECTED —
// Deps::text is a plain function pointer the composition root sets to
// `chaski_text`. The component therefore references no symbol defined in
// main/, which keeps the link graph one-directional, and the ids stay in the
// single table rather than being mirrored into a second enum here.
//
// Portability: no esp_* and no FreeRTOS headers (implementation-plan ground
// rule 3). Time, input, panel, storage and glyph drawing all arrive through
// seams, which is what makes every flow below host-testable.
#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <vector>

#include "chaski/ayllu.h"
#include "chaski/draft.h"
#include "chaski/input.h"
#include "chaski/kipu.h"
#include "chaski/layout.h"
#include "chaski/panel.h"
#include "chaski/settings.h"
#include "chaski/store.h"

namespace chaski::ui {

enum class Screen {
  kCover,        // what sleep leaves behind (client §9)
  kPin,          // only when a guardian pushed one (client §11.5, B.4)
  kInbox,
  kRead,
  kComposePick,  // pick a person FIRST — never a To: field (design §4)
  kComposeWrite,
  kOutbox,
  kSettings,
  kFault,        // the §11.6 states, each naming an action a child can take

  // §11.3's decision point. One draft slot in v1, so both "you were writing
  // something when you fell asleep" and "you already have one open" are the
  // same question asked of the child, never an overwrite decided for them.
  kDraftConflict,

  // Settings sub-screens (§11.7). Separate screens rather than modes because
  // each has its own keys and its own back destination.
  kSettingsContacts,  // the cosmetic overlay, device-local (§4.4, B.3)
  kSettingsLog,       // "what my Chaski tells home" (design §3.7)
  kSettingsAbout,
};

// Fault mirrors syncengine::Fault at the presentation layer. Each has its own
// visible state on purpose: a silently dead device is the failure the design
// spec warns about in hardware form (D-6, client §11.6).
enum class FaultKind {
  kNone,
  kCantReachHome,
  kAskYourGuardians,
  kRoadBusy,
  kChargeMe,
};

// TextFn resolves a string id from the one table (main/chaski_strings.c). It
// takes an int rather than the table's enum so this header stays free of any
// main/ dependency; the composition root passes `chaski_text`.
using TextFn = const char* (*)(int id);

// Glyph is everything drawn that is not text. The mail flag is here rather
// than in the strings table because it is a mark, not a word — and because a
// glyph cannot accidentally be given a count to render (B.5).
enum class Glyph {
  kWordmark,
  kMailFlag,
  kBattery,
  kCharging,
  kUnread,
  kSelected,
  kMark,         // a pinned contact, or a letter that couldn't be sent
  kPortrait,     // the contact's 1-bit portrait from the built-in set (§11.2)
  kSubstitute,   // an unrenderable cluster, shown honestly (B.10)
};

// Painter is the glyph seam: it turns already-broken lines into pixels.
//
// It is a seam and not a function in this component because v1 ships no bitmap
// font — layout owns metrics, and glyph data has no owner yet. Screens are
// therefore composed, asserted and shipped independently of what draws them,
// and host tests record the exact text and marks each screen asked for, which
// is a stronger statement about content leakage than comparing pixels would be
// (C-12).
class Painter {
 public:
  virtual ~Painter() = default;

  // Clear sizes `fb` to w x h and blanks it.
  virtual void Clear(panel::Framebuf& fb, int w, int h) = 0;

  // DrawText draws one already-broken line at a character cell. `utf8` holds
  // whole grapheme clusters only — the caller breaks through layout, never by
  // byte index (server §4.9).
  virtual void DrawText(panel::Framebuf& fb, int row, int col,
                        const std::string& utf8) = 0;

  virtual void DrawGlyph(panel::Framebuf& fb, int row, int col, Glyph g) = 0;
};

// Row is one line of a list screen, already resolved to text. There is no
// contact id and no letter id here: what the screen can render is what it was
// given, and ids are not for reading.
struct Row {
  std::string primary;    // sender label, contact name, or subject
  std::string secondary;  // subject, or the current value of a setting
  std::string meta;       // date, or the letter's state in the child's words
  bool unread = false;
  bool marked = false;    // pinned contact, or a rejected outbox entry
};

// View is the whole of what a screen shows, resolved to text and ready to
// paint. Tests assert against this, which is why it holds no ids, no counts
// the spec forbids, and nothing a renderer would have to interpret.
struct View {
  Screen screen = Screen::kCover;
  std::string title;
  std::vector<std::string> message;  // broken to the panel width
  std::vector<Row> rows;
  std::vector<std::string> lines;    // letter page, log lines, about lines
  int selected = 0;
  int page = 0;
  int page_count = 0;

  // The compose counter, in graphemes — the unit the reader perceives and the
  // unit the server's cap counts (server §0). Held as numbers so the model
  // cannot disagree with the string that renders them.
  std::size_t graphemes_used = 0;
  std::size_t graphemes_max = 0;

  // diagnostic is a guardian's line, never the primary text of a fault
  // screen: a child needs an action they can take, not an error code
  // (client §11.6).
  std::string diagnostic;
  FaultKind fault = FaultKind::kNone;

  // editing is true while a text field has the keys — the compose body and
  // subject, the PIN entry, a contact's nickname.
  bool editing = false;
};

// About is the §11.7 about screen's content, supplied by the composition root
// because none of it is the UI's to know.
struct About {
  std::string firmware_version;
  int battery_pct = 0;
  bool charging = false;
  int rssi = 0;
};

struct Deps {
  store::LetterStore* letters = nullptr;
  store::Outbox* outbox = nullptr;
  store::DraftStore* drafts = nullptr;
  store::SettingsStore* settings = nullptr;
  ayllu::Store* contacts = nullptr;

  // health_log backs "what my Chaski tells home": the child can answer "what
  // does it know about me" without asking anyone (design §3.7). May be null.
  kipu::Log* health_log = nullptr;

  panel::Panel* panel = nullptr;
  Painter* painter = nullptr;
  TextFn text = nullptr;

  // The clock is valid only after the first sync of a power cycle. Until then
  // dates render blank rather than wrong and compose timestamps are deferred
  // — the server stamps them anyway (client §5.6, C-21).
  std::function<std::int64_t()> now_epoch;
  std::function<bool()> clock_valid;
  std::function<std::int64_t()> monotonic_ms;

  // verify_pin checks an entry against the PIN in encrypted NVS. The value
  // never reaches this component (client §4.2, §11.5).
  std::function<bool(const std::string&)> verify_pin;

  // request_sync is the sync the UI itself asks for. The sync KEY does not
  // come through here — input intercepts it below UI dispatch so a wedged
  // screen cannot swallow it (client §10).
  std::function<void()> request_sync;

  std::function<About()> about;
};

class App {
 public:
  virtual ~App() = default;

  // Start opens a session on `initial`. A pushed PIN overrides it — nothing
  // but the cover shows until the PIN is entered (client §11.5) — and a
  // pending draft is offered before anything else, because waking mid-letter
  // and finding it gone is the failure §11.3 forbids.
  virtual void Start(Screen initial) = 0;

  // Key delivers one UI-visible event. Put-away and sync never arrive here:
  // they are consumed in the input layer (client §10, C-11).
  virtual void Key(const input::KeyEvent& e) = 0;

  // Tick drives the time-based work: the ~30 s draft autosave, the batched
  // compose refresh (§8.3), the PIN backoff expiring, and a PIN cleared
  // remotely taking effect (client §11.5).
  virtual void Tick() = 0;

  virtual Screen Current() const = 0;
  virtual const View& CurrentView() const = 0;

  // ShowFault switches to the fault screen. `diagnostic` is for a guardian and
  // is never the primary text (client §11.6); pass empty when there is none.
  virtual void ShowFault(FaultKind k, const std::string& diagnostic) = 0;

  // AutosaveDraft is the every-wipe-trigger save of §11.3. The session calls
  // it on timeout, on put-away, and on the low-battery path; it is idempotent
  // and safe to call from any screen.
  virtual void AutosaveDraft() = 0;

  // Cover is what the wipe controller should leave on the glass. Deriving it
  // here keeps the count where it belongs: nowhere (B.5).
  virtual panel::CoverState Cover() const = 0;

  // IdleTimedOut reports the ~45 s inactivity trigger (client §6, §9). The
  // session owns the wipe; the UI only knows when the child stopped.
  virtual bool IdleTimedOut() const = 0;
};

std::unique_ptr<App> New(const Deps& d);

// RenderCover paints the resting cover. It receives CoverState and nothing
// else, so it cannot render a count, a name, or a letter (client §9, B.5,
// C-12).
void RenderCover(const panel::CoverState& s, Painter& painter, TextFn text,
                 panel::Framebuf& fb);

// CoverRenderer adapts RenderCover to panel::WipeDeps::render_cover.
std::function<void(const panel::CoverState&, panel::Framebuf&)> CoverRenderer(
    Painter* painter, TextFn text);

// PIN backoff (client §11.5, B.4): 1 s, doubling, capped at 60 s. There is no
// attempt limit and nothing is ever destroyed — flash encryption already
// protects the stored letters, and a forgotten PIN three states away must not
// brick the inbox. Recovery is the guardian clearing it in the config.
inline constexpr int kPinBackoffFirstMs = 1000;
inline constexpr int kPinBackoffMaxMs = 60000;
inline constexpr std::size_t kPinMaxDigits = 6;  // 4-6 digits (§11.5)

// Inactivity timeout, then wipe and sleep (client §6, design §4.1).
inline constexpr std::int64_t kInactivityTimeoutMs = 45000;

// Refresh discipline (client §8.3): never a partial refresh per keystroke.
// Typing is batched to a word boundary or a short idle, and a bounded number
// of partials is followed by a full refresh so ghosting cannot accumulate —
// ghosting is a privacy parameter here, not cosmetics (design §11).
inline constexpr std::int64_t kComposeIdleRefreshMs = 200;
inline constexpr int kPartialsBeforeFullRefresh = 8;

// Frontlight steps, default off (client §2). The panel is readable without it;
// a device that lights itself in a bag is neither private nor cheap.
inline constexpr int kFrontlightStepCount = 4;

}  // namespace chaski::ui
