# The bench tier

The rows of `specs/chaski-client.spec.md` §15 that need a real Chaski attached
to a real cable: **C-1**, the bench halves of **C-2** and **C-4**, **C-7**, and
**C-19**.

Everything else in the C-table is host-tested (`make fw-hosttest`, 277 cases) or
a build gate (`make fw-gates`). Those run anywhere. These do not, and with no
board attached every test here **skips with an explanation**. A skip is not a
pass — the whole reason the implementation plan carries a standing no-hardware
caveat is that a green suite has never meant a working device.

---

## Day one with hardware

Written for someone who has just unpacked a Walter, and it is late.

**1. Flash a dev build.** The bench needs the USB console and the control
channel, which production images do not contain at all.

```sh
. ~/esp/esp-idf/export.sh
cd firmware/chaski
idf.py -D SDKCONFIG_DEFAULTS="sdkconfig.defaults;sdkconfig.dev" build
idf.py -p $PORT flash monitor             # ctrl-] to leave the monitor
```

You should see a `hello` line at boot.

**Finding `$PORT`.** Walter has no USB-serial bridge chip — the USB-C port is
the ESP32-S3's own USB-Serial-JTAG peripheral — so it enumerates as a CDC
device and the name is OS-specific:

| | Port | Notes |
|---|---|---|
| Linux | `/dev/ttyACM0` | `ls /dev/ttyACM*`. Needs the `dialout` group: `sudo usermod -aG dialout $USER`, then log out and back in |
| macOS | `/dev/cu.usbmodem*` | `ls /dev/cu.usbmodem*`. No group needed. Use `cu.`, **not** `tty.` — the `tty.` node blocks on open waiting for carrier detect and will appear to hang |

Seeing `/dev/ttyUSB*` on Linux means a bridge chip, which Walter does not have
— you are looking at some other board.

**Before the first flash, decide about eFuses.** `sdkconfig.defaults` enables
flash encryption (D-4), and the first boot of such an image burns eFuses and
encrypts flash in place, irreversibly. That is correct for the product and a
poor first move on a new board, where it gives every later failure a second
possible cause. Add `sdkconfig.bringup` to the overlay list to defer it:

```sh
idf.py -B build-bringup \
  -D SDKCONFIG=build-bringup/sdkconfig.generated \
  -D SDKCONFIG_DEFAULTS="sdkconfig.defaults;sdkconfig.dev;sdkconfig.bringup" \
  build
idf.py -B build-bringup -p $PORT flash monitor
```

After the first configure those flags are cached in `build-bringup/`, so later
runs need only `-B build-bringup`. Read the header of `sdkconfig.bringup` for
what it costs while in use and when to drop it.

**If `cmake` fails on a missing submodule** (`components/mqtt/esp-mqtt` is
usually the first to error), the ESP-IDF checkout is incomplete rather than the
project being wrong: `cd ~/esp/esp-idf && git submodule update --init
--recursive`. `MINIMAL_BUILD` does not avoid this — IDF walks every component's
CMakeLists to build the dependency graph before pruning to what `main` requires.

**2. Bring the server up.** Wasi, `strip`, and maddy as the mail fixture — the
same stack the server's own e2e suite uses.

```sh
make up
```

**3. Provision the device** with the token that stack expects, and give it a
contact to write to. Both live in the compose stack's config; the device is
told its token over the control channel rather than being reflashed.

```sh
export CHASKI_BENCH_PORT=/dev/ttyACM0     # macOS: /dev/cu.usbmodem*
export CHASKI_BENCH_TOKEN=<the dev bearer token from deploy/>
export CHASKI_BENCH_CONTACT=c_01          # a real, active id from the ayllu
```

The full set of knobs, so none of them has to be found by reading the suite:

| Variable | Default | What it does |
|---|---|---|
| `CHASKI_BENCH_PORT` | — | The device node. **Unset means every test skips.** |
| `CHASKI_BENCH_WASI` | `https://127.0.0.1:18443/sync` | Where the harness forwards the device's sync frames. The default is the compose stack's published listener; override it when Wasi is somewhere else. |
| `CHASKI_BENCH_TOKEN` | — | Bearer token to `provision` the device with. Unset skips provisioning and uses whatever the device already holds. |
| `CHASKI_BENCH_CONTACT` | — | An active contact id to compose to. Unset skips the rows that need one. |

**If Wasi is on another machine** — a board on your laptop, the stack on a dev
box — note that compose publishes on `127.0.0.1` only, so it is not reachable
across the network as configured. An SSH tunnel is the least invasive fix:

```sh
ssh -L 18443:127.0.0.1:18443 <that-host>    # then the default URL just works
```

TLS verification is off in the harness (`OpenDevice(..., insecure: true)`), so
no CA material has to travel with it. That is a bench-only affordance: the
device's own trust path is pinned to the two private roots (§12, D-6) and is
exercised by C-7, not by this client.

**4. Run it.**

```sh
go test -tags bench -v ./test/firmware/bench/
```

