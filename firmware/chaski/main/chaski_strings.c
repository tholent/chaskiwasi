// strings.c — every user-visible word in the firmware. See strings.h.
#include "chaski_strings.h"

static const char* const kStrings[STR__COUNT] = {
    [STR_APP_NAME] = "Chaski",
    [STR_SYSTEM_SENDER] = "Wasi",
    [STR_CONTACT_UNKNOWN] = "Someone",

    [STR_STATE_WAITING] = "waiting",
    [STR_STATE_ON_THE_ROAD] = "on the road",
    [STR_STATE_ARRIVED] = "arrived",

    [STR_INBOX_TITLE] = "Letters",
    [STR_INBOX_EMPTY] = "No letters yet.",
    [STR_READ_TRIMMED] = "...",
    [STR_READ_TRUNCATED] = "This letter continues in the archive at home.",

    [STR_COMPOSE_PICK_TITLE] = "Write to",
    [STR_COMPOSE_WRITE_TITLE] = "Your letter",
    [STR_COMPOSE_SUBJECT_HINT] = "About (you can skip this)",
    [STR_COMPOSE_TOO_LONG] = "That's as long as a letter can be.",
    [STR_COMPOSE_SENT] = "Your letter is waiting for the runner.",
    [STR_DRAFT_RESUME] = "Keep writing the letter you started?",
    [STR_OUTBOX_FULL] = "The bag is full. Send these first.",

    [STR_OUTBOX_TITLE] = "On the road",
    [STR_SEND_FAILED] = "This letter couldn't be sent. Ask your guardians about it.",

    [STR_FAULT_CANT_REACH_HOME] = "Can't reach home.",
    [STR_FAULT_ASK_GUARDIANS] = "Something needs a grown-up. Ask your guardians.",
    [STR_FAULT_ROAD_BUSY] = "The road is busy. Trying again soon.",
    [STR_FAULT_CHARGE_ME] = "Nearly out of power. Please charge me.",

    [STR_PIN_PROMPT] = "Your number",
    [STR_PIN_WRONG] = "Not quite. Try again.",

    [STR_SETTINGS_TITLE] = "Settings",
    [STR_SETTINGS_FONT_SIZE] = "Text size",
    [STR_SETTINGS_FRONTLIGHT] = "Light",
    [STR_SETTINGS_CONTACTS] = "Names and faces",
    [STR_SETTINGS_WHAT_IT_TELLS] = "What my Chaski tells home",
    [STR_SETTINGS_ABOUT] = "About this Chaski",
};

const char* S(chaski_string_id_t id) {
  if (id < 0 || id >= STR__COUNT || kStrings[id] == 0) return "";
  return kStrings[id];
}
