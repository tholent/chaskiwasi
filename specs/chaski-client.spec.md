# Chaski Client Specification

Status: draft for review · Companion to `specs/wasi-server-plan.md` (the wire
contract) and `specs/chaskiwasi-design-spec.md` (hardware and device UX).
Scope: the firmware that runs on the child's device ("Chaski") — an ESP32-S3 +
Sequans GM02SP module (DPTechnics **Walter**) driving an e-ink panel and a
physical keyboard.

Authority: **the server plan's wire contract (server §4, §10, §12) is
authoritative for everything that crosses the wire** — this document cites it
and never restates it normatively. This document is authoritative for firmware
behaviour. Where it supersedes the design spec's device sections, Appendix B
records the supersession, in the same spirit as the server plan's Appendix A.

---

## 0. Reading guide and vocabulary

- Citations: **server §n** = `wasi-server-plan.md`, **design §n** =
  `chaskiwasi-design-spec.md`, **A.n** = the server decision log, **B.n** = the
  client decision log (Appendix B here). **I-n** = the five system invariants
  (server §1); **D-n** = the device invariants (§1 below). **C-n** = the client
  verification table (§15).
- RFC 2119 keywords (MUST/SHOULD/MAY) carry their usual meaning.
- "Grapheme" always means an extended grapheme cluster per Unicode UAX #29 —
  same definition as server §0. Character caps and line breaking operate on
  graphemes, never bytes or code points.
- **Vocabulary boundary (design §0.1), made structural:** every string a person
  can see lives in one strings table (`main/chaski_strings.c`), and `pututu`, `ayllu`,
  and `kipu` never appear in it. Wire field names (`ayllu_version`,
  `pututu_counter`) are exempt — the wire is machine-facing. Enforced by test
  (C-15), not discipline. The device's user-facing vocabulary is the game-mail
  register (design §4): letters, **waiting → on the road → arrived**, and the
  system sender is **"Wasi"** (A.16).

## 1. Device invariants

Stated once, enforced everywhere, each backed by a named test in §15. These are
the on-device projections of I-1…I-5 plus the design principles that live in
firmware.

**D-1 · The screen reveals nothing at rest.** Every transition to sleep runs
the full wipe sequence (§9) first — timeout, put-away, and low battery alike.
The cover screen shows the wordmark, battery state, and a mail-flag glyph when
unread letters exist — never a count, never a name, never content (B.5). E-ink
bistability means a skipped wipe persists past a dead battery; the wipe is
proactive, never a shutdown step (design §4.1). A hardware low-voltage alert
backstops the one remaining window — awake with content when the battery dies
suddenly — so even a lying fuel gauge or a hung UI cannot leave a letter on a
dead panel (§9.1, B.13). (C-10, C-12, C-24)

**D-2 · No email address ever exists on the device.** The firmware has no code
path that parses, stores, renders, or logs an address. The wire never carries
one (I-2); the strings table is scanned for address-shaped text as a tripwire.
(C-15)

**D-3 · Production firmware contains exactly one radio path.** LTE-M/NB-IoT
via the GM02SP, driven over UART. WiFi and BLE are not compiled into **any**
build, dev builds included — the dev transport is USB, not a radio (§14, B.2).
GNSS is unused in v1 (v2: §17). Verified by scanning the linked ELF for radio
symbols. (C-16)

**D-4 · Everything persisted is encrypted at rest.** ESP32-S3 flash encryption
covers the letter store and app image; NVS encryption covers secrets (device
token, pututu key, PIN); secure boot v2 means only project-signed images run.
This device will get lost (design §5.6); losing it must leak nothing but the
hardware. (C-23, and the CM4 fuse ceremony §12)

**D-5 · An acked letter is never resent; an unacked letter is never dropped.**
The device half of server §4.7. Every ack status is terminal; anything unacked
stays in the outbox, visible to the child as "on the road", and is retried
every sync forever. A letter being *written* is covered too: drafts survive
sleep, timeout, and power loss (§11.3). (C-3, C-4, C-17)

**D-6 · The device trusts exactly the two pinned private CAs.** Nothing else —
no public roots, ever (server §12.2, A.7). TLS trust failure shows the
distinct, visible "can't reach home" state, and the trust store is updatable
through the USB firmware path. These are the two firmware requirements the
server spec imposes beyond the wire, honoured here. (C-7)

**D-7 · No letter content in any log output, at any level, in any build.**
Dev-build serial consoles included — a bench log grep is part of the e2e gate,
exactly like the server's V-11. Log letter ids and local ids where correlation
is needed. (C-19)

### 1.1 Anti-requirements

Deliberately absent, mirroring server §1.1 — each of these is an easy
"improvement" that breaks a promise:

| Deliberately absent | Because |
|---|---|
| Any reply/thread concept in the UI — no reply button, no thread view | A.1: the product is passing notes. "Write to this person" is the only verb. |
| Read receipts, delivery state, or engagement data sent to the server | The server holds no engagement state (server §1.1); the kipu carries health, not behaviour. |
| Any awareness of the Held folder | Quarantine is a guardian-side concept; the device must not become a surveillance indicator. |
| Layout metrics from the server (`pages`, `chars_per_page`) | A.10: rendering is device-owned because font size is a runtime accessibility setting. |
| A To: field, address entry, or contact editing beyond cosmetics | Design §4: pick a person by face and name, then write. The guardian owns the list (I-4). |
| Any connection the device did not initiate | Server §4: the device initiates every exchange; the pututu SMS carries only "sync now". |
| An OTA firmware path | Design §6.5: updates are physical, over USB-C, signed. Config still moves over the wire. |

## 2. Hardware baseline

What the firmware is written against. Part selection rationale lives in design
§5; this section only fixes the contract between firmware and board.

- **Module:** Walter — ESP32-S3-WROOM-1-N16R2 (16 MB flash, 2 MB PSRAM) +
  Sequans GM02SP (LTE-M, NB-IoT, GNSS), modem on UART (115200 8N1, hardware
  handshaking, AT commands). The modem is separately powered and holds network
  registration, PDP context, and TLS sessions across ESP32 deep sleep (design
  §6.4). Prototype target is the bare Walter board.
