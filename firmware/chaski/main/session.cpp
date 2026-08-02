// Implementation of the composition root's wiring — see session.h.
//
// Content-free logging throughout, in every build (D-7, C-19): counts, status
// names, and error codes. Never a body, never a subject, never the bearer
// token, not even truncated.
#include "session.h"

#include <optional>
#include <utility>

#include "esp_littlefs.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "nvs.h"
#include "nvs_flash.h"

#include "chaski/wire.h"

#if CHASKI_DEV_BUILD
#include "chaski/usbbridge.h"
#include "chaski/usbcdc_link.h"
#endif

namespace chaski::app {
namespace {

constexpr const char* kTag = "chaski";

// A bearer token is provisioned, not typed. The bound is a sanity check on
// what NVS hands back, so a corrupted entry cannot become a multi-kilobyte
// header on every sync.
constexpr std::size_t kMaxTokenBytes = 512;

// The version reported in the kipu block and to the server. It is a constant
// until wave 5B's release pipeline stamps a real one; a wrong-looking constant
// is better than esp_app_get_description() reporting the build machine's git
// state as a product version.
constexpr const char* kFirmwareVersion = "0.1.0-dev";

}  // namespace

std::int64_t Clock::MonotonicMs() const { return esp_timer_get_time() / 1000; }

std::int64_t Clock::NowEpoch() const {
  if (!valid_) return 0;  // §5.6: blank, never a wrong date
  return epoch_at_mark_ + (MonotonicMs() - mono_at_mark_) / 1000;
}

void Clock::SetFromServer(std::int64_t epoch) {
  // A response with no server_time leaves the clock invalid rather than
  // setting it to 1970 (C-21).
  if (epoch <= 0) return;
  epoch_at_mark_ = epoch;
  mono_at_mark_ = MonotonicMs();
  valid_ = true;
}

bool Session::Open() {
  esp_err_t err = nvs_flash_init();
  if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
    // A partition written by a different NVS layout, or one with no free
    // pages. Erasing loses the provisioning, which is recoverable over USB;
    // refusing to boot is not.
    ESP_LOGW(kTag, "nvs unusable (%s); erasing", esp_err_to_name(err));
    if (nvs_flash_erase() == ESP_OK) err = nvs_flash_init();
  }
  if (err != ESP_OK) {
    ESP_LOGE(kTag, "nvs init failed: %s", esp_err_to_name(err));
    return false;
  }

  esp_vfs_littlefs_conf_t conf = {};
  conf.base_path = kLetterRoot;
  conf.partition_label = kLetterPartition;
  // A device whose letter partition will not mount is a device that has never
  // stored a letter, or one whose flash is damaged. Formatting is right in
  // both cases: the mailbox is canonical and the device is a derived view
  // (design Principle 5), so nothing unique is lost.
  conf.format_if_mount_failed = 1;
  err = esp_vfs_littlefs_register(&conf);
  if (err != ESP_OK) {
    ESP_LOGE(kTag, "letters partition would not mount: %s", esp_err_to_name(err));
    return false;
  }
  fs_mounted_ = true;

  if (!OpenStores()) return false;
  LoadDeviceToken();
  BuildEngine();
  return true;
}

bool Session::OpenStores() {
  letters_ = store::OpenLetterStore(kLetterRoot);
  outbox_ = store::OpenOutbox(kLetterRoot);
  seen_ = store::OpenSeenRing(kLetterRoot, store::kMinSeenIds);
  state_ = store::OpenStateStore(kLetterRoot);
  settings_ = store::OpenSettingsStore(kLetterRoot);
  drafts_ = store::OpenDraftStore(kLetterRoot);
  contacts_ = ayllu::Open(kLetterRoot);

  if (letters_ && outbox_ && seen_ && state_ && settings_ && drafts_ && contacts_) return true;
  ESP_LOGE(kTag, "opening the durable stores failed");
  return false;
}

void Session::CloseStores() {
  engine_.reset();
  contacts_.reset();
  drafts_.reset();
  settings_.reset();
  state_.reset();
  seen_.reset();
  outbox_.reset();
  letters_.reset();
}

