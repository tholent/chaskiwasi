// strings.c — every user-visible word in the firmware. See strings.h.
//
// Line length: the reference panel is 264x176 at a 6x13 face, so a line holds
// about 43 characters (layout::Metrics::CharsPerLine). Longer strings are not
// wrong — the layout engine breaks them on grapheme boundaries — but a string
// written to fit reads better than one that wraps mid-thought.
#include "chaski_strings.h"

static const char* const kStrings[STR__COUNT] = {
    [STR_APP_NAME] = "Chaski",
    [STR_SYSTEM_SENDER] = "Wasi",
    [STR_CONTACT_UNKNOWN] = "Someone",

    [STR_STATE_WAITING] = "waiting",
    [STR_STATE_ON_THE_ROAD] = "on the road",
    [STR_STATE_ARRIVED] = "arrived",
    [STR_STATE_COULDNT_SEND] = "couldn't send",

    [STR_INBOX_TITLE] = "Letters",
    [STR_INBOX_EMPTY] = "No letters yet.",
    [STR_READ_TRIMMED] = "...",
    [STR_READ_TRUNCATED] = "This letter continues in the archive at home.",
    [STR_READ_PAGE_FMT] = "{} of {}",

    [STR_COMPOSE_PICK_TITLE] = "Write to",
    [STR_COMPOSE_PICK_EMPTY] = "No one to write to yet.",
    [STR_COMPOSE_WRITE_TITLE] = "Your letter",
    [STR_COMPOSE_SUBJECT_HINT] = "About (you can skip this)",
    [STR_COMPOSE_TOO_LONG] = "That's as long as a letter can be.",
    [STR_COMPOSE_EMPTY] = "Write something first.",
    [STR_COMPOSE_SENT] = "Your letter is waiting for the runner.",
    [STR_COMPOSE_COUNTER_FMT] = "{}/{}",
    [STR_DRAFT_RESUME] = "Keep writing the letter you started?",
    [STR_DRAFT_LET_IT_GO] = "Let it go",
    [STR_OUTBOX_FULL] = "The bag is full. Send these first.",

    [STR_DRAFT_CONFLICT_PROMPT] = "You're still writing another letter.",
    [STR_DRAFT_KEEP_WRITING] = "Keep writing that one",
    [STR_DRAFT_START_NEW] = "Start a new one instead",

    [STR_OUTBOX_TITLE] = "On the road",
    [STR_OUTBOX_EMPTY] = "Nothing is waiting.",
    [STR_SEND_FAILED] = "This letter couldn't be sent. Ask your guardians about it.",
    [STR_SEND_FAILED_ACTION] = "Write it again",

    [STR_STORAGE_FAILED] = "That didn't save. Try again.",

    [STR_FAULT_CANT_REACH_HOME] = "Can't reach home.",
    [STR_FAULT_ASK_GUARDIANS] = "Something needs a grown-up. Ask your guardians.",
    [STR_FAULT_ROAD_BUSY] = "The road is busy. Trying again soon.",
    [STR_FAULT_CHARGE_ME] = "Nearly out of power. Please charge me.",
    [STR_FAULT_DIAGNOSTIC_FMT] = "For a grown-up: {}",

    [STR_PIN_PROMPT] = "Your number",
    [STR_PIN_WRONG] = "Not quite. Try again.",
    [STR_PIN_TRY_AGAIN_SOON] = "Wait a moment, then try again.",
    [STR_PIN_DIGIT_MARK] = "*",

    [STR_SETTINGS_TITLE] = "Settings",
    [STR_SETTINGS_FONT_SIZE] = "Text size",
    [STR_SETTINGS_FONT_NORMAL] = "Normal",
    [STR_SETTINGS_FONT_BIGGER] = "Bigger",
    [STR_SETTINGS_FRONTLIGHT] = "Light",
    [STR_SETTINGS_LIGHT_OFF] = "Off",
    [STR_SETTINGS_LIGHT_STEP_FMT] = "Step {}",
    [STR_SETTINGS_CONTACTS] = "Names and faces",
    [STR_SETTINGS_NICKNAME] = "What you call them",
    [STR_SETTINGS_KEEP_AT_TOP] = "Keep at the top",
    [STR_SETTINGS_WHAT_IT_TELLS] = "What my Chaski tells home",
    [STR_SETTINGS_LOG_EMPTY] = "Nothing has been told yet.",
    [STR_SETTINGS_ABOUT] = "About this Chaski",
    [STR_ABOUT_VERSION_FMT] = "Version {}",
    [STR_ABOUT_BATTERY_FMT] = "Battery {}%",
    [STR_ABOUT_SIGNAL_FMT] = "Signal {}",

    [STR_LOG_LINE_FMT] = "{} - {}",
    [STR_LOG_BATTERY_FMT] = "battery {}%",
    [STR_LOG_CHARGING] = "charging",
    [STR_LOG_SIGNAL_GOOD] = "good signal",
    [STR_LOG_SIGNAL_WEAK] = "weak signal",
    [STR_LOG_SIGNAL_NONE] = "no signal",
    [STR_LOG_NOTHING_WAITING] = "nothing waiting",
    [STR_LOG_ONE_WAITING] = "1 letter waiting",
    [STR_LOG_MANY_WAITING_FMT] = "{} letters waiting",
    [STR_LIST_SEPARATOR] = ", ",

    [STR_TIME_FMT] = "{} {}:{}",
    [STR_DAY_MON] = "Mon",
    [STR_DAY_TUE] = "Tue",
    [STR_DAY_WED] = "Wed",
    [STR_DAY_THU] = "Thu",
    [STR_DAY_FRI] = "Fri",
    [STR_DAY_SAT] = "Sat",
    [STR_DAY_SUN] = "Sun",
};

const char* S(chaski_string_id_t id) {
  if (id < 0 || id >= STR__COUNT || kStrings[id] == 0) return "";
  return kStrings[id];
}

const char* chaski_text(int id) { return S((chaski_string_id_t)id); }
