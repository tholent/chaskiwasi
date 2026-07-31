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
| Modem UART TX / RX / RTS / CTS | fixed by module | 115200 8N1, hardware handshaking |
| Charger status / VBUS sense | TBD | Charger-presence wake after an emergency wipe |

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
| — | — | — | — | — |
