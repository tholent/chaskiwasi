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

typedef enum {
  STR_APP_NAME,
  STR_SYSTEM_SENDER,        // "Wasi" (server A.16)
  STR_CONTACT_UNKNOWN,      // fallback label for an unresolved contact (C-14)

  // Letter states, in the child's words.
  STR_STATE_WAITING,
  STR_STATE_ON_THE_ROAD,
  STR_STATE_ARRIVED,

  // Inbox / read.
  STR_INBOX_TITLE,
  STR_INBOX_EMPTY,
  STR_READ_TRIMMED,         // a quoted tail was removed
  STR_READ_TRUNCATED,       // the rest is in the archive at home

  // Compose.
  STR_COMPOSE_PICK_TITLE,
  STR_COMPOSE_WRITE_TITLE,
  STR_COMPOSE_SUBJECT_HINT,
  STR_COMPOSE_TOO_LONG,
  STR_COMPOSE_SENT,
  STR_DRAFT_RESUME,
  STR_OUTBOX_FULL,          // the bag is full — sync to send these first (B.9)

  // Draft conflict (client §11.3): one draft slot in v1 — starting a new
  // letter while one is already in progress asks which to keep.
  STR_DRAFT_CONFLICT_PROMPT,
  STR_DRAFT_KEEP_WRITING,
  STR_DRAFT_START_NEW,

  // Outbox / send failure. The device never explains WHICH reject happened:
  // the distinctions exist for guardians, server-side (client §5.4).
  STR_OUTBOX_TITLE,
  STR_SEND_FAILED,          // "This letter couldn't be sent. Ask your guardians about it."

  // Fault states (client §11.6). Each names an action, not an error code.
  STR_FAULT_CANT_REACH_HOME,
  STR_FAULT_ASK_GUARDIANS,
  STR_FAULT_ROAD_BUSY,
  STR_FAULT_CHARGE_ME,

  // PIN (client §11.5).
  STR_PIN_PROMPT,
  STR_PIN_WRONG,
  STR_PIN_TRY_AGAIN_SOON,   // shown during the doubling backoff (client §11.5)

  // Settings, including the transparency log (design §3.7).
  STR_SETTINGS_TITLE,
  STR_SETTINGS_FONT_SIZE,
  STR_SETTINGS_FRONTLIGHT,
  STR_SETTINGS_CONTACTS,
  STR_SETTINGS_WHAT_IT_TELLS,   // "what my Chaski tells home"
  STR_SETTINGS_ABOUT,

  STR__COUNT,
} chaski_string_id_t;

// S returns the display text for an id. Never returns NULL.
const char* S(chaski_string_id_t id);

#ifdef __cplusplus
}
#endif
