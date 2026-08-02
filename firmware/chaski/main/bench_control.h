// The bench control channel: how a device with no keyboard is made to compose
// and sync (chaski-implementation-plan §4, client §15's bench tier).
//
// DEV BUILDS ONLY. bench_control.cpp is not compiled into a production image at
// all — main/CMakeLists.txt adds it only when the console is on the USB-Serial-
// JTAG peripheral, which is what sdkconfig.dev turns on and sdkconfig.defaults
// turns off. There is no runtime flag to get here from a release build, because
// a runtime flag is a thing that can be set.
//
// It rides frame::Type::kCommand and kEvent, which the §14 codec reserved for
// exactly this. Payloads are one UTF-8 JSON object each; the vocabulary is
// documented for humans in test/firmware/bench/README.md, which is the file to
// change first when this one grows a verb.
//
// Two rules the protocol depends on, both enforced by the host:
//
//  1. One command at a time. The device answers every command with exactly one
//     event carrying the same id, and the host waits for it. This is not
//     politeness: during a sync the transport owns the link, and its decoder
//     discards the frame kinds that are not its own — so a command sent while
//     a sync is in flight is eaten, not queued.
//  2. Three verbs answer with a reboot instead of an event (`reboot`, and
//     `sync` with cut_at armed). The host waits for the unsolicited `hello`
//     that every boot emits, and reads the boot count to know it happened.
//
// D-7 holds here as everywhere: commands carry the child's words INTO the
// device, and nothing carries them back out. No event contains a body, a
// subject, or a name — a letter's text is checked by sending a CRC-32 and a
// length for the device to compare against what it stored, so the comparison
// happens on the device and only the verdict crosses the wire.
#pragma once

#include "session.h"

namespace chaski::bench {

// BootSummary is what the wake that preceded the control loop did. It rides the
// `hello` event so a harness can tell a cold boot's automatic sync (§6, and the
// recovery C-4 is watching for) from one it asked for itself.
struct BootSummary {
  int boot_count = 0;
  const char* wake_reason = "";
  bool synced = false;
  const char* fault = "none";
  int letters_stored = 0;
  int acks_applied = 0;
};

// Serve emits `hello` and then answers commands until the board is reset. It
// does not return: on a bench the board is attached to a host and to power, and
// deep sleep would end the session and take the console with it.
[[noreturn]] void Serve(app::Session& s, const BootSummary& boot);

}  // namespace chaski::bench
