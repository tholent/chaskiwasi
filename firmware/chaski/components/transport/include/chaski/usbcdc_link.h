// The target-side SerialLink: the ESP32-S3's built-in USB-Serial-JTAG
// peripheral, which is the USB-C connector the developer already has plugged
// in (client §14, decision B.2).
//
// Target-only. The implementation is the one translation unit in this
// component that touches esp_* headers, and CMakeLists compiles it only under
// ESP-IDF, so the host tier keeps building with no toolchain
// (implementation-plan ground rule 3).
//
// The dev console shares this peripheral (sdkconfig.dev sets
// CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG), so log lines and frames interleave on
// the same pipe. That is by design and is why §14's framing carries a magic
// and a CRC: the host resynchronises past console text, and the bench captures
// that same text as C-19's evidence. A device log and a device wire on one
// cable is one cable's worth of ways to get it wrong, so neither side may ever
// assume the next byte is its own.
#pragma once

#include <memory>

#include "chaski/usbbridge.h"

namespace chaski::transport {

// NewUsbCdcLink installs the USB-Serial-JTAG driver and returns a link over
// it, or nullptr if the driver will not install. Call once.
std::unique_ptr<SerialLink> NewUsbCdcLink();

}  // namespace chaski::transport
