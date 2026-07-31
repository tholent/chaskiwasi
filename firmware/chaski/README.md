# Chaski firmware

The device half of Chaskiwasi: an ESP32-S3 + Sequans GM02SP (DPTechnics
**Walter**) driving an e-ink panel and a physical keyboard.

Read first, in this order:

| Document | Standing |
|---|---|
| `specs/chaski-client.spec.md` | **Authoritative** for firmware behaviour. D-1…D-7, the C-table, Appendix B. |
| `specs/wasi-server-plan.md` | **Authoritative** for the wire (§4, §10, §12). Never restated here — cited. |
| `specs/chaski-implementation-plan.md` | Build order, seams, wave ownership, ground rules. |
| `docs/bringup.md` | Pins, board profiles, panel timings, measurement log. |

## Pinned versions

Frozen at CM0. Changing any of these is a deliberate, bench-tested act.

| | Version | Why it is pinned here |
|---|---|---|
| ESP-IDF | **v5.5.5** | See note below |
| walter-modem | `dptechnics/walter-modem ^1.5.0` | Vendor driver for the GM02SP; `components/modem` is its only caller |
| utf8proc (device) | 2.8.0 — Unicode **15.0.0** | Must agree with the server's segmenter |
| rivo/uniseg (server) | v0.4.7 — Unicode **15.0.0** | The reference for C-9's vectors (B.7) |

**On "LTS":** the implementation plan called for "the newest LTS". ESP-IDF has
no LTS designation — Espressif supports each minor release for 30 months. v5.5
is the newest 5.x line, which is what the vendor component ecosystem targets;
v6.0 is newer but ahead of the driver. Recorded as finding F-C1.

**The two Unicode versions must match.** If they diverge, a letter the compose
counter accepted can be rejected by the server as over-cap — a "couldn't send"
the child did nothing to earn. `make fw-vectors` regenerates the vectors and
C-9 is the referee (B.7).

## Building

Host tests and text gates need **no ESP-IDF**. This is the point of ground
rule 3 — logic components include no `esp_*` or FreeRTOS headers, so the entire
letter path is testable on a laptop.

```sh
make fw-hosttest     # CMake + GoogleTest over the logic components
make fw-gates        # C-15 vocabulary/address gates, Unicode-skew check
make check           # the repo gate: Go + the two above
```

The target build needs ESP-IDF v5.5.5:

```sh
. ~/esp/esp-idf/export.sh
make fw-build                    # or: cd firmware/chaski && idf.py build
make fw-check                    # host tests + gates + target build + C-16 symbol scan
```

Two build variants (client §3):

```sh
idf.py build                                              # prod
idf.py -D SDKCONFIG_DEFAULTS="sdkconfig.defaults;sdkconfig.dev" build   # dev
```

Neither compiles WiFi or BLE. The dev transport is USB, not a radio (B.2), and
C-16 scans **both** ELFs to keep that true.

## Layout

```
main/            app_main (wake dispatch), chaski_strings.c — ALL user-visible text
components/
  wire/          mirror of internal/protocol; fixtures keep it honest
  store/         letters, outbox, seen-ring, sync state (POSIX I/O only)
  ayllu/         contact snapshot + device-local cosmetic overlay (B.3)
  syncengine/    request assembly and the §5.2 apply order
  transport/     the entire network surface: modem (prod) | USB bridge (dev)
  layout/        grapheme-safe line breaking and pagination — owns every layout number
  panel/         SSD1680 driver + the two wipe orderings (§9, §9.1)
  power/         fuel gauge, the low-voltage backstop (B.13)
  input/         key seam; put-away and sync intercepted below UI dispatch
  pututu/        SMS doorbell verification
  kipu/          tier-1 health block + the readable log
  ui/            screens and flows
  modem/         walter-modem wrapper — target-only, no host build
```

## Two rules that are easy to break by accident

1. **`chaski_strings.c` holds every user-visible word.** Nothing else may
   contain a UI literal, and `pututu`/`ayllu`/`kipu` may never appear in it.
   `make fw-gates` fails the build otherwise. (The header is *not* named
   `strings.h`: that shadows POSIX `<strings.h>` for every file that sees this
   directory. Found the hard way — F-C2.)
2. **Nothing logs letter content, in any build, at any level** (D-7). The dev
   build is the one attached to a terminal someone can read over your shoulder,
   which is why C-19 greps *its* output (B.11).
