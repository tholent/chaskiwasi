// app_main — the composition root and the wake dispatcher.
//
// The ESP32-S3 reboots through here on EVERY wake; the modem does not (design
// §6.4). So this function is deliberately cheap: read why we woke, wire the
// seams to the implementations this build variant uses, do exactly one job,
// and go back to sleep.
//
// Wave 0 scaffold: the dispatch skeleton only. Wave 2 (2C) fills in the real
// wiring — see chaski-implementation-plan §4.
#include "chaski_strings.h"

extern "C" void app_main(void) {
  // Wake reasons, per client §6: RTC timer (poll the modem for a buffered
  // doorbell, sync when due), a keypress (open the UI), or USB attach.
  //
  // Nothing here may log letter content at any level, in any build (D-7).
  (void)S(STR_APP_NAME);
}