bool Session::Reformat() {
  if (!fs_mounted_) return false;
  CloseStores();

  const esp_err_t err = esp_littlefs_format(kLetterPartition);
  if (err != ESP_OK) {
    ESP_LOGE(kTag, "formatting the letters partition failed: %s", esp_err_to_name(err));
    return false;
  }
  if (!OpenStores()) return false;
  BuildEngine();
  ESP_LOGI(kTag, "letters partition formatted");
  return true;
}

void Session::LoadDeviceToken() {
  device_token_.clear();

  nvs_handle_t h = 0;
  esp_err_t err = nvs_open(kNvsNamespace, NVS_READONLY, &h);
  if (err != ESP_OK) {
    // A factory-fresh device has no namespace yet. That is not an error here:
    // it produces a 401 the child sees as "ask your guardians" (§5.3), which
    // is exactly what an unprovisioned device should show.
    ESP_LOGW(kTag, "no provisioning namespace in nvs (%s)", esp_err_to_name(err));
    return;
  }

  std::size_t len = 0;
  err = nvs_get_str(h, kNvsKeyDeviceToken, nullptr, &len);
  if (err == ESP_OK && len > 1 && len <= kMaxTokenBytes) {
    std::string value(len, '\0');
    err = nvs_get_str(h, kNvsKeyDeviceToken, value.data(), &len);
    if (err == ESP_OK) {
      value.resize(len - 1);  // nvs_get_str counts the terminator
      device_token_ = std::move(value);
    }
  }
  nvs_close(h);

  // The token itself is never logged, not even truncated: a near-miss is as
  // sensitive as a hit (server §4.1's own rule, applied on this end).
  ESP_LOGI(kTag, "device token %s", device_token_.empty() ? "absent" : "present");
}

bool Session::SetDeviceToken(const std::string& token) {
  if (token.size() >= kMaxTokenBytes) return false;

  nvs_handle_t h = 0;
  esp_err_t err = nvs_open(kNvsNamespace, NVS_READWRITE, &h);
  if (err != ESP_OK) {
    ESP_LOGE(kTag, "opening nvs for provisioning failed: %s", esp_err_to_name(err));
    return false;
  }
  err = nvs_set_str(h, kNvsKeyDeviceToken, token.c_str());
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  if (err != ESP_OK) {
    ESP_LOGE(kTag, "writing the device token failed: %s", esp_err_to_name(err));
    return false;
  }

  device_token_ = token;
  BuildEngine();  // the transport holds the header by value
  ESP_LOGI(kTag, "device token stored (%d bytes)", static_cast<int>(token.size()));
  return true;
}

std::uint64_t Session::PututuCounterSeen() const {
  nvs_handle_t h = 0;
  if (nvs_open(kNvsNamespace, NVS_READONLY, &h) != ESP_OK) return 0;
  std::uint64_t v = 0;
  // Absent means zero, which is what a device that has never accepted a
  // doorbell should report (server §10.3).
  if (nvs_get_u64(h, "pututu_hw", &v) != ESP_OK) v = 0;
  nvs_close(h);
  return v;
}