- **Panel:** Good Display **GDEY027T91-FL02** (2.7", 264×176, SSD1680, 4-grey,
  SPI) is the *reference* panel. The 2.7"-vs-4.2" question is still open
  (design §11); therefore the layout engine is **panel-agnostic** — panel
  dimensions and fonts are compile-time board-profile constants, and no other
  module may know them. Frontlight via a constant-current boost driver, PWM
  dimmed, default off, on a physical button (design §5.4).
- **Keyboard:** one input seam (§10), two implementations: **KeebDeck Basic
  over I2C** for the prototype; **direct GPIO matrix scan** with `ext1`
  any-key deep-sleep wake for production (design §5.2). The put-away and sync
  keys are ordinary keys given special handling in the input layer, not
  discrete switches (design §4.1).
- **Power:** single-cell LiPo, fuel gauge (MAX17048 class) on I2C, Walter's
  MOSFET-switched peripheral rail feeding panel + frontlight boost. The rail is
  cut only after the panel is in deep sleep (§9). USB-C for charging and
  flashing (native USB Serial/JTAG — no UART bridge chip). **The fuel gauge's
  ALRT pin is wired to an RTC-capable GPIO** and configured with an
  undervoltage alert (§9.1); while the device is awake the gauge runs active
  sampling (~250 ms) — hibernate's ~45 s sample period is acceptable only in
  sleep, where the alert doesn't matter (§9.1).
- **No battery-backed RTC.** A 32.768 kHz crystal disciplines deep-sleep
  timing; wall-clock time comes from `server_time` on every sync (§5.6).
- Pin map, matrix wiring, and board-profile constants live in a bring-up
  document (`firmware/chaski/docs/bringup.md`), not in this spec — they will
  churn with hardware revisions and nothing behavioural depends on them.

## 3. Platform and build

- **ESP-IDF, pinned to one LTS release** (choose the newest LTS at CM0 and
  record it in `firmware/chaski/README`; upgrading is a deliberate act, tested
  on the bench, never drive-by). Pure IDF: no Arduino component (B.1).
- **Language:** C++ (no exceptions, no RTTI) where object lifetime and seams
  earn it — the UI state machine, the stores — plain C elsewhere. Match IDF
  idiom; don't fight the framework.
