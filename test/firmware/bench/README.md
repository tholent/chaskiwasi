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
idf.py -p /dev/ttyACM0 flash monitor      # ctrl-] to leave the monitor
```

You should see a `hello` line at boot. If the port is not `/dev/ttyACM0`, check
`ls /dev/ttyACM* /dev/ttyUSB*`; on Linux you may need to be in the `dialout`
group (`sudo usermod -aG dialout $USER`, then log out and back in).

**2. Bring the server up.** Wasi, `strip`, and maddy as the mail fixture — the
same stack the server's own e2e suite uses.

```sh
make up
```

**3. Provision the device** with the token that stack expects, and give it a
contact to write to. Both live in the compose stack's config; the device is
told its token over the control channel rather than being reflashed.

```sh
export CHASKI_BENCH_PORT=/dev/ttyACM0
export CHASKI_BENCH_TOKEN=<the dev bearer token from deploy/>
export CHASKI_BENCH_CONTACT=c_01          # a real, active id from the ayllu
```

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
| `sync reported fault "no_signal"` | the stack is down — `make up` |
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