void Session::BuildEngine() {
  engine_.reset();
  transport_.reset();

#if CHASKI_DEV_BUILD
  // §14: the dev transport is USB-CDC to tools/chaskibridge, which forwards to
  // a real Wasi with this header untouched. The link outlives the transport —
  // the driver is installed once per boot.
  if (link_ == nullptr) link_ = transport::NewUsbCdcLink();
  if (link_ == nullptr) {
    ESP_LOGE(kTag, "usb-cdc link unavailable; this boot has no road");
  } else {
    transport_ = std::make_unique<transport::UsbBridgeTransport>(
        link_.get(), device_token_.empty() ? std::string() : "Bearer " + device_token_);
  }
#endif

  if (transport_ == nullptr) {
    // Production images until wave 4A lands ModemTransport. Letters wait in
    // the outbox and nothing about the UI reads as broken (§5.3), which is the
    // same shape as no signal — because that is what it is.
    ESP_LOGW(kTag, "no transport in this build; sync is unavailable");
    return;
  }

  syncengine::Deps d;
  d.transport = transport_.get();
  d.letters = letters_.get();
  d.outbox = outbox_.get();
  d.seen = seen_.get();
  d.state = state_.get();
  d.contacts = contacts_.get();
  d.clock = &clock_;
  d.on_config = [this](const wire::DeviceConfig& c) {
    // §5.5 field by field; absent fields leave the current value alone
    // (F-C10). `rat` also has to reach the modem, which is wave 4A's — until
    // then it is stored and not pushed, which the settings store records.
    if (settings_) settings_->ApplyConfig(c);
  };
  d.kipu = []() -> std::optional<wire::Kipu> {
    // No block at all until there is a fuel gauge and a modem to fill one
    // (wave 4C, 4A). wire::Kipu has no "unknown" for battery_pct or rssi, so a
    // block sent now would report a healthy device at 0% on no network — a
    // guardian-visible lie about exactly the thing the kipu exists to report
    // (§13). The field is optional on the wire; absent is the honest value.
    return std::nullopt;
  };
  d.pututu_counter_seen = [this]() { return PututuCounterSeen(); };

#if CHASKI_DEV_BUILD
  d.fault_hook = [this](int step) {
    if (cut_step_ <= 0 || step != cut_step_) return;
    ESP_LOGW(kTag, "bench: cutting at apply step %d", step);
    esp_restart();
  };
#endif

  syncengine::Options o;
  o.firmware_version = kFirmwareVersion;
  engine_ = syncengine::New(d, o);
}

#if CHASKI_DEV_BUILD
void Session::SetCutStep(int step) { cut_step_ = step; }
#endif

const char* FaultName(syncengine::Fault f) {
  switch (f) {
    case syncengine::Fault::kNone:
      return "none";
    case syncengine::Fault::kNoSignal:
      return "no_signal";
    case syncengine::Fault::kCantReachHome:
      return "cant_reach_home";
    case syncengine::Fault::kProvisioningFault:
      return "provisioning_fault";
    case syncengine::Fault::kRoadBusy:
      return "road_busy";
    case syncengine::Fault::kServerFault:
      return "server_fault";
  }
  return "unknown";
}

SyncReport RunSync(Session& s, syncengine::Trigger t) {
  SyncReport r;

  syncengine::Engine* e = s.engine();
  if (e == nullptr) {
    // No transport in this image. Letters wait in the outbox, visibly "waiting
    // for the runner", and nothing reads as broken (§5.3).
    r.outcome.fault = syncengine::Fault::kNoSignal;
    ESP_LOGW(kTag, "sync skipped: this build has no transport");
    return r;
  }

  // §5.3: a 401 does not back off, it stops — until a deliberate sync-key
  // press. Trigger::kUserKey is that press, and it is the only trigger allowed
  // through a halt.
  if (t != syncengine::Trigger::kUserKey && e->NextBackoffMs() == syncengine::kBackoffHalted) {
    r.outcome.fault = syncengine::Fault::kProvisioningFault;
    r.backoff_ms = syncengine::kBackoffHalted;
    ESP_LOGW(kTag, "sync skipped: provisioning fault holds until a key press");
    return r;
  }

  r.attempted = true;
  r.outcome = e->RunOnce(t);
  r.backoff_ms = e->NextBackoffMs();

  // Counts and a fault name. Letter ids would be legitimate here (I-1 allows
  // ids for correlation); bodies, subjects and names never are (D-7).
  ESP_LOGI(kTag, "sync fault=%s stored=%d deduped=%d acks=%d rounds=%d more=%d incomplete=%d",
           FaultName(r.outcome.fault), r.outcome.letters_stored, r.outcome.letters_deduped,
           r.outcome.acks_applied, r.outcome.rounds, r.outcome.more ? 1 : 0,
           r.outcome.apply_incomplete ? 1 : 0);
  return r;
}

}  // namespace chaski::app