- **Dependencies**, kept deliberately short:
  - `walter-modem` (DPTechnics' ESP-IDF component) for the GM02SP: AT
    transport, PDP/PSM management, TLS profiles, HTTP, SMS.
  - **utf8proc** for grapheme segmentation (B.7). Parity with the server's
    `rivo/uniseg` is enforced by shared test vectors, not hope (C-9).
  - LittleFS (`joltwallet/littlefs` component) on a dedicated partition for
    the letter store.
  - A panel driver written in-repo (§8, B.1) — SSD1680 is small and
    well-documented; GxEPD2 stays as a *reference implementation* to crib
    waveform details from, not a dependency.
- **Build variants:**
  - **prod** — modem transport only, console logging at `ERROR` (content-free,
    D-7), secure-boot-signed, flash-encryption release fuses.
  - **dev** — adds the USB-CDC bridge transport (§14) and verbose logging
    (still content-free, D-7 — C-19 runs against dev builds). No radio is ever
    added by a build flag; the dev/prod difference is USB framing and log
    level, so a variant mix-up cannot violate D-3.
- **Sleep-path config:** `CONFIG_BOOTLOADER_SKIP_VALIDATE_IN_DEEP_SLEEP=y`,
  ROM bootloader logging off (design §6.4).
- **Build gates**, run by `make check` alongside the Go gates: radio-symbol
  scan on both variant ELFs (C-16), strings-table scan (C-15), and a lint that
  flags user-facing string literals outside `main/strings.c`.

### 3.1 Repository layout (monorepo, B.6)

```
firmware/chaski/
  main/                app entry, wake dispatch, strings.c (the only UI text)
  components/
    transport/         sync transport seam: modem impl + usbbridge impl (§14)
    modem/             walter-modem wrapper: PSM, SMS poll, TLS profiles, RAT
    syncengine/        request assembly, response application, backoff (§5)
    store/             LittleFS letter store, outbox, seen-ring, state (§4)
    ayllu/             contact snapshot + device-local cosmetic overlay (§4.4)
    pututu/            SMS token verify, counter, rate limit (§7)
    panel/             SSD1680 driver, wipe sequence (§8, §9)
    layout/            grapheme line-breaking, pagination, fonts (§8)
    input/             matrix/I2C scan, put-away interception (§10)
    ui/                screens and flows (§11)
    kipu/              tier-1 block assembly + on-device readable log (§13)
  docs/bringup.md      pins, board profiles, panel timings
tools/chaskibridge/    Go host tool: USB-CDC ⇄ Wasi proxy (§14)
tools/graphvectors/    Go tool: emits grapheme test vectors from uniseg (C-9)
test/firmware/         host tests + bench (hardware-in-loop) suite (§15)
```

`internal/protocol` remains the single written description of the wire in Go;
the firmware's structs mirror it and C-1 keeps them honest end to end.

## 4. Storage model

Three tiers, by durability need. All flash writes go through an atomic
write-then-rename discipline on LittleFS (power cut mid-write must never eat
the previous version), and flash writes are kept rare — they cost energy and
wear (design §6.4).

### 4.1 LittleFS (encrypted flash partition)

| File(s) | Contents |
|---|---|
| `letters/<id>` | One delivered letter: id, contact id, subject, date, body, `trimmed`/`truncated`/`degraded` flags, local unread bit. Write is idempotent by id — re-delivery overwrites identically. |
| `outbox/<local_id>` | One queued letter: local id, contact id, optional subject, body, compose timestamp. Removed only on ack (any status — all are terminal, server §4.7). |
| `draft` | The single in-progress composition (§11.3). |
| `state` | Sync cursor (opaque, echoed verbatim — server §4.4), `ayllu_version`, seen-id ring (≥1000 ids, ≈12 KB — the server §4.5 wire contract), `local_id` counter high-water, letter-retention bookkeeping. |
| `ayllu` | Contact snapshot as last received, plus the device-local cosmetic overlay (§4.4). |
| `settings` | Font size, frontlight step, applied server config (`max_letter_chars`, `sync_interval_s`, PIN state, cover options). |

**Letter retention (B.8):** the device is a *view*, the mailbox is the archive
(design Principle 5). The store keeps the most recent `letters_keep` letters
(default 200, matching the server's `resync_window`) and evicts oldest beyond
that with no ceremony — the UI's oldest-letter row says the archive continues
at home. Eviction removes the file, not the seen-ring entry, so an evicted
letter is not re-delivered.

### 4.2 NVS (encrypted)

Provisioned secrets and the one counter that must survive power loss:
device bearer token, pututu HMAC key, Wasi sync URL, highest-accepted pututu
counter (§7), and the PIN when enabled (§11.5). Written at provisioning time
(§12) and on the rare pututu/PIN change — near-zero wear.

### 4.3 RTC slow memory (survives deep sleep, not power loss)

Wake bookkeeping only, never the sole copy of anything durable: wake reason,
boot counter, unread-flag mirror for fast cover rendering, last
SMS-triggered-wake timestamp (rate limit, §7), next-scheduled-sync time.
**The cursor is durable in `state`, not RTC-only** (B.12, superseding the
design §6.4 suggestion): a battery death would otherwise cost a full window
resync per event. Losing RTC memory is always safe — every field is
reconstructible or conservative.

### 4.4 The ayllu snapshot and the cosmetic overlay (B.3)

The server's ayllu payload (contacts with `name`, `active`, `pinned`, `order`,
`portrait`) replaces the snapshot wholesale whenever `ayllu_version` changes.
The youth's cosmetics — nickname, ordering, pinning, portrait choice — are a
**device-local overlay keyed by contact id**, applied on top at render time and
never sent anywhere. Server values act as guardian-set defaults for contacts
the overlay doesn't cover; a guardian's *name* change always shows (the overlay
stores a nickname alongside, rendered as the primary label when set). A factory
reset restores letters (window resync) but loses cosmetics; that cost is
accepted and recorded in B.3, along with the wire-extension alternative if it
ever hurts in practice.

Tombstones (`active: false`) are stored and rendered like any contact — name
on old letters, absent from the compose picker — which is exactly what `c_sys`
("Wasi") requires too, with no special case (server §7.2, A.15, A.16). A
letter whose contact id is unknown even after applying the ayllu block renders
under a neutral fallback label and is **kept** — never dropped for a lookup
miss (C-14).

## 5. The sync engine

The device half of server §4. One transaction shape, initiated only by the
device, triggered by: the sync key, the scheduled interval (`sync_interval_s`),
a pututu wake (§7), and a queued outbound letter at the next wake.

### 5.1 Request assembly

Per server §4.2: current cursor (verbatim — the device never parses it, server
§4.4), `ayllu_version`, `pututu_counter_seen` (§7), the tier-1 kipu block
(§13), and up to the full outbox as `outbound` entries. `local_id` comes from
a monotonically increasing persistent counter and is **never reused**, even
across power loss (C-5) — the server's 4096-entry ack ring dedups by it, and
reuse would alias two different letters.

### 5.2 Response application order — pinned for crash safety

```
1. apply server_time to the clock (§5.6)
2. process acks: remove each acked letter from the outbox (durable),
   surface reject statuses (§5.4)
3. apply the ayllu block if present (durable) — before letters, so names resolve
4. for each letter: seen-ring check → write letter file (durable) → add id
   to ring; unknown ids fall back per §4.4
5. apply the config block (§5.5)
6. write the new cursor to state (durable) — LAST
7. if more: true, sync again immediately (≤ 10 rounds, server §4.6);
   the drain counts as one sync event
```

Every crash lands safe: before step 6 the old cursor stands, the server
re-delivers, and the ring (step 4 ordering) absorbs the repeats; a crash
between SMTP-send-side events is the server's problem and already specified
(server §4.7). A replayed request is always safe because the server made it so
— the device's only obligations are the verbatim cursor, unique local ids, and
the ring.

### 5.3 Failure handling

Per the server §4.1 status table, each with a distinct device state (§11.6):

| Condition | Behaviour |
|---|---|
| `200` | Apply per §5.2 |
| `401` | **Provisioning-fault screen**; stop retrying until the next manual sync-key press. This means the token was rotated or the device was pointed somewhere wrong — a guardian problem, not a road problem. |
| `503` + `Retry-After` | Honour the header; show "the road is busy" only if the sync was user-initiated |
| TLS trust failure | **"Can't reach home"** state (D-6) — visually distinct from no-signal |
| Anything else / transport error | Retry the identical request with capped exponential backoff (30 s, 2 min, 10 min, then next scheduled wake). Background retries never wake the screen. |

No signal is not an error state on this device: letters wait in the outbox,
visibly "waiting for the runner", and nothing about the UI reads as broken
(design Principle 4).

### 5.4 Ack semantics in the UI

All ack statuses are terminal (server §4.7); on any ack the letter leaves the
outbox. `sent` → the letter's row shows "on the road" history normally. Any
`rejected_*` or `invalid` → the letter moves to a visible "couldn't send" state
that **keeps the child's text** — one key re-opens it as a new draft — and
shows the single string *"This letter couldn't be sent. Ask your guardians
about it."* The device never distinguishes the reject reasons to the child;
the distinctions exist for guardians, server-side.

### 5.5 Config application

The `config` block is applied field-by-field; unknown fields are ignored
(forward compatibility). `max_letter_chars` takes effect for the *next*
composition; `rat` is pushed to the modem (design §6.2); `sync_interval_s`
reschedules the timer; PIN enable/disable per §11.5; cover options per §9.
Config is never persisted as authoritative anywhere but `settings` — the
server can always overwrite it (design §6.5: config moves freely, firmware
does not).

### 5.6 Time

The clock is valid from the first sync after power-up and disciplined by
`server_time` on every sync (drift between syncs is irrelevant at letter
timescales). Until valid, letter dates render as blank rather than wrong, and
compose timestamps are deferred (stamped at sync time by the server anyway).
The device makes no timekeeping promises to anyone — it has no battery RTC and
does not pretend otherwise.

## 6. Sleep and wake

The design §6.4 architecture, made normative:

- **Deep sleep is the resting state.** Target: the ESP32-S3 in deep sleep, the
  GM02SP in PSM (TAU 30–60 min), panel in deep sleep behind the cut rail —
  single-digit µA at the battery (design §7 budget; C-20 measures it).
- **Wake sources:** RTC timer (every 10–15 min), `ext1` keypress (production
  matrix; the KeebDeck Basic prototype wakes on its interrupt line), USB
  attach. The wake path reads the wake reason and dispatches: timer → modem
  SMS poll (§7) and, when the scheduled interval or a doorbell says so, a
  sync; key → UI wake (cover → PIN if enabled → inbox).
- **Expensive state lives in the modem** (design §6.4): the firmware never
  power-cycles the GM02SP in normal operation, never re-attaches per sync, and
  reuses the modem's held TLS session where the stack allows. The ESP32
  rebuilds its own cheap state from flash on every wake.
- **Active-use power discipline:** the radio stays down while composing;
  light-sleep between keystrokes; display refresh batching per §8.3. These are
  the levers that matter — design §7 shows composing dwarfs everything else.
- **Inactivity timeout ~45 s** → wipe → sleep. **Battery < 5 %** → wipe to the
  charge-me cover and refuse to open content until charging (design §4.1).
- After any *background* sync that stored new letters, the cover is re-rendered
  (flag up) before returning to sleep — the mail flag is the whole point of the
  timer wake.

## 7. Pututu handling

The device rules of server §10.2, verbatim in force:

- On timer wake, poll the modem for buffered SMS. For each message: parse
  `CH1.<counter>.<mac>`; verify the HMAC-SHA256 MAC with the pututu key from
  NVS; accept only if `counter` strictly exceeds the highest accepted value;
  persist the new high-water to NVS before acting on it.
- An accepted token schedules an immediate sync. **All failures are silent** —
  no response, no wake, no UI, no log above debug (and even debug logs the
  counter, never speculation about senders — there are none to speculate
  about; the token carries no content by contract).
- **Rate limit:** at most one SMS-triggered wake per 5 minutes regardless of
  validity, tracked in RTC memory; after power loss the limiter starts
  conservative (treat the first minute after boot as inside the window).
- `pututu_counter_seen` rides every sync request so a restored-from-backup
  Wasi heals its counter (server §10.3). The device needs no logic beyond
  reporting its high-water — the healing is server-side. (C-8)

## 8. Display and rendering

### 8.1 Panel driver

An in-repo SSD1680 driver (B.1) exposing exactly what the product needs:
partial refresh (typing), fast refresh (page turns), full refresh
(anti-ghosting), the **multi-pass clear waveform** (§9 — a single full refresh
is not a wipe, design §4.1), deep-sleep entry, and BUSY-wait. 4-grey mode MAY
be used for reading screens; compose uses 1-bit partial refresh. Waveform
timings are board-profile data, cribbed against GxEPD2's SSD1680 support as
reference.

### 8.2 Layout engine

- **Line breaks fall on grapheme-cluster boundaries, never inside one** — the
  server §4.9 wire contract, tested against shared vectors (C-9). Break at
  spaces where possible, mid-word at cluster boundaries when a word exceeds
  the line.
- **Pagination is computed on-device from the current font** (A.10). Font size
  is a runtime setting on the settings screen — at least two sizes in v1, with
  the reference 6×13 face making one 500-grapheme letter ≈ one 2.7" screen.
  Changing font size repaginates instantly and locally; nothing is
  re-downloaded.
- **Glyph coverage is honest (B.10):** bundled bitmap fonts cover Latin plus
  common diacritics; anything else renders as a visible substitution glyph —
  never a crash, never a dropped grapheme, never a mid-cluster split. Emoji
  rendering is a v2 question; the *counting* of emoji is correct in v1 (they
  are graphemes like any other).
- `trimmed` renders as a trailing "…" marker; `truncated` renders as *"the
  letter continues in the archive at home"* on the last page (server §4.3
  invites exactly this distinction). `degraded` renders nothing — it is a
  server-side bookkeeping flag and the next sync cleans it up.

