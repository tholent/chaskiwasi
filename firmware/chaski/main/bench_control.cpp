// Implementation of the bench control channel — see bench_control.h for what
// it is, why it exists, and the two rules the host has to keep.
//
// This translation unit is compiled into dev images only.
#include "bench_control.h"

#include <cstdint>
#include <cstdio>
#include <string>
#include <vector>

#include "cJSON.h"
#include "esp_log.h"
#include "esp_system.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "chaski/frame.h"
#include "chaski/store.h"
#include "chaski/syncengine.h"
#include "chaski/transport.h"
#include "chaski/wire.h"

namespace chaski::bench {
namespace {

namespace frame = transport::frame;

constexpr const char* kTag = "bench";

// A poll long enough to keep the loop off the CPU and short enough that a
// command is answered without a human noticing the wait.
constexpr int kReadTimeoutMs = 100;
constexpr std::size_t kReadChunk = 512;

// A command is a small JSON object plus, at most, one letter. The cap is the
// frame codec's, restated so a malformed length cannot make this loop the place
// that runs out of heap.
constexpr std::size_t kMaxLettersListed = 64;

// ---- JSON out -------------------------------------------------------------
//
// Written by hand rather than with cJSON: every value emitted here is an int, a
// bool, a fixed identifier, or an id ("l-" + hex, "o-" + digits), so the
// escaping below is complete for what actually crosses. Building the document
// as a string also makes it obvious, at every call site, that no field carries
// a body or a subject (D-7).

void AppendJsonString(std::string* out, const std::string& v) {
  out->push_back('"');
  for (const char c : v) {
    switch (c) {
      case '"':
        out->append("\\\"");
        break;
      case '\\':
        out->append("\\\\");
        break;
      case '\n':
        out->append("\\n");
        break;
      case '\r':
        out->append("\\r");
        break;
      case '\t':
        out->append("\\t");
        break;
      default:
        if (static_cast<unsigned char>(c) < 0x20) {
          char esc[8];
          std::snprintf(esc, sizeof(esc), "\\u%04x",
                        static_cast<unsigned>(static_cast<unsigned char>(c)));
          out->append(esc);
        } else {
          out->push_back(c);
        }
        break;
    }
  }
  out->push_back('"');
}

class Event {
 public:
  Event(int id, const char* name) {
    buf_ = "{\"id\":" + std::to_string(id) + ",\"ev\":";
    AppendJsonString(&buf_, name);
  }

  Event& Int(const char* key, long long v) {
    Key(key);
    buf_ += std::to_string(v);
    return *this;
  }

  Event& Bool(const char* key, bool v) {
    Key(key);
    buf_ += v ? "true" : "false";
    return *this;
  }

  Event& Str(const char* key, const std::string& v) {
    Key(key);
    AppendJsonString(&buf_, v);
    return *this;
  }

  // Raw appends an already-formed JSON value, for the two array fields.
  Event& Raw(const char* key, const std::string& json) {
    Key(key);
    buf_ += json;
    return *this;
  }

  std::string Done() const { return buf_ + "}"; }

 private:
  void Key(const char* key) {
    buf_ += ',';
    AppendJsonString(&buf_, key);
    buf_ += ':';
  }

