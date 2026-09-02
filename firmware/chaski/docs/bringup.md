# Chaski bring-up

Hardware facts and measurements. Deliberately **not** in the spec: these churn
with board revisions, and nothing behavioural depends on them
(`specs/chaski-client.spec.md` §2).

Status: opened at CM0. Pin assignments are provisional until the first board.

## Board profiles

The layout engine is panel-agnostic on purpose — the 2.7"-vs-4.2" question is
still open (design §11), and the answer must not be a firmware rewrite. Panel
size and font metrics are the only place a panel number may appear.

| | 2.7" reference | 4.2" candidate |
|---|---|---|
| Part | GDEY027T91-FL02 | GDEY042T81-FL02 |
| Resolution | 264 × 176 | 400 × 300 |
| Controller | SSD1680Z8 | SSD1683 |
| Capacity at 6×13 | ~570 chars — one 500-grapheme letter is one screen | ~1500 chars |

Decide by cardboard mock-up before either panel is bonded (design §10).

## Pin map — PROVISIONAL

Fill from the Walter datasheet at first board bring-up. Every row is a guess
until measured.

| Function | Walter pin | Notes |
|---|---|---|
| EPD SPI CLK / MOSI / CS / DC / RST / BUSY | TBD | SSD1680, 4-wire SPI |
| Peripheral rail enable | TBD | MOSFET-switched; panel + frontlight boost (design §5.5) |
| Frontlight PWM | TBD | Constant-current boost, ≤12 V, default off |
| Fuel gauge SDA / SCL | TBD | MAX17048 class |
| **Fuel gauge ALRT** | **TBD — must be RTC-capable** | The §9.1 low-voltage backstop (B.13) |
| Keyboard (proto) SDA / SCL / IRQ | TBD | KeebDeck Basic over I2C |
| Keyboard (prod) matrix rows / cols | TBD | Direct scan + `ext1` any-key wake |
| Modem UART TX / RX / RTS / CTS | **48 / 14 / 21 / 47**, reset **45** | Fixed by the module. Not a guess: these are the defaults the vendored `dptechnics/walter-modem` compiles in (`WalterModem.cpp` CONFIG_INT block), so they are what the driver will drive whatever this table says |
| Charger status / VBUS sense | TBD | Charger-presence wake after an emergency wipe |

**Free for peripherals:** the boot-clean GPIOs are IO1, IO2, IO4–IO8, IO15–IO18
— eleven pins, none colliding with the modem's five above. The EPD needs six
(SCK, MOSI, CS, DC, RST, BUSY) and I2C needs two, which fits with three spare.
Cross-check against the DPTechnics pinout before soldering; this list came from
a third-party summary, unlike the modem row.

## First boot on a bare Walter — what is normal

Recorded from the first real board (2026-09-02) so the next person does not
debug any of it. All four of these look like faults and are not.

- **`E esp_littlefs: Corrupted dir pair` then `mount failed (-84). formatting...`**
  A new flash chip has never held a filesystem. `session.cpp` sets
  `format_if_mount_failed`, deliberately: a partition that will not mount
  belongs to a device that has either never stored a letter or has damaged
  flash, and the mailbox is canonical in both cases (design Principle 5). Does
  not recur after the first boot.
- **`W chaski: no provisioning namespace in nvs`** — the device has no token
  yet. Provisioning is a bench-control command, not a reflash.
- **A ~45 s silence after the sync frame goes out.** The dev transport is the
  USB bridge, and `kDefaultTimeoutMs` is 45000 — generous on purpose so a real
  bench sync across the compose stack is never cut off. With no harness
  listening, the full timeout elapses before `fault=no_signal`. It is a wait,
  not a hang.
- **Frame bytes interleaved with log lines**, e.g.
  `CHK1#{"cursor":"","ayllu_version":0}`. The console and the §14 bridge share
  one USB-Serial-JTAG pipe by design; that is precisely why frames carry a
  magic. It is also why C-19 treats a torn frame as INCONCLUSIVE rather than
  clean — those bytes land in the console capture it greps.