### 8.3 Refresh discipline

Batch partial refreshes at word boundaries or a ~200 ms idle timer — never
per keystroke — and light-sleep between them (design §7's largest lever). Full
refresh on entering a reading screen after N partials (board-profile constant)
to bound ghosting; ghosting is a *privacy* parameter here, not cosmetics
(design §11), and the wipe sequence owns the worst case.

## 9. Screen wipe and the cover

The design §4.1 requirements, normative for firmware:

**Triggers:** inactivity timeout (~45 s) · **put-away key** (immediate, no
confirmation, from any screen — §10) · battery < 5 % SOC (wipe to charge-me
cover) · **hardware low-voltage alert** (~3.3 V — the emergency path, §9.1).

**Sequence, in exactly this order** (C-10 asserts it against a
command-recording panel mock):

```
1. multi-pass clear waveform (alternating black/white flush)
2. wait for BUSY to deassert
3. render the cover screen
4. wait for BUSY to deassert
5. SSD1680 deep-sleep command
6. cut the peripheral rail
7. ESP32-S3 deep sleep
```

The flashing is not hidden — it is the child's visible confirmation that the
letter is gone (design §4.1).

**The cover** shows: the Chaski wordmark or road motif, battery state, and —
when any stored letter is unread — a **raised mail-flag glyph, with no count
and no names** (B.5). It must read as a resting object, never as broken: no
blank white panel. The charge-me cover is the same composition with the
battery element emphasised. Cover composition options may be tuned via the
pushed `cover` config value; no option may add content, senders, or counts.

### 9.1 Emergency wipe — the hardware low-voltage backstop (B.13)

The graceful < 5 % path is a software poll: it assumes the fuel gauge's SOC
estimate is honest and the firmware is healthy. Neither is guaranteed —
gauge drift, cold-weather voltage sag, or a hung task can carry a device with
a letter on screen straight past 5 % to a dead battery, and e-ink then shows
that letter indefinitely (design §4.1). The backstop is a **hardware
undervoltage interrupt**: the MAX17048's VALRT alert (20 mV steps) on its ALRT
pin (§2), threshold ~3.3 V, final value set at bring-up (§16).

**Scope: armed while awake only.** A sleeping device already ran the wipe and
shows the cover, which is safe past battery death by design (D-1). The alert
therefore needs no deep-sleep wake path and adds zero idle cost; it exists for
exactly one state — awake with content on screen.

The full ladder:

| Layer | Trigger | Behaviour |
|---|---|---|
| Graceful | ~5 % SOC, software poll | Full multi-pass wipe → charge-me cover → refuse content (§9) |
| **Emergency** | VALRT hardware alert (~3.3 V) | The sequence below |
| Last resort | ESP32-S3 brownout detector | Reset/death — no time to drive a 3–4 s waveform. Residue here is the accepted loss, minimised by the layers above |

**Sequence**, run from a high-priority handler independent of UI dispatch —
the same below-the-UI placement as put-away (§10), and for the same reason: a
wedged screen must not be able to swallow it.

```
1. mask input; abandon any sync in flight
2. modem off — dropping the burst load lets the battery voltage
   recover, buying headroom to finish
3. wait for any in-progress refresh: BUSY must deassert. NEVER abort
   a waveform mid-flight — freezing the panel mid-wipe is worse than
   not wiping (design §4.1)
4. flush: two black/white cycles (pass count tuned by the angled-light
   ghosting measurement, design §11 — it is a privacy test)
5. if voltage still holds, render the charge-me cover; otherwise stop
   on white — acceptable for this path only (the no-blank-panel rule
   is UX guidance for the resting cover, and a dead white screen
   reveals nothing)
6. SSD1680 deep-sleep command
7. cut the peripheral rail
8. ESP32-S3 deep sleep, waking on charger/VBUS presence ONLY — never
   on keys, so a curious key-press cannot drain the pack past the
   brownout floor
```

**False positives are cheap, so bias toward wiping on doubt.** An LTE TX
burst can sag a mid-charge battery through the threshold momentarily, and a
cold battery reads low at every SOC. But an unnecessary emergency wipe
destroys nothing — letters are in encrypted flash and the draft is autosaved
(§11.3); the worst case is an unexpected trip to the charge-me cover. Debounce
is therefore minimal: after step 2, one VCELL re-read ~250 ms later MAY cancel
the wipe if the voltage has recovered with margin; anything more elaborate is
risk in the wrong direction. Cold firing early is the conservative direction
and is accepted — it costs winter runtime, never privacy.

**What this does not fix, stated honestly:** instantaneous power loss — a
yanked pack, a connector bounce on a hard drop — beats any electronic
response. The per-sleep wipe (D-1) is what keeps that residual exposure to
active-use seconds rather than hours; no threshold closes it.

## 10. Input

- One input seam with two implementations (§2): I2C keypad (prototype) and
  direct matrix scan with debounce (production). Everything above the seam is
  identical.
- **The put-away scancode is intercepted in the input layer, below UI
  dispatch** (design §4.1): no screen, however wedged, sees the key before the
  wipe path runs. The task watchdog covers the residual case of a hung input
  layer itself — its expiry path also runs the wipe before reset. (C-11)
- The **sync key** is likewise handled below UI dispatch: it triggers a sync
  from any screen. It never reveals content — on the cover it syncs without
  unlocking (the flag may go up; nothing else changes), PIN or no PIN.
- Key positions, the tactile cap for put-away, and layout legends remain open
  hardware questions (design §11) — firmware sees scancodes from the seam and
  is indifferent.

## 11. UI flows

All flows speak through `strings.c` (§0). The register is the game-mail story:
a letter is **waiting** (outbox), **on the road** (unacked after a sync
attempt — from the child's view these are one state: with the runner), or
**arrived**.

### 11.1 Inbox

A list of letters, newest first: sender label (overlay nickname or server
name), subject, date, unread marker. Opening a letter marks it read locally
(never on the wire — §1.1). Tombstoned contacts' letters render normally with
the name; notice letters render from "Wasi". No threads, no grouping (A.1).

### 11.2 Compose

Pick a person **first** — a picker of active contacts, pinned first, then
overlay order, portraits as 1-bit glyphs from the built-in set. Tombstones and
`c_sys` never appear here. Then write: body field with a live grapheme counter
against `max_letter_chars`, optional single-line subject (skippable; the
server generates one from the body otherwise, server §6.2). Send moves the
letter to the outbox and shows it "waiting for the runner"; the sync key or
next wake takes it from there.

### 11.3 Drafts never die

The in-progress composition is autosaved to `draft` on every wipe trigger and
every ~30 s of typing. Waking after a timeout mid-letter offers to continue
the draft. One draft slot in v1 — starting a new letter with a draft pending
asks which to keep. This is D-5's spirit applied to words not yet sent: the
device never loses something the child wrote.

### 11.4 Outbox and couldn't-send

The outbox screen shows queued letters and their state. Acked-rejected letters
surface per §5.4 — text preserved, guardian-facing next step, one key to
recompose.

### 11.5 PIN (guardian-pushed, optional — B.4)

Disabled by default. A guardian may push a 4–6 digit PIN via the config block;
the device then requires it on wake from the cover before any content shows.
Wrong entries back off (1 s, doubling, capped at 60 s) — there is no
data-destroying attempt limit; flash encryption already protects the stored
letters, and a forgotten PIN three states away must not brick the inbox.
**Recovery is remote and guardian-shaped:** clearing the PIN in Wasi's config
takes effect at the device's next background sync, which happens on the timer
regardless of the lock. Sync and put-away work without the PIN; nothing else
does.

### 11.6 Fault states

Distinct, honest, child-readable, and content-free: **can't reach home** (TLS
trust, D-6), **ask your guardians** (401 provisioning fault), **the road is
busy** (503 on a user-initiated sync), **charge me** (battery floor). Each
names an action a child can actually take; none exposes an error code as the
primary text (codes go on a diagnostics line for a guardian's eyes).

### 11.7 Settings

Font size (§8.2) · frontlight step (§2) · cosmetic contact editing (§4.4) ·
**"what my Chaski tells home"** — the plain-language kipu log (design §3.7's
third transparency mechanism, §13) · about screen (firmware version, battery,
signal). Nothing here touches the contact list's membership, the radio, or
the archive.

## 12. Security and provisioning

- **Secure boot v2 + flash encryption**, enabled by eFuse in the CM4 ceremony:
  dev units run with dev fuses (reflashable, encryption in development mode);
  child-carried units burn release fuses — only project-signed images boot,
  flash reads back ciphertext, JTAG disabled. The signing key joins the CA
  keys in the offline ceremony story (server §12.2, `docs/` ceremony guide).
- **Per-device provisioning** happens at flash time: a small tool (grown
  alongside `tools/chaskibridge`) generates an encrypted factory NVS image
  carrying the device token, pututu key, and sync URL. Nothing secret is ever
  typed on the device; the device has no provisioning UI at all.
- **Trust store:** the two CA roots are compiled into the signed image and
  written to the GM02SP's TLS profile at boot when they differ from what the
  modem holds — which is what makes the trust store updatable via the USB
  firmware path (D-6). TLS is executed by the modem's stack against those
  pinned roots; the private-CA profile configuration is flagged
  verify-with-vendor (§16).
- **Firmware updates are physical**: ESP Web Tools page hosted on Wasi,
  guardian clicks Install over USB-C (design §6.5). Signed images only;
  physical access alone flashes nothing unsigned.
- **Threat model, briefly:** a lost/stolen device leaks nothing (D-1 at rest,
  D-4 in flash, first names at most by design of the wire); a hostile finder
  with USB gets a signed-image-only boot chain and encrypted flash; a hostile
  network gets a client that trusts only two offline CAs and initiates every
  exchange; a coercive reader of the child's screen is met by put-away (§10)
  — one key, any screen, no confirmation.

## 13. Kipu, device side

Each sync request carries the tier-1 block (server §4.8): battery %, charging
state, RAT, signal, queue depth, firmware version — ≤ 512 bytes, health only,
never position in v1. The settings screen renders the last sent blocks as
plain language — *"Tue 15:04 — battery 64%, good signal, 1 letter waiting"* —
so the child can always answer "what does it know about me" without asking
anyone (design §3.7). The device keeps only what that screen shows (a short
ring); the kipu is designed to be forgotten (A.6).

## 14. Dev transport — the USB bridge (B.2)

The transport seam is one function: submit a sync request body with
authentication, get back a status and response body.

- **prod:** the modem implementation — HTTPS POST via the GM02SP against the
  pinned CAs.
- **dev:** a framed USB-CDC protocol (length-prefixed request out, status +
  length-prefixed response in). **`tools/chaskibridge`** (Go) sits on the host
  end and forwards to any Wasi — normally the `deploy/compose.dev.yml` stack
  with maddy — passing the device's bearer header through untouched, so the
  firmware exercises real auth against a real Wasi with the real V-table
  fixtures. The bridge adds nothing and interprets nothing; it is a wire, not
  a mock.

Everything above the seam — sync engine, stores, dedup, acks, UI — is
identical in both variants, which is what makes the bench suite (§15)
meaningful. `chaskisim` remains the server's own harness; the firmware does
not reuse it, but C-9's shared vectors and C-1's round-trip keep the two
implementations converging on the same wire.

## 15. Verification — the C-table

Three tiers: **host** tests (firmware logic compiled for the host, no
hardware), **bench** tests (real firmware, dev build, USB bridge, against the
compose stack — the client's analog of the V-table run), and **gates** (build
scans, power measurements). Named `TestC<n>_<Name>` where applicable, mirroring
the server convention.

| # | Tier | Asserts |
|---|---|---|
| C-1 | bench | Full round-trip: compose → outbox → sync via bridge → letter lands in maddy (V-1's fixtures see it); inbound letter arrives, renders, marks read locally only |
| C-2 | bench | Window resync and deliberate re-delivery leave no duplicate letters; seen-ring holds ≥1000 ids; eviction doesn't resurrect (§4.1) |
| C-3 | host | Every ack status is terminal: `sent` removes; each `rejected_*`/`invalid` preserves text, surfaces the guardian string, never resends (D-5) |
| C-4 | bench | Power cut between request send and response apply → letter resent, server ack-ring dedups, exactly one email; cut mid-§5.2 at each step → clean recovery, cursor never ahead of stored letters |
| C-5 | host | `local_id` strictly monotonic and never reused across reboot and simulated power loss |
| C-6 | host | `more` drain caps at 10 rounds; drain counts as one sync event |
| C-7 | bench | 401 → provisioning-fault state, no hot retry; 503 honours `Retry-After`; TLS trust failure → distinct "can't reach home" (D-6) |
| C-8 | bench | Pututu: valid token → one sync; replayed/forged/stale-counter tokens silently ignored; ≤1 SMS wake per 5 min; counter survives power loss; rollback heals via `pututu_counter_seen` against a restored dev Wasi (server V-20's counterpart) |
| C-9 | host | Grapheme parity: vectors emitted by `tools/graphvectors` (Go uniseg) — ZWJ emoji, combining marks, flags — count and line-break identically under utf8proc; no break ever splits a cluster (server §4.9) |
| C-10 | bench | Wipe ordering: command-recording panel mock sees exactly the §9 sequence, for all three triggers; low battery refuses to open content |
| C-11 | host | Put-away intercepted below UI dispatch: a deliberately hung screen handler cannot swallow it; watchdog path also wipes |
| C-12 | host | Cover render contains wordmark/battery/flag only — no count, no names, no content bytes (D-1) |
| C-13 | host | Tombstones and `c_sys` named on letters, absent from compose picker; system sender renders as "Wasi" |
| C-14 | host | Letter with unknown `contact_id` is kept and rendered under the fallback label, never dropped |
| C-15 | gate | `strings.c` contains no `pututu`/`ayllu`/`kipu` and no address-shaped text; no user-facing literals outside it (D-2, §0) |
| C-16 | gate | Neither variant's ELF links WiFi/BLE symbols (`esp_wifi_*`, `esp_bt_*`, …) (D-3) |
| C-17 | host | Draft survives timeout, put-away, and simulated power loss mid-compose (§11.3) |
| C-18 | bench | PIN: config push enables; wrong entries back off; sync + put-away work locked; config clearing unlocks at next background sync (§11.5) |
| C-19 | gate | Full bench run's captured serial output (dev build, verbose) contains no substring of any letter (D-7 — the client V-11) |
| C-20 | gate | PPK2 measurements: deep-sleep floor, timer-wake cost, and a scripted 15-min compose session, each within 2× the design §7 budget line; regressions fail the release |
| C-21 | host | Dates render blank until first sync; `server_time` disciplines the clock (§5.6) |
| C-22 | bench | Config push: `max_letter_chars` change moves the compose cap next letter; `rat` reaches the modem; unknown config fields ignored |
| C-23 | gate | Release-build config asserts flash encryption, NVS encryption, and secure boot v2 enabled; dev/release fuse states documented and checked by the provisioning tool (D-4) |
| C-24 | bench | Emergency wipe (§9.1): injected ALRT with a deliberately hung UI task → the handler runs the full sequence in order (modem off first, BUSY respected, flush cycles, cover-or-white, rail cut) against the command-recording panel mock; post-wipe the device wakes on VBUS only, never keys; alert is not armed in sleep. Power half: PPK2 measures battery sag through a flush at the threshold voltage — cold-battery case on the bring-up checklist (§16) |

## 16. Milestones

| | Contents |
|---|---|
| **CM0** | Scaffold: IDF project + component seams (§3.1), strings table, host-test harness, `chaskibridge`, `graphvectors`, board profile for bare Walter |
| **CM1** | The letter path on the bench, no display needed: sync engine, stores, dedup, outbox, acks, backoff — dev build over the bridge against the compose stack; C-1…C-6, C-9, C-14, C-17 pass |
| **CM2** | Face and hands: SSD1680 driver, layout engine, wipe sequence incl. the §9.1 emergency path, input seam (KeebDeck Basic), all §11 flows, PIN, cover; C-10…C-13, C-15, C-18, C-24's sequence half (its power half lands with C-20 in CM3) |
| **CM3** | The road: modem transport with pinned CAs, PSM/TAU, timer wake + SMS poll, pututu verify, RAT push, kipu; first live sync over LTE-M; C-7, C-8, C-22, first C-20 numbers |
| **CM4** | Hardening: fuse ceremony, provisioning tool + factory NVS, ESP Web Tools page on Wasi, full C-table green, C-19/C-20/C-23 gating a release build |
| *v2* | "Where I am" (§17), tier-2 kipu toggle UI (paired with server v2), emoji glyph coverage, 4.2" board profile if the panel decision goes that way |

Verify-with-vendor before CM3 (carried from design §11, still open): GM02SP
private-CA TLS profile mechanics and session reuse across PSM; PSM/eDRX timer
survival on Hologram's carriers; whether a modem RING line is routed
(optimisation only); neighbouring-cell reporting (a v2 dependency).

Bring-up measurements for §9.1, before the threshold is frozen: confirm
MAX17048 VALRT/ALRT behaviour and the active-vs-hibernate sampling latency in
practice; measure battery-voltage sag through an emergency flush at the
candidate threshold with a **cold** battery — that measurement sets both the
final threshold and the flush pass count.

## 17. Specified for v2 (built later, shaped now)

- **"Where I am"** (design §4.2, unchanged): compose-time GNSS fix + serving
  cell capture, degradation ladder rendered honestly (pin only for GNSS rank),
  capture-time stamping, resolution entirely on Wasi's `cell` service (server
  §11.2). Firmware v1 keeps GNSS dark (D-3); the compose UI reserves no space
  for it — it arrives as a new compose action, not a retrofit.
- **Tier-2 toggle**: the child-side switch for coarse position reporting, with
  the symmetric notice letters generated server-side (design §3.7). Needs one
  v2 wire addition (the toggle state riding the sync request), to be specified
  in the server plan when built.

## Appendix B — client decision log

**B.1 · Pure ESP-IDF, custom SSD1680 driver — no Arduino layer.** The design
spec reached for GxEPD2 (design §4.1, §5.3) because it encodes the clear
waveform correctly; it is an Arduino library, and pulling arduino-esp32 in as
a component drags a second release train and a core that initialises
subsystems this firmware wants provably absent (D-3). SSD1680 is a small,
well-documented controller; the driver is bounded work, and GxEPD2 remains the
reference to check waveform behaviour against. Chosen with the user.

**B.2 · Dev transport is USB, never a radio.** The alternative — WiFi compiled
into dev builds only — makes D-3 depend on a build flag being right forever.
A USB-CDC bridge plus a dumb Go proxy gives the same iteration speed against
the real compose stack, exercises real bearer auth, and keeps every build's
ELF radio-free so C-16 can assert D-3 unconditionally. Chosen with the user.

**B.3 · Contact cosmetics are a device-local overlay — resolves a spec gap.**
Design §3.2 grants the youth nicknames, ordering, pinning, portraits; server
§3 describes `ayllu.toml` as holding that overlay and ships the fields in the
ayllu payload; but the sync request has no device→server mutation, so as
specified the youth could never actually edit anything. Resolution: the
overlay lives on the device (§4.4), server values are guardian-set defaults,
and nothing is sent upstream — consistent with the server's own
anti-requirement of holding no engagement state. Cost, accepted: factory reset
loses cosmetics (letters return; decorations don't). The alternative — a
cosmetics block in the sync request round-tripping into `ayllu.toml` — is
recorded here as the v2 path if reality shows resets are common or cosmetics
matter more than expected. Server-spec §3's wording ("the youth's cosmetic
overlay" in `ayllu.toml`) should be read as "guardian-seeded defaults" until
then. Chosen with the user.

**B.4 · PIN is optional and guardian-pushed, with remote-only recovery.**
Design §11 left the wake PIN open. Decision: off by default; a guardian
enables it through the pushed config block; recovery is the guardian clearing
it, which lands at the next background sync — no local reset, no
data-destroying attempt limit, no forgettable-secret brick for a kid three
states away. Backoff prices guessing; flash encryption already protects the
stored letters from anyone who opens the case. Chosen with the user.

**B.5 · The cover shows a mail-flag glyph, never a count.** Design §11's open
question, decided: the raised flag is the physical-mailbox story the whole UX
leans on and reveals only that something arrived — not how much, not from
whom. A count is the design spec's own warning ("even '3 waiting' is a
conversation someone else can start"); nothing is also defensible but gives
the kid no reason to open it between syncs. Chosen with the user.

**B.6 · Firmware lives in this monorepo.** The wire contract, the specs, the
compose stack, and the V-table conventions are all here; a separate repo would
cross-reference them by copy and drift. Go and C coexist behind separate build
entry points, and `make check` grows the firmware gates. Chosen with the user.

**B.7 · utf8proc for graphemes, with cross-implementation vectors.** The
server counts with `rivo/uniseg`; the device must agree at the boundaries or a
letter the compose counter allowed gets a terminal `invalid` ack — a "couldn't
send" the child did nothing to deserve. Rather than trusting two UAX #29
implementations to agree, `tools/graphvectors` emits vectors from the server's
own library and C-9 runs them against utf8proc; a Unicode-version skew fails a
test instead of a child.

**B.8 · The device stores a bounded recent window, not the archive.** Design
Principle 5 makes the mailbox canonical and the device a derived view; storing
"everything ever" on-device would slowly invert that. `letters_keep` defaults
to the server's `resync_window` (200) so a factory reset and steady state hold
the same shape. 16 MB of flash could hold far more; declining to is the point.

**B.9 · Outbox cap is 12, counting letters waiting for the runner.** Server
§4.1 sizes its request cap around "a full 12-letter outbox"; the device adopts
the same number as its own cap. At the cap, compose still works — finishing a
letter parks it as the draft with *"the bag is full — sync to send these
first"* — because refusing to let a kid write is never the right failure.

**Terminally rejected letters do not count against the cap.** The cap models
the runner's bag; a rejected letter is not waiting for the runner, it is
waiting for the child (§5.4). Counting them would turn a stuck state into a
lockout — only the child clears a reject, so twelve undismissed ones would
block composing indefinitely — and would make the on-screen explanation false,
since no amount of syncing moves a rejected letter. Rejected entries are
retained with their text intact until the child dismisses or recomposes them;
at ~2 KB each on a 12 MB partition, their accumulation is not a storage
concern, and the outbox screen makes them visible rather than silent. (Raised
during the Wave 1 build, where the first implementation counted all entries.)

**B.10 · v1 renders unknown glyphs as a substitution mark, honestly.** Full
emoji glyph coverage on a 4-grey panel is real work with real flash cost and
is deferred to v2. What v1 must get right — and does — is *counting and never
splitting* those graphemes (C-9); a visible ▯ is acceptable, a corrupted
cluster or a crash is not.

**B.11 · Dev builds keep D-7.** It would be conventional to let debug builds
log freely; here the bench log grep (C-19) runs against the *dev* build's
verbose output, because the dev build is the one attached to terminals that
scroll past shoulders. Content-free logging is a habit, not a release flag.

**B.12 · The cursor is durable in flash, not RTC-only — extends design §6.4.**
The design spec put the sync cursor in RTC slow memory, which dies with power.
That is *safe* (empty cursor = window resync, dedup absorbs it) but needlessly
expensive: every flat battery would cost a 200-letter re-download on a per-MB
bill. One small flash write per applied sync (~10/day) moves the cursor to
LittleFS; RTC memory keeps only reconstructible wake bookkeeping.

**B.13 · A hardware low-voltage interrupt backstops the wipe.** D-1's
per-sleep wipe left one window open: awake with content on screen when the
battery dies suddenly — fuel-gauge drift, cold sag, or hung firmware carrying
the device past the graceful 5 % trigger. The interrupt is free: the MAX17048
already in the BOM exposes an undervoltage alert on its ALRT pin, so the
backstop adds no part and no quiescent draw, where a discrete supervisor would
have added both. Scoping it to the awake state (sleep is already wiped) keeps
it out of the power budget entirely. The emergency flush is black/white
cycles rather than the full cosmetic clear because completion outranks polish
— freezing the panel mid-waveform is worse than not wiping — and it may end
on white, an exception to the no-blank-cover rule that applies to this path
only. Post-wipe wake is charger-presence only, so key-presses cannot drain
the pack past the brownout floor. False triggers (TX sag, cold) are accepted
as cheap: nothing is destroyed, so the design biases toward wiping on doubt.
What no threshold fixes — instantaneous power loss — is stated in §9.1 rather
than papered over. Investigated and adopted with the user.
