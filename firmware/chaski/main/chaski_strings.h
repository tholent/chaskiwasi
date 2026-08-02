// chaski_strings.h — the ONE place user-visible text lives.
//
// Named chaski_strings.h, not strings.h: a header called strings.h shadows
// POSIX <strings.h> for every translation unit that sees this directory on its
// include path, which breaks anything reaching for strcasecmp (GoogleTest
// does). Found by building the Wave 0 scaffold; recorded as F-C2.
//
// This is the vocabulary boundary made structural rather than a matter of
// discipline (design §0.1, client §0). Two rules, both enforced by C-15:
//   1. No user-visible literal exists anywhere else in the firmware.
//   2. pututu, ayllu, and kipu never appear in this file. They are greppable
//      internal identifiers; on a screen a child reads they are noise at best.
//
// Wire field names may carry those words — the wire is machine-facing. Screens
// may not.
//
// The device's public vocabulary is the game-mail register: letters (never
// messages), waiting -> on the road -> arrived. The system sender is "Wasi",
// chosen over "Home" because this device exists for a young person who moves
// often and "home" asserts something about their life that may not be true
// (server A.16).
#pragma once

#ifdef __cplusplus
extern "C" {
#endif

// Composition: a few strings carry a {} placeholder, filled by the UI's own
// formatter. Punctuation and separators live here with the words, so that
// building a sentence out of parts still leaves every visible byte in this
// file — a format string assembled in the UI would be text outside the table
// wearing a disguise (client §0, C-15).
typedef enum {
  STR_APP_NAME,
  STR_SYSTEM_SENDER,        // "Wasi" (server A.16)
  STR_CONTACT_UNKNOWN,      // fallback label for an unresolved contact (C-14)

  // Letter states, in the child's words.
  STR_STATE_WAITING,
  STR_STATE_ON_THE_ROAD,
  STR_STATE_ARRIVED,
  STR_STATE_COULDNT_SEND,   // a terminal reject, waiting for the child (§5.4)

  // Inbox / read.
  STR_INBOX_TITLE,
  STR_INBOX_EMPTY,
  STR_READ_TRIMMED,         // a quoted tail was removed
  STR_READ_TRUNCATED,       // the rest is in the archive at home
  STR_READ_PAGE_FMT,        // page indicator: "{} of {}"

  // Compose.
  STR_COMPOSE_PICK_TITLE,
  STR_COMPOSE_PICK_EMPTY,
  STR_COMPOSE_WRITE_TITLE,
  STR_COMPOSE_SUBJECT_HINT,
  STR_COMPOSE_TOO_LONG,
  STR_COMPOSE_EMPTY,        // nothing written yet; sending would be refused
  STR_COMPOSE_SENT,
  STR_COMPOSE_COUNTER_FMT,  // graphemes used against the cap: "{}/{}"
  STR_DRAFT_RESUME,
  STR_DRAFT_LET_IT_GO,      // the other answer to "keep writing?"
  STR_OUTBOX_FULL,          // the bag is full — sync to send these first (B.9)

  // Draft conflict (client §11.3): one draft slot in v1 — starting a new
  // letter while one is already in progress asks which to keep.
  STR_DRAFT_CONFLICT_PROMPT,
  STR_DRAFT_KEEP_WRITING,
  STR_DRAFT_START_NEW,

  // Outbox / send failure. The device never explains WHICH reject happened:
  // the distinctions exist for guardians, server-side (client §5.4).
  STR_OUTBOX_TITLE,
  STR_OUTBOX_EMPTY,
  STR_SEND_FAILED,          // "This letter couldn't be sent. Ask your guardians about it."
  STR_SEND_FAILED_ACTION,   // the one key that reopens it as a new draft (D-5)

  // Storage refusals. Rare, and worth a word rather than a screen that does
  // nothing: silence would look like the device ignoring the child.
  STR_STORAGE_FAILED,

  // Fault states (client §11.6). Each names an action, not an error code.
  STR_FAULT_CANT_REACH_HOME,
  STR_FAULT_ASK_GUARDIANS,
  STR_FAULT_ROAD_BUSY,
  STR_FAULT_CHARGE_ME,
  STR_FAULT_DIAGNOSTIC_FMT, // a guardian's line, never the primary text

  // PIN (client §11.5).
  STR_PIN_PROMPT,
  STR_PIN_WRONG,
  STR_PIN_TRY_AGAIN_SOON,   // shown during the doubling backoff (client §11.5)
  STR_PIN_DIGIT_MARK,       // one entered digit, never the digit itself

  // Settings, including the transparency log (design §3.7).
  STR_SETTINGS_TITLE,
  STR_SETTINGS_FONT_SIZE,
  STR_SETTINGS_FONT_NORMAL,
  STR_SETTINGS_FONT_BIGGER,
  STR_SETTINGS_FRONTLIGHT,
  STR_SETTINGS_LIGHT_OFF,
  STR_SETTINGS_LIGHT_STEP_FMT,
  STR_SETTINGS_CONTACTS,
  STR_SETTINGS_NICKNAME,
  STR_SETTINGS_KEEP_AT_TOP,
  STR_SETTINGS_WHAT_IT_TELLS,   // "what my Chaski tells home"
  STR_SETTINGS_LOG_EMPTY,
  STR_SETTINGS_ABOUT,
  STR_ABOUT_VERSION_FMT,
  STR_ABOUT_BATTERY_FMT,
  STR_ABOUT_SIGNAL_FMT,

  // The plain-language health log (design §3.7's third transparency
  // mechanism). Assembled from these parts so the child reads a sentence, not
  // a hex dump: "Tue 15:04 - battery 64%, good signal, 1 letter waiting".
  STR_LOG_LINE_FMT,
  STR_LOG_BATTERY_FMT,
  STR_LOG_CHARGING,
  STR_LOG_SIGNAL_GOOD,
  STR_LOG_SIGNAL_WEAK,
  STR_LOG_SIGNAL_NONE,
  STR_LOG_NOTHING_WAITING,
  STR_LOG_ONE_WAITING,
  STR_LOG_MANY_WAITING_FMT,
  STR_LIST_SEPARATOR,

  // Dates. Blank until the first sync of a power cycle: a wrong date is worse
  // than no date, and this device has no battery-backed clock (client §5.6).
  STR_TIME_FMT,             // "{} {}:{}" — day, hour, minute
  STR_DAY_MON,
  STR_DAY_TUE,
  STR_DAY_WED,
  STR_DAY_THU,
  STR_DAY_FRI,
  STR_DAY_SAT,
  STR_DAY_SUN,

  STR__COUNT,
} chaski_string_id_t;

// S returns the display text for an id. Never returns NULL.
const char* S(chaski_string_id_t id);

// chaski_text is S behind an int-typed signature, for components that render
// text without depending on main/ — which every component must, since ESP-IDF
// components cannot require main (F-C12). The composition root hands this to
// ui::Deps::text; the ids stay in one table rather than being mirrored into a
// second enum somewhere else.
const char* chaski_text(int id);

#ifdef __cplusplus
}
#endif