**`synced=1` alongside `fault=no_signal` is not a contradiction, and is worth
knowing before it costs someone an hour.** `RunResult::synced` means a sync was
*attempted*: `wake.cpp` sets it unconditionally after calling `jobs.Sync()`, and
it cannot mean anything else, because `Jobs::Sync` returns void and `wake::Run`
never learns the outcome. The outcome lives in `BoardJobs::last_sync()`, which
`app_main` reads separately. `app_main` gates `ScheduleNextSync` on it so that a
device which cannot reach home still retries on the normal cadence instead of
letting `next_sync_due_ms` go stale. The control channel already calls the same
field `attempted`; the log line and `boot_synced` are the ones that read wrong.

Also confirmed on this board: ESP32-S3 rev v0.2, 2 MB quad PSRAM (matching
WROOM-1-N16R2), `USB mode: USB-Serial/JTAG` — no bridge chip, which is why the
port is `/dev/cu.usbmodem*` on macOS and `/dev/ttyACM*` on Linux.

## Measurements to take, and what each decides

Nothing below is a formality — each result changes a constant in the firmware.

### Panel

- [ ] **Ghosting after a long partial-refresh session**, photographed at an
      angle in bright light, at 2/4/6 clear passes. *Decides:*
      `graceful_flush_passes` and `emergency_flush_passes`. This is a
      **privacy** test, not a cosmetic one (design §11) — the question is
      whether a letter is still legible, not whether the panel looks clean.
- [ ] Partial / fast / full refresh timings, and BUSY duration for each.
      *Decides:* the refresh-batching timer (§8.3) and the emergency wipe's
      time budget.

### Power (needs the PPK2 — a USB meter cannot see µA)

- [ ] Deep-sleep floor with the modem in PSM and the rail cut. *Decides:*
      whether the design §7 budget survives contact with the board. Target
      ~10 µA; the budget assumes it.
- [ ] Timer-wake cost (wake → modem SMS poll → sleep), no sync.
- [ ] Full sync cost over LTE-M.
- [ ] A scripted 15-minute compose session. *Decides:* whether refresh
      batching is working — design §7 says active composing dwarfs everything
      else, so this is the number that matters most.
- [ ] **Battery sag through an emergency flush at threshold, on a COLD
      battery** (§9.1, C-24). *Decides:* the final `kEmergencyMilliVolts` and
      whether the flush completes at all under the booster's load. Until this
      is measured, 3300 mV is provisional.
- [ ] MAX17048 VALRT/ALRT behaviour in practice, and active-vs-hibernate
      sampling latency. *Decides:* whether the backstop can be armed the way
      §9.1 assumes.

### Modem — verify with the vendor before CM3

These block Wave 4 rather than running alongside it (implementation-plan §4).

- [ ] Private-CA TLS profile mechanics on the GM02SP: can we install two custom
      roots and *only* those? D-6 depends on the answer.
- [ ] Does the TLS session survive PSM and ESP32 deep sleep? The power budget
      assumes the expensive state stays in the modem (design §6.4).
- [ ] Do PSM/eDRX timers survive carrier negotiation? They are *requested*, not
      guaranteed.
- [ ] Is a modem RING line routed to an RTC-capable pin? Optimisation only —
      the timer-wake SMS poll is the design, not a fallback.
- [ ] Can the GM02SP report *neighbouring* cells? Determines whether rank 2 of
      the v2 location ladder exists at all (design §4.2).

## Measurement log

Append results here with date and firmware sha. A number without those is not
a measurement.

| Date | FW sha | What | Result | Decided |
|---|---|---|---|---|
| 2026-09-02 | `162076d` | First power-on of a bare Walter, dev build + `sdkconfig.bringup`, macOS host | Boots, mounts (after first-boot format), reaches `control channel up (boot=1 wake=cold)` and emits `hello`. Sync attempted over the USB bridge with no harness listening → `fault=no_signal` after the full 45 s timeout, which is correct. | Nothing yet — this is the board-is-alive check, not a measurement. Unblocks the C-1 bench run. |