  std::string buf_;
};

bool Emit(transport::SerialLink* link, const std::string& json) {
  std::string framed;
  if (!frame::Encode(frame::Type::kEvent, json, &framed)) return false;
  return link->Write(reinterpret_cast<const std::uint8_t*>(framed.data()), framed.size());
}

void EmitError(transport::SerialLink* link, int id, const char* why) {
  // `why` is always one of this file's own constants. The command that caused
  // it is never echoed: a malformed compose command contains a letter.
  Emit(link, Event(id, "error").Str("why", why).Done());
}

// ---- JSON in --------------------------------------------------------------

bool Field(const cJSON* root, const char* key, std::string& out) {
  const cJSON* item = cJSON_GetObjectItemCaseSensitive(root, key);
  if (!cJSON_IsString(item) || item->valuestring == nullptr) return false;
  out = item->valuestring;
  return true;
}

long long FieldNumber(const cJSON* root, const char* key, long long fallback) {
  const cJSON* item = cJSON_GetObjectItemCaseSensitive(root, key);
  if (!cJSON_IsNumber(item)) return fallback;
  return static_cast<long long>(item->valuedouble);
}

const char* AckStatusName(wire::AckStatus s) {
  switch (s) {
    case wire::AckStatus::kSent:
      return "sent";
    case wire::AckStatus::kRejectedInactive:
      return "rejected_inactive";
    case wire::AckStatus::kRejectedUnknownContact:
      return "rejected_unknown_contact";
    case wire::AckStatus::kInvalid:
      return "invalid";
    case wire::AckStatus::kRejectedUndeliverable:
      return "rejected_undeliverable";
    case wire::AckStatus::kUnknown:
      return "unknown";
  }
  return "unknown";
}

syncengine::Trigger TriggerNamed(const std::string& name) {
  // "user" is the deliberate sync-key press, and it is the only trigger that
  // clears a provisioning halt (§5.3). The bench needs to name it explicitly
  // for exactly that reason — C-7 turns on the difference.
  if (name == "user") return syncengine::Trigger::kUserKey;
  if (name == "doorbell") return syncengine::Trigger::kPututu;
  if (name == "outbound") return syncengine::Trigger::kOutboundQueued;
  return syncengine::Trigger::kScheduled;
}

std::uint32_t Crc32Of(const std::string& s) {
  return frame::Crc32(reinterpret_cast<const std::uint8_t*>(s.data()), s.size());
}

// ---- commands -------------------------------------------------------------

void CmdHello(transport::SerialLink* link, int id, app::Session& s, const BootSummary& boot) {
  Emit(link, Event(id, "hello")
                 .Str("variant", "dev")
                 .Int("boot", boot.boot_count)
                 .Str("wake", boot.wake_reason)
                 .Bool("provisioned", s.provisioned())
                 .Bool("boot_synced", boot.synced)
                 .Str("boot_fault", boot.fault)
                 .Int("boot_stored", boot.letters_stored)
                 .Int("boot_acks", boot.acks_applied)
                 .Done());
}

void CmdState(transport::SerialLink* link, int id, app::Session& s) {
  const store::SyncState st = s.state()->Snapshot();
  Emit(link, Event(id, "state")
                 .Int("letters", static_cast<long long>(s.letters()->Count()))
                 .Int("unread", static_cast<long long>(s.letters()->UnreadCount()))
                 .Int("outbox", static_cast<long long>(s.outbox()->All().size()))
                 .Int("sendable", static_cast<long long>(s.outbox()->SendableCount()))
                 .Int("contacts", static_cast<long long>(s.contacts()->Merged().size()))
                 .Int("ayllu_version", s.contacts()->Version())
                 .Int("local_id_high_water", static_cast<long long>(st.local_id_high_water))
                 // The cursor is opaque and stays on the device; whether there
                 // is one is the only part a harness has any business knowing
                 // (server §4.4).
                 .Bool("has_cursor", !st.cursor.empty())
                 .Bool("clock_valid", s.clock()->Valid())
                 .Bool("provisioned", s.provisioned())
                 .Done());
}

void CmdLetters(transport::SerialLink* link, int id, app::Session& s, const cJSON* root) {
  std::size_t limit = static_cast<std::size_t>(FieldNumber(root, "limit", kMaxLettersListed));
  if (limit == 0 || limit > kMaxLettersListed) limit = kMaxLettersListed;

  std::string array = "[";
  bool first = true;
  for (const store::StoredLetter& l : s.letters()->ListNewestFirst(limit)) {
    if (!first) array += ',';
    first = false;
    array += "{\"id\":";
    AppendJsonString(&array, l.letter.id);
    array += ",\"unread\":";
    array += l.unread ? "true" : "false";
    array += '}';
  }
  array += ']';

  // Ids only. I-1 allows a letter id where correlation is needed and allows
  // nothing else; the subject would be the easy thing to add here and is the
  // exact thing D-7 forbids.
  Emit(link, Event(id, "letters")
                 .Raw("entries", array)
                 .Int("total", static_cast<long long>(s.letters()->Count()))
                 .Done());
}

void CmdLetter(transport::SerialLink* link, int id, app::Session& s, const cJSON* root) {
  std::string letter_id;
  if (!Field(root, "letter_id", letter_id)) {
    EmitError(link, id, "missing_field");
    return;
  }

  store::StoredLetter stored;
  if (!s.letters()->Get(letter_id, stored)) {
    Emit(link, Event(id, "letter").Bool("present", false).Done());
    return;
  }

  // The device compares; the host learns only the verdict. Sending back a
  // digest of the body would be content-derived output on a cable a bench
  // captures, and the whole point of C-19 is that nothing derived from a letter
  // leaves this device by any path but the wire it was delivered on.
  //
  // CRC-32 rather than a hash: it is already linked for the frame trailer, and
  // the question here is "is this the letter I injected", not "can an adversary
  // forge a match". Length is checked alongside it.
  const bool have_body = cJSON_HasObjectItem(root, "body_crc32");
  const auto want_crc = static_cast<std::uint32_t>(FieldNumber(root, "body_crc32", 0));
  const auto want_len = static_cast<std::size_t>(FieldNumber(root, "body_len", -1));
  const bool body_matches = have_body && stored.letter.body.size() == want_len &&
                            Crc32Of(stored.letter.body) == want_crc;

  Event ev(id, "letter");
  ev.Bool("present", true)
      .Bool("unread", stored.unread)
      .Str("contact_id", stored.letter.contact_id)
      .Int("body_len", static_cast<long long>(stored.letter.body.size()))
      .Bool("trimmed", stored.letter.trimmed)
      .Bool("truncated", stored.letter.truncated);
  if (have_body) ev.Bool("body_matches", body_matches);
  Emit(link, ev.Done());
}

void CmdOutbox(transport::SerialLink* link, int id, app::Session& s) {
  std::string array = "[";
  bool first = true;
  for (const store::OutboxEntry& e : s.outbox()->All()) {
    if (!first) array += ',';
    first = false;
    array += "{\"local_id\":";
    AppendJsonString(&array, e.local_id);
    array += ",\"contact_id\":";
    AppendJsonString(&array, e.contact_id);
    array += ",\"rejected\":";
    array += e.rejected ? "true" : "false";
    array += ",\"status\":";
    AppendJsonString(&array, AckStatusName(e.reject_status));
    array += ",\"body_len\":";
    array += std::to_string(e.body.size());
    array += '}';
  }
  array += ']';

  Emit(link, Event(id, "outbox")
                 .Raw("entries", array)
                 .Int("sendable", static_cast<long long>(s.outbox()->SendableCount()))
                 .Done());
}

void CmdCompose(transport::SerialLink* link, int id, app::Session& s, const cJSON* root) {
  std::string contact_id;
  std::string body;
  if (!Field(root, "contact_id", contact_id) || !Field(root, "body", body)) {
    EmitError(link, id, "missing_field");
    return;
  }
  std::string subject;
  (void)Field(root, "subject", subject);  // optional: the server generates one

  // §5.6: until the first sync of a power cycle the clock is invalid, and a
  // composed_at of 0 is the honest value — the server stamps the letter anyway.
  const std::int64_t composed_at = s.clock()->Valid() ? s.clock()->NowEpoch() : 0;

  std::string local_id;
  if (!s.outbox()->Add(contact_id, subject, body, composed_at, local_id)) {
    // B.9: at the cap the real UI parks the text as the draft. There is no UI
    // here, so the harness is told plainly rather than quietly losing a letter.
    EmitError(link, id, "outbox_full_or_write_failed");
    return;
  }
  ESP_LOGI(kTag, "composed %s (%d bytes)", local_id.c_str(), static_cast<int>(body.size()));
  Emit(link, Event(id, "composed").Str("local_id", local_id).Done());
}

void CmdSync(transport::SerialLink* link, int id, app::Session& s, const cJSON* root) {
  std::string trigger;
  (void)Field(root, "trigger", trigger);

  const int cut_at = static_cast<int>(FieldNumber(root, "cut_at", 0));
  s.SetCutStep(cut_at);

  const app::SyncReport r = app::RunSync(s, TriggerNamed(trigger));

  // Only reached when the cut did not fire.
  s.SetCutStep(0);

  Emit(link, Event(id, "synced")
                 .Bool("attempted", r.attempted)
                 .Str("fault", app::FaultName(r.outcome.fault))
                 .Int("stored", r.outcome.letters_stored)
                 .Int("deduped", r.outcome.letters_deduped)
                 .Int("acks", r.outcome.acks_applied)
                 .Int("rounds", r.outcome.rounds)
                 .Bool("more", r.outcome.more)
                 .Bool("incomplete", r.outcome.apply_incomplete)
                 .Bool("ayllu_updated", r.outcome.ayllu_updated)
                 .Bool("config_updated", r.outcome.config_updated)
                 .Int("backoff_ms", r.backoff_ms)
                 .Done());
}

// Dispatch answers exactly one command with exactly one event, except where the
// command's answer is a reboot.
void Dispatch(transport::SerialLink* link, app::Session& s, const BootSummary& boot,
              const std::string& payload) {
  cJSON* root = cJSON_ParseWithLength(payload.data(), payload.size());
  if (root == nullptr) {
    EmitError(link, 0, "bad_json");
    return;
  }

  const int id = static_cast<int>(FieldNumber(root, "id", 0));
  std::string cmd;
  if (!Field(root, "cmd", cmd)) {
    EmitError(link, id, "missing_field");
    cJSON_Delete(root);
    return;
  }
  ESP_LOGI(kTag, "command %s id=%d", cmd.c_str(), id);

  if (cmd == "hello") {
    CmdHello(link, id, s, boot);
  } else if (cmd == "state") {
    CmdState(link, id, s);
  } else if (cmd == "letters") {
    CmdLetters(link, id, s, root);
  } else if (cmd == "letter") {
    CmdLetter(link, id, s, root);
  } else if (cmd == "outbox") {
    CmdOutbox(link, id, s);
  } else if (cmd == "compose") {
    CmdCompose(link, id, s, root);
  } else if (cmd == "sync") {
    CmdSync(link, id, s, root);
  } else if (cmd == "mark_read") {
    std::string letter_id;
    if (!Field(root, "letter_id", letter_id)) {
      EmitError(link, id, "missing_field");
    } else if (!s.letters()->MarkRead(letter_id)) {
      EmitError(link, id, "store_error");
    } else {
      // Read state is device-local and no read receipt ever crosses the wire
      // (client §1.1). The harness asserts that by watching the server, not by
      // being told so here.
      Emit(link, Event(id, "ok").Done());
    }
  } else if (cmd == "clear_cursor") {
    // The device has no UI path to a window resync; the server produces one
    // after a restore from backup (server §4.4). This is how C-2 stages that
    // without corrupting the server's state to get there.
    if (!s.state()->SetCursor("")) {
      EmitError(link, id, "store_error");
    } else {
      Emit(link, Event(id, "ok").Done());
    }
  } else if (cmd == "provision") {
    std::string token;
    if (!Field(root, "token", token)) {
      EmitError(link, id, "missing_field");
    } else if (!s.SetDeviceToken(token)) {
      EmitError(link, id, "store_error");
    } else {
      Emit(link, Event(id, "ok").Done());
    }
  } else if (cmd == "factory_reset") {
    // Erases the letters, keeps the provisioning: on this device a factory
    // reset means "forget the letters", not "forget who you are" (§4.2, §12).
    if (!s.Reformat()) {
      EmitError(link, id, "store_error");
    } else {
      Emit(link, Event(id, "ok").Done());
    }
  } else if (cmd == "reboot") {
    cJSON_Delete(root);
    ESP_LOGW(kTag, "rebooting on request");
    esp_restart();
  } else {
    EmitError(link, id, "unknown_command");
  }

  cJSON_Delete(root);
}

}  // namespace

void Serve(app::Session& s, const BootSummary& boot) {
  transport::SerialLink* link = s.link();
  if (link == nullptr) {
    // Nothing can be said and nothing can be heard. Idling is better than
    // restarting into the same condition: a boot loop would bury the one log
    // line that explains what happened.
    ESP_LOGE(kTag, "no usb link; the control channel cannot start");
    for (;;) {
      vTaskDelay(pdMS_TO_TICKS(1000));
    }
  }

  ESP_LOGI(kTag, "control channel up (boot=%d wake=%s)", boot.boot_count, boot.wake_reason);
  CmdHello(link, 0, s, boot);

  frame::Decoder decoder;
  std::uint8_t chunk[kReadChunk];
  for (;;) {
    const std::size_t n = link->Read(chunk, sizeof(chunk), kReadTimeoutMs);
    if (n > 0) decoder.Append(chunk, n);

    frame::Type type = frame::Type::kCommand;
    std::string payload;
    while (decoder.Next(&type, &payload) == frame::DecodeStatus::kFrame) {
      // A late response to a sync that already gave up is the transport's
      // business, not this loop's; ignoring what is not a command is also what
      // lets the host end grow a frame kind without reflashing the device.
      if (type != frame::Type::kCommand) continue;
      Dispatch(link, s, boot, payload);
    }
  }
}

}  // namespace chaski::bench