**5. Record the run** in `RUNLOG.md`, one line, with the firmware sha. That file
is the bench analog of CI history — CI has no hardware, so a run that is not
written down did not happen.

### When it goes wrong

| Symptom | Cause |
|---|---|
| every test skips | `CHASKI_BENCH_PORT` unset, or the port node is absent |
| `device did not answer hello` | not a dev build, or the console is not on USB-Serial-JTAG |
| `sync reported fault "no_signal"` | the stack is down (`make up`), or `CHASKI_BENCH_WASI` points where nothing is listening. This fault is the harness failing to reach Wasi, not the device failing to reach the harness — check the URL before suspecting the board |
| `sync reported fault "provisioning"` | wrong bearer token; re-`provision` |
| `device refused compose: unknown_contact` | `CHASKI_BENCH_CONTACT` is not an active id |
| `N torn frame(s)` in C-19 | a bad cable or a baud mismatch; the run cannot answer C-19 either way |

---

## The control channel

A device with no keyboard cannot be made to compose a letter, so the bench
drives it over the link that is already there. Commands ride
`frame::Type::kCommand` and answers ride `kEvent` — two frame kinds §14's codec
reserved for exactly this.

**Dev builds only.** `main/bench_control.cpp` is not compiled into a production
image: `main/CMakeLists.txt` includes it only when the console is on the
USB-Serial-JTAG peripheral, which `sdkconfig.dev` turns on and
`sdkconfig.defaults` turns off. There is no runtime flag to reach it from a
release build, because a runtime flag is a thing that can be set.

### Rules

1. **One command at a time.** Every command is answered by exactly one event
   carrying the same `id`, and the host waits for it. This is not politeness:
   during a sync the transport owns the link and its decoder discards frame
   kinds that are not its own, so a command sent mid-sync is eaten, not queued.
2. **Some answers are a reboot.** `reboot`, and `sync` with `cut_at` armed,
   answer by resetting. The host waits for the unsolicited `hello` that every
   boot emits and reads its `boot` counter to know it happened.
3. **Letter content goes in, never out.** Commands carry the child's words into
   the device; no event carries one back. A stored letter is checked by sending
   a length and a CRC-32 for the device to compare on its own side, so only the
   verdict crosses the wire. D-7 holds here exactly as everywhere else.

### Commands

| `cmd` | Fields | Answers with |
|---|---|---|
| `hello` | — | `hello`: `boot`, `wake`, `variant`, `boot_synced`, `boot_fault`, `boot_stored`, `boot_acks` |
| `state` | — | `state`: `provisioned`, `has_cursor`, `ayllu_version`, `clock_valid`, `unread`, `sendable` |
| `letters` | — | `letters`: `total`, `unread`, and per-letter `letter_id`/`contact_id`/`body_len`/`trimmed`/`truncated` |
| `letter` | `letter_id`, `body_len`, `body_crc32` | `letter`: `body_matches` — the comparison happens on the device |
| `outbox` | — | `outbox`: `entries`, `sendable` |
| `compose` | `contact_id`, `body`, optional `subject` | `composed`: `local_id` |
| `sync` | optional `trigger`, optional `cut_at` | `synced`: `attempted`, `fault`, `stored`, `deduped`, `acks`, `rounds`, `more`, `incomplete`, `ayllu_updated`, `config_updated`, `backoff_ms` — or a reboot when `cut_at` fires |
| `mark_read` | `letter_id` | `ok` |
| `clear_cursor` | — | `ok` — stages the window resync C-2 needs |
| `provision` | `token` | `ok` |
| `factory_reset` | — | `ok` — forgets the letters, keeps the provisioning |
| `reboot` | — | a reboot, then `hello` |

Errors answer with `error` and a `why`: `bad_json`, `missing_field`,
`unknown_command`, `store_error`, `outbox_full_or_write_failed`.

### `cut_at`

`sync` with `cut_at` set to a step number of the §5.2 apply order resets the
device at that step. It is how C-4 exercises a power cut on real hardware,
where the interesting failure is a reset rather than an exception:

```
1 server_time   2 acks   3 ayllu   4 letters   5 config   6 cursor (last)
```

Cutting at 2 lands in the window a naive implementation gets wrong — after the
acks are applied, before the cursor is written. The device must come back
having lost no letter, and the server's ack ring (V-9) is what makes the
resulting duplicate send harmless.

---

## What this tier still does not cover

Hardware-blocked rows that need more than a board on a desk:

- **C-8 live**: a real pututu SMS from Hologram over LTE-M (needs a SIM and the
  modem path, Wave 4).
- **C-10 / C-24 live**: the wipe against a real panel, and the angled-light
  ghosting measurement that sets the flush pass count. Their *sequence* halves
  are host-tested against a recording panel; the privacy measurement is not a
  test, it is a photograph — see `firmware/chaski/docs/bringup.md`.
- **C-20**: the PPK2 power numbers. Nothing here measures current.
- **C-23**: the fuse ceremony, which is deliberately not automatable.
