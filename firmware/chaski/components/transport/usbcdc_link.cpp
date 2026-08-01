// Target-only implementation of the USB-CDC SerialLink. See
// include/chaski/usbcdc_link.h; this file is compiled only under ESP-IDF.
#include "chaski/usbcdc_link.h"

#include "driver/usb_serial_jtag.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"

namespace chaski::transport {
namespace {

// The driver's ring buffers. A sync request with a full outbox is under 10 KB
// (server §4.1) and arrives in USB-sized chunks, so these size the burst the
// driver may hold while the app is busy applying a response, not a whole
// message.
constexpr int kRxBufferBytes = 2048;
constexpr int kTxBufferBytes = 2048;

class UsbCdcLink final : public SerialLink {
 public:
  bool Write(const std::uint8_t* data, std::size_t n) override {
    std::size_t sent = 0;
    while (sent < n) {
      const int wrote = usb_serial_jtag_write_bytes(data + sent, n - sent,
                                                    pdMS_TO_TICKS(kWriteTimeoutMs));
      // A host that is not reading fills the buffer and stalls. Reporting the
      // failure is right: syncengine treats it as no-signal, the letter waits
      // in the outbox, and nothing is acked (§5.3, D-5).
      if (wrote <= 0) return false;
      sent += static_cast<std::size_t>(wrote);
    }
    return true;
  }

  std::size_t Read(std::uint8_t* buf, std::size_t cap, int timeout_ms) override {
    const int got = usb_serial_jtag_read_bytes(buf, cap, pdMS_TO_TICKS(timeout_ms));
    return got > 0 ? static_cast<std::size_t>(got) : 0;
  }

  std::int64_t MonotonicMs() override { return esp_timer_get_time() / 1000; }

 private:
  static constexpr int kWriteTimeoutMs = 1000;
};

}  // namespace

std::unique_ptr<SerialLink> NewUsbCdcLink() {
  usb_serial_jtag_driver_config_t cfg = USB_SERIAL_JTAG_DRIVER_CONFIG_DEFAULT();
  cfg.rx_buffer_size = kRxBufferBytes;
  cfg.tx_buffer_size = kTxBufferBytes;
  if (usb_serial_jtag_driver_install(&cfg) != ESP_OK) return nullptr;
  return std::unique_ptr<SerialLink>(new UsbCdcLink());
}

}  // namespace chaski::transport
