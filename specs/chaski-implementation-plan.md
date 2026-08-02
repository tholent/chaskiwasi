# Chaski Firmware — Implementation Plan

Derived from `specs/chaski-client.spec.md` (authoritative for firmware) and
`specs/wasi-server-plan.md` (authoritative for the wire). The design spec is
context. This plan is the contract subagents code against; section references
(§5.2, D-1, C-9, B.13…) point at the client spec unless prefixed
**server §** or **design §**.

Companion: `specs/implementation-plan.md` is the server-side equivalent and
sets the conventions this document reuses (citation comments, V/C-table test
naming, wave structure, findings log).

---

## 0. Ground rules for every task

1. **The spec is the requirements doc.** Every exported symbol implementing a
   numbered clause carries a citation comment (`// §5.2: cursor is written
   last.`, `// D-5: acks are terminal.`), same style as the Go tree.
2. **Invariants D-1…D-7 are testable, not aspirational.** Code that could put
   letter content in a log, an address anywhere, a radio symbol in the ELF, or
   a UI string outside `chaski_strings.c` is a defect, not a style issue.
3. **Seams stay pure.** Components under `components/` that implement logic
   (syncengine, layout, store, pututu, wipe controller) include **no `esp_*`
   or `freertos/*` headers** — hardware reaches them only through the §3 seam
   interfaces. This is what makes the host test tier exist at all.
4. **Content-free logging from day one, in every build** (D-7). The C-19 log
   grep runs on every bench session, not once at CM4. Log letter ids and local
   ids; never bodies, subjects, or names.
5. **Vocabulary boundary:** `pututu`/`ayllu`/`kipu` may appear in identifiers,
   comments, and wire field names — never in `chaski_strings.c` (§0, C-15).
6. **Numbered tests carry their number:** `TestC<n>_<Name>` (host tests in
   GoogleTest use `TEST(C<n>, Name)`), so the §15 table maps to the suite
   mechanically.
7. **File ownership is disjoint within a wave.** The seam headers exist before
   wave 1; nobody edits another agent's component. Bench time is the one
   shared resource — see §4's bench discipline note.
8. **No irreversible fuses before CM4.** Dev boards run development-mode flash
   encryption and no secure boot until the ceremony; the provisioning tool
   refuses release fuses outside it.

## 1. Toolchain and dependencies

| | |
|---|---|
| Framework | **ESP-IDF, newest LTS at CM0** — exact version pinned in `firmware/chaski/README` at scaffold time and treated as frozen; upgrades are deliberate, bench-tested acts (§3) |
| Target | `esp32s3` (Walter: ESP32-S3-WROOM-1-N16R2) |
| Language | C++ (no exceptions, no RTTI) for seams/state machines; C elsewhere; IDF idiom throughout |
| Modem | `dptechnics/walter-modem` ESP-IDF component (GM02SP: PDP, PSM, TLS profiles, HTTP, SMS). Wrapped, never called directly outside `components/modem/` |
| Graphemes | **utf8proc**, vendored; its Unicode version and `rivo/uniseg`'s recorded side by side in the README (C-9 is the referee, B.7) |
| Letter store | `joltwallet/littlefs` component on a dedicated partition |
| JSON | cJSON (ships with IDF) — 2 KB payloads, no schema machinery |
| Host tests | Plain CMake + **GoogleTest**, host-only; possible because of ground rule 3 — logic components build on Linux with no IDF installed |
| Bench harness | Go (`test/firmware/bench`, build tag `bench`), reusing `deploy/compose.dev.yml` + maddy and embedding `chaskibridge` as a library |
| Host tools | Go 1.26.5 (existing toolchain): `chaskibridge`, `graphvectors`, `fwgates`, provisioning tool |
| Flashing | `idf.py flash` for development; ESP Web Tools page (CM4) for guardians |

**Makefile integration:** `make check` (the repo gate) grows two cheap,
IDF-free additions — the firmware **host tests** (CMake+GoogleTest) and the
**strings/vocabulary gates**. Everything needing the cross-toolchain or a
built ELF (firmware build, symbol scan, bench suite) lives behind
`make fw-check`, so server-side work never acquires an IDF dependency.

## 2. Repository layout

Extends the spec §3.1 sketch with the test tree:

```
firmware/chaski/
  README                     pinned IDF version, utf8proc/uniseg Unicode versions
  partitions.csv             factory app + littlefs + nvs + factory-nvs (no OTA slots — design §6.5)
  sdkconfig.defaults         prod baseline; sdkconfig.dev adds USB bridge + verbose logs
  main/                      app_main, wake dispatch, strings.c (ALL user-visible text)
  components/
    transport/               seam + usbbridge impl (dev)      §14
    modem/                   walter-modem wrapper + modem transport impl (prod)  §5.3, §6
    syncengine/              request assembly, §5.2 apply order, backoff, acks
    store/                   letterstore, outbox, seenring, statestore (atomic writes)  §4
    ayllu/                   snapshot + device-local cosmetic overlay  §4.4
    pututu/                  token verify, counter (NVS), rate limit  §7
    panel/                   seam + ssd1680 impl + recording fake  §8.1, §9
    layout/                  utf8proc line-break, pagination, fonts  §8.2
    input/                   seam + keebdeck-i2c impl; put-away/sync interception  §10
    ui/                      screens, flows, PIN, cover, drafts  §11
    kipu/                    tier-1 block + readable log ring  §13
    power/                   fuel-gauge seam (MAX17048 impl + fake), ALRT wiring, wipe controller  §9, §9.1
  docs/bringup.md            pins, board profiles, panel timings, measurement log
tools/chaskibridge/          Go: USB-CDC ⇄ Wasi proxy (cmd + importable lib)  §14
tools/graphvectors/          Go: grapheme vectors from rivo/uniseg  (C-9, B.7)
tools/fwgates/               Go: ELF radio-symbol scan, chaski_strings.c scan, UI-literal lint  (C-15, C-16)
tools/provision/             Go: factory NVS image generation, fuse-state checks  §12 (CM4)
test/firmware/
  host/                      CMake + GoogleTest tree, one dir per component
  host/testdata/             grapheme vectors, wire fixtures (generated, committed)
  bench/                     Go bench suite (build tag `bench`) + run-log convention
```

**Wire fixtures:** a `go:generate` step in `internal/protocol` emits canonical
request/response JSON (every field, every ack status, worst-case emoji bodies)
into `test/firmware/host/testdata/wire/`. The firmware's mirrored structs
parse and re-emit them in host tests — the struct mirror can drift only by
failing a test, not silently. Regenerating fixtures is part of any wire
change, which keeps the server the single writer of the wire's shape.

## 3. Key seams (defined in the scaffold, before any parallel work)

Sketches, not final signatures — the scaffold freezes the real headers.

```cpp
// components/transport — the entire network surface (§14)
struct SyncResponse { int http_status; bool transport_ok; bool tls_trust_failed;
                      std::string_view body; };
struct Transport { virtual SyncResponse Sync(std::string_view request_json) = 0; };
// impls: ModemTransport (prod, pinned CAs), UsbBridgeTransport (dev)

// components/panel — everything the product needs, nothing more (§8.1)
struct Panel {
  virtual void PartialRefresh(Rect, const Framebuf&) = 0;
  virtual void FastRefresh(const Framebuf&) = 0;
  virtual void FullRefresh(const Framebuf&) = 0;
  virtual void ClearFlush(int passes) = 0;   // §9 step 1 / §9.1 step 4
  virtual void WaitBusy() = 0;               // §9.1: never abort a waveform
  virtual void DeepSleep() = 0;
};
// impls: Ssd1680 (real), RecordingPanel (host tests: C-10, C-12, C-24)

// components/input — scancodes in, interception below UI dispatch (§10)
struct InputSource { virtual bool Poll(KeyEvent&) = 0; };  // keebdeck-i2c | matrix (later)
// the input layer, not the UI, owns put-away and sync scancodes (C-11)

// components/store (§4) — all writes atomic (write-then-rename on LittleFS)
struct LetterStore { void Put(const Letter&);        // idempotent by id
                     void EvictBeyond(size_t keep); /* list, get, mark-read */ };
struct Outbox     { LocalId Add(Draft&&);            // LocalId monotonic, never reused (C-5)
                    void Resolve(LocalId, AckStatus); /* list */ };
struct SeenRing   { bool Contains(LetterId); void Add(LetterId); };  // ≥1000 (server §4.5)
struct StateStore { /* cursor, ayllu_version, local_id high-water — §5.2 step 6 owns cursor */ };

// components/syncengine — §5.2 is the whole contract
struct SyncEngine { SyncOutcome RunOnce(Trigger);      // assembles, calls Transport,
                                                        // applies in §5.2 order
                    /* backoff schedule per §5.3 */ };

// components/power — battery truth + the wipe controller (§9, §9.1)
struct Gauge { virtual int SocPct() = 0; virtual int MilliVolts() = 0;
               virtual void ArmUndervoltAlert(int mv, Callback) = 0; };  // MAX17048 | fake
struct WipeController { void GracefulWipe(CoverKind);   // §9 seven steps
                        void EmergencyWipe(); };        // §9.1 eight steps (C-24)

// components/modem — the only walter-modem caller
struct Modem { /* attach/PSM mgmt, HTTPS via TLS profile, SMS drain,
                  RAT set (§5.5), TrustStoreSync (§12) */ };
```

`main/` owns the wake dispatch and composition root: it reads the wake reason,
wires seams to implementations per build variant, and calls exactly one of
{UI session, background sync, SMS poll} (§6).

## 4. Execution waves

Up to 3 `coder` subagents per wave, disjoint file ownership, seam headers
frozen in wave 0. Each wave ends with its gate before the next begins.

**Bench discipline:** there is one Walter on the bench. Host tests and builds
parallelise freely; anything needing the device serialises through a single
integration slot at the end of the wave. Bench runs append a line to
`test/firmware/bench/RUNLOG.md` (date, firmware sha, suite result) — the bench
analog of CI history, since CI has no hardware.

**Standing caveat — no hardware yet.** As of the CM0/CM1 build there is *no
Walter board in the development environment at all*. Everything delivered so
far is host-tested or build-verified; nothing has executed on the target. The
bench tier is therefore written-and-compilable rather than passing, and skips
with a clear message when no device is attached. Two consequences to keep
honest: (1) a wave "gate" currently means host tests + gates + a linking target
image, not a device run; (2) the hardware-blocked C-rows — C-1, the bench
halves of C-2/C-4/C-7, C-8's live path, C-10 and C-24 on a real panel, C-19's
serial grep, C-20's PPK2 numbers, C-23's fuse checks — stay open no matter how
green the suite looks. Do not let a green `make check` be reported as a working
device.

### Wave 0 — scaffold (serial, no subagents) → CM0

IDF project skeleton building for `esp32s3` and pinned; `partitions.csv`;
seam headers with doc comments (§3); `chaski_strings.c` skeleton; wire structs
mirrored from `internal/protocol` + the fixture `go:generate`;
`tools/graphvectors` and `tools/fwgates` (both runnable, gates green on the
skeleton); `tools/chaskibridge` skeleton; host-test CMake tree with one
passing test per component; `make check` / `make fw-check` split (§1);
`docs/bringup.md` opened with the board profile for the bare Walter.

### Wave 1 — the logic core, host-only → CM1 part 1

| Agent | Owns | Delivers |
|---|---|---|
| 1A | `store/`, `ayllu/` | Atomic write-then-rename on a filesystem seam (host: tmpdir; target: LittleFS); letter store idempotent by id + eviction (B.8); outbox with monotonic never-reused `LocalId` (C-5); seen-ring ≥1000 (C-2 logic); state store; ayllu snapshot replace + cosmetic overlay merge (§4.4, C-13/C-14 data side) |
| 1B | `syncengine/` + wire structs | Request assembly (§5.1); **§5.2 apply order with a fault-injection hook at every step** (C-4 logic); terminal acks + couldn't-send preservation (C-3); backoff table (§5.3); `more` cap (C-6); clock validity (C-21); wire fixtures round-trip |
| 1C | `layout/`, `chaski_strings.c`, gates | utf8proc integration; grapheme line-breaker + paginator over vectors from `graphvectors` — ZWJ emoji, combining marks, flags (C-9); font plumbing for two sizes; strings table filled for §11's flows; `fwgates` lint wired into `make check` (C-15) |

**Gate:** all wave-1 host tests green with no IDF installed; C-9 vectors
committed; `make check` runs them.

### Wave 2 — the bench letter path → CM1 complete

| Agent | Owns | Delivers |
|---|---|---|
| 2A | `transport/` (usbbridge), `tools/chaskibridge`, bench harness | Framed USB-CDC protocol both ends; bridge passes bearer header untouched (§14); Go bench suite booting compose stack + driving the device; C-1 end to end; C-2/C-4 bench halves (power-cut via scripted reset at each §5.2 step); C-7's 401/503 cases |
| 2B | `pututu/`, `kipu/` | Token parse/MAC verify/monotonic counter in NVS/rate limit, all behind a clock+NVS seam so C-8's host half runs without a modem (§7); tier-1 kipu block ≤512 B + readable log ring (§13) |
| 2C | `main/` wake dispatch, `power/` (fake gauge), sleep skeleton | Composition root per build variant; wake-reason dispatch (§6); deep-sleep enter/exit with RTC bookkeeping on the bare Walter; timer wake loop with logged (content-free) transitions; fake gauge driving the graceful-wipe *state* logic (no panel yet) |

**Gate:** dev-build firmware on the bare Walter completes C-1 against the
compose stack through the bridge; first `RUNLOG.md` entry; C-19 grep clean on
that session's captured serial output.

**Wave 2 status — PARTIAL.** The three agents were terminated mid-task by a
session limit; the surviving work was stabilised and committed. Landed and
host-tested: the USB-CDC frame codec on both sides (with generated
cross-language vectors), `usbbridge`/`usbcdc_link`, pututu verification against
the server's own tokens, the tier-1 kipu block and its readable log, draft and
settings storage (closing F-C7), and wake-reason dispatch with RTC bookkeeping.

Outstanding before this wave's gate can be attempted:
- **`test/firmware/bench/` does not exist.** C-1's end-to-end half and the
  bench halves of C-2, C-4 and C-7 are unwritten, as is the `RUNLOG.md`
  convention. This is the largest remaining piece.
- **`main/wake.cpp` is not linked into `app_main` or the target image** — it is
  host-tested logic that nothing calls yet. Wiring it needs the `Jobs`
  implementations, whose doorbell poll depends on the modem (Wave 4).
- `tools/chaskibridge`'s serial loop is written but has never driven a device.

### Wave 3 — face and hands → CM2

| Agent | Owns | Delivers |
|---|---|---|
| 3A | `panel/` | Ssd1680 driver (partial/fast/full/clear-flush/busy/deep-sleep) against the reference panel; RecordingPanel; WipeController: §9 seven-step order and §9.1 emergency path incl. modem-off-first and VBUS-only wake; C-10, C-24 sequence half; ghosting measurement procedure written into `bringup.md` (design §11 — privacy test) |
| 3B | `input/` | KeebDeck Basic I2C source; interception layer below UI dispatch for put-away + sync; watchdog expiry path that wipes (§10); C-11 |
| 3C | `ui/` | Inbox/read/compose/outbox/settings/fault-state screens (§11); draft autosave + resume (C-17); PIN screen, backoff, config-push enable/clear (C-18); cover renderer incl. charge-me and mail-flag (C-12); tombstone/`c_sys` rules (C-13) |

**Gate:** the in-hand demo — pick Rosa, type, put-away mid-compose, wake,
resume draft, sync, read the reply — on real hardware; CM2's C-list green
(C-10…C-13, C-15, C-17, C-18, C-24 sequence half); C-19 grep on the session.

### Wave 4 — the road → CM3

Hardware-bound: agents own disjoint components, but every integration step
serialises through the bench slot. The pre-CM3 verify-with-vendor list
(§16 — TLS profile mechanics, PSM timer survival, RING line, VALRT behaviour)
is a **blocking input** to this wave, not a parallel task.

| Agent | Owns | Delivers |
|---|---|---|
| 4A | `modem/`, `transport/` (ModemTransport) | walter-modem wrapper; TLS profile with the two pinned CAs + TrustStoreSync at boot (§12, D-6); HTTPS sync over LTE-M; RAT push (§5.5); distinct can't-reach-home on trust failure (C-7 TLS case) |
| 4B | PSM + pututu integration | PSM/TAU configuration and hold-across-sleep (§6); timer-wake SMS drain → pututu verify → sync; counter-heal against a restored dev Wasi (C-8 bench, server V-20's mirror) |
| 4C | `power/` (real gauge), power discipline | MAX17048 driver + ALRT ISR → EmergencyWipe (C-24 live); refresh batching + light-sleep-between-keystrokes in compose (§8.3); PPK2 measurement scripts and first C-20 numbers into `bringup.md`, incl. the §9.1 cold-battery sag run |

**Gate:** live end-to-end on Hologram: letter out via LTE-M, pututu SMS rings
the device, reply arrives within the coalescing window; measured sleep floor
and wake costs recorded; C-7, C-8, C-22 green.

### Wave 5 — hardening → CM4

| Agent | Owns | Delivers |
|---|---|---|
| 5A | `tools/provision/`, fuse ceremony | Factory NVS image (token, pututu key, URL); dev/release fuse-state checks (refuses release fuses outside the ceremony, ground rule 8); ceremony doc joining the CA ceremony in `docs/`; C-23 |
| 5B | Release pipeline + full sweep | Signed release build; secure boot v2 + flash encryption on a sacrificial board first; automated C-19 grep over a full bench run; C-16 on both variant ELFs; the complete C-table wired into `make fw-check` (bench rows as the scripted checklist) |
| 5C | ESP Web Tools flow | The guardian flashing page — **served by Wasi**, so this task touches `internal/web/` and follows the server repo's rules (V-14 vocabulary boundary included); tested end to end with a non-technical user per design §11 |

**Gate:** full C-table pass recorded in `RUNLOG.md` against a release-signed
build; a factory-fresh device provisioned, flashed via the Web Tools page, and
syncing over LTE with no developer tooling touched.

## 4a. Findings against the spec

Discrepancies found while implementing, recorded here as F-C1, F-C2… in the
manner of the server plan's §4a — described, resolved-or-deferred, and folded
back into `chaski-client.spec.md` as Appendix B entries once decided. Both
prior specs show this section earns its keep (the server build logged nine).

**F-C1 · ESP-IDF has no "LTS" designation. RESOLVED — pinned v5.5.5.**
§1 called for "the newest LTS at CM0". No such label exists: Espressif supports
each minor release for 30 months, with no release singled out for longer.
The pin was therefore chosen on different grounds and recorded in
`firmware/chaski/README.md`: **v5.5.5**, newest patch of the newest 5.x line.
v6.0 is newer but ahead of the vendor ecosystem — `dptechnics/walter-modem`
v1.5.0 targets 5.x, and the modem driver is not a component to be adventurous
with. Read §1's "newest LTS" as "newest release with long remaining support
that the vendor driver targets".

**F-C13 · There was no erase key, and `kBack` was doing both jobs. RESOLVED —
`Key::kErase` added.**
The `Key` enum had `kBack` and no backspace, so the compose screen used one key
for both: it erased while the field had text and navigated once the field
emptied. That makes "go back" silently destructive in one state and
navigational in the next, with the child unable to predict which they will get
and the difference being a sentence they wrote. `kErase` is now distinct —
`kBack` navigates, `kErase` deletes — and only `kErase` repeats, because
correcting a mistake thirty characters back is otherwise thirty presses, which
is where a kid abandons the letter. Raised by agent 3B, which declined to add
the enumerator itself because 3C was compiling against the header concurrently;
appended rather than inserted so no existing scancode table was renumbered.

**F-C12 · Log lines and `static_assert` messages are not UI text.** The C-15
stray-literal scan flagged diagnostics in `main/`, which would have forced
developer text through the strings table and put it in the same file as the
words on the glass. The gate now skips developer-facing call sites
(`ESP_LOG*`, `ESP_ERROR_*`, `static_assert`, `assert`), including their wrapped
continuation lines. Two subtleties worth keeping: the semicolon test has to
ignore semicolons *inside* string literals, because diagnostic messages are
prose and prose contains semicolons — the naive version resumed scanning
mid-statement and flagged the assert's own continuation. And D-7 still applies
to what those calls interpolate; that is a question about arguments, which this
scan cannot answer and C-19's output grep can.

**F-C11 · The doorbell rate limiter could be used to silence the doorbell.
RESOLVED — a closed window is no longer re-charged.**
The first implementation charged the limiter on every *received* token,
including one it had just refused. That slides the window forward on each
arrival, so a token every four minutes suppresses the doorbell indefinitely.
The limiter exists to stop a flood becoming a battery- and balance-drain
(server §10.2); charging it this way traded that for a
delay-the-child's-letters attack, which is the worse of the two — letters would
wait for the six-hourly scheduled sync (§13 `sync.interval_s`) with nothing
visibly wrong on either end, and the child would simply find their family
quieter than it is.

The binding reading of "regardless of validity": a forged or replayed token
still **spends the window it was allowed to spend**, so garbage buys no extra
wakes. It does not mean re-charging a window that is already closed. Found by a
failing test in the Wave 2 build, not by review, and mutation-tested — putting
the old behaviour back fails two tests.

**F-C10 · An absent `config` field silently reset the device. RESOLVED —
`DeviceConfig` fields are optional.**
`wire::DeviceConfig` held plain values initialised to the server's documented
defaults, so a decoder could not tell "the server sent 500" from "the server
sent nothing". §5.5 applies the config block field-by-field and ignores what it
does not recognise; an absent field is that same case and must leave the
device's current value alone. As written, a server that stopped sending
`max_letter_chars` would silently reset every device to 500 — a configuration
change nobody made, that no log records, and that the child would experience as
their letters getting shorter. Fields are now `std::optional`, absent stays
absent, and the documented defaults live in named constants for a device that
has never been told otherwise. Raised by agent 1B as a wire-contract ambiguity.

**F-C8 · utf8proc was never actually vendored. RESOLVED — vendored 2.8.0 for
both platforms, with one upstream patch.**
§1 said "utf8proc, vendored", and the scaffold shipped nothing: the host had
Debian's `libutf8proc-dev`, so Wave 1 compiled cleanly and nobody noticed until
the target build, where ESP-IDF ships no utf8proc and the component registry has
no port. Vendored at `components/utf8proc/` (2.8.0, Unicode 15.0.0, MIT), and
the **host tier now compiles the same vendored source instead of the system
package** — otherwise C-9 would validate Debian's build while the device ran a
different one, which is precisely the two-implementations skew B.7 exists to
prevent. Version stays in lockstep with the server's `rivo/uniseg` v0.4.7.

One local patch was required and is recorded in `components/utf8proc/PATCHES.md`:
`last_boundclass` is declared `int*` but used as `utf8proc_int32_t*`. Those are
the same type on x86-64 (`int32_t` is `int`) and different on xtensa (`long
int`), so it compiles on the host and fails under IDF's `-Werror=all`. The fix
is upstream's own, from 2.9.0; it is applied locally rather than by upgrading,
because 2.9.0 moves to Unicode 15.1.0 and that bump must be paired with the
server's segmenter, not made to silence a compiler.

**F-C9 · D-3 as literally worded is unachievable on this silicon. RESOLVED —
C-16 distinguishes linked code from ROM addresses; D-3 reworded.**
The first C-16 run failed on our own firmware, reporting `ieee80211_*` and
`btdm_*` symbols. They are real, and they are not ours: every ESP32-S3 has a
WiFi/BT stack in mask ROM, and `esp32s3.rom.ld` names those entry points in
every build of the chip. They appear as `SHN_ABS`, size-0, `NOTYPE` symbols —
addresses, not code — while genuinely linked code (`esp_wifi_init`) was absent,
which is what MINIMAL_BUILD (F-C4) had already achieved.
A gate that fails on every possible build of the target is worthless, so C-16
now skips absolute symbols and fails only on radio symbols occupying a real
section. Verified both directions: our image passes, a synthesised binary
defining `esp_wifi_init` still fails. D-3 is reworded from "contains" to
"links", which is the honest and testable claim.

**F-C5 · A full bag of rejected letters would have locked composing out.
RESOLVED — the cap counts sendable entries only; B.9 amended.**
B.9 fixed the outbox at 12 but never said whether a terminally rejected letter
still occupies a slot. The first implementation counted all entries, which is
the reading that breaks: only the child clears a reject (§5.4), so twelve
undismissed ones would block composing indefinitely, while the UI told them to
"sync to send these first" — advice no sync can act on. The cap now counts
`SendableCount()`, and B.9 records why. Rejected entries keep their text and
their visibility on the outbox screen; at ~2 KB each they are not a storage
concern. Raised by agent 1A as a question rather than silently chosen, which is
how a spec gap should surface.

**F-C6 · `state` is three files, not one.** Client §4.1 lists the cursor, the
≥1000-id seen ring, and the local-id high-water under a single `state` file.
They are written on different schedules by objects with different lifetimes,
and folding a ~12 KB ring into the record rewritten on every cursor advance
would spend a 12 KB flash write per sync on a device that counts them (design
§6.4). Split into `state`, `seen`, and `local_id`, single-writer each. The spec
sentence describes the *contents* of device state correctly; only the file
count differs, so this is recorded rather than amended.

**F-C7 · `draft` and `settings` have no seam and therefore no Wave 1 owner.**
Both are named in the client §4.1 storage model, but the scaffold declared no
API for either, so C-17 (a draft survives timeout, put-away, and power loss)
cannot be satisfied by Wave 1's components as they stand. Owner: Wave 3 agent
3C, which owns drafts and settings in the UI — it adds the store-side API when
it gets there. Recorded so it is a scheduled gap rather than a discovered one.

**F-C3 · The cJSON header has a different path on host and target. RESOLVED —
one spelling, host include path widened.**
§1 says cJSON "ships with IDF", and the scaffold README claimed it was "the same
header both sides". It is not: Debian's `libcjson-dev` installs
`<cjson/cJSON.h>`, ESP-IDF's `json` component exposes `"cJSON.h"` at the top
level. The host tier could not have caught this — it only appears when the
target build runs, which is the argument for running that build during Wave 0
rather than at CM3. Resolution: component code uses the IDF spelling (`"cJSON.h"`,
the one that must work on the device), and `test/firmware/host/CMakeLists.txt`
adds the `cjson/` subdirectory to the include path so the same spelling resolves
on the host. No `#ifdef ESP_PLATFORM` in any source file. The `wire` component
also needed `REQUIRES json` in its `idf_component_register` call — an empty
REQUIRES compiles fine on the host and fails only on the target.

**F-C4 · `CONFIG_ESP_WIFI_ENABLED=n` does not disable WiFi. RESOLVED —
`MINIMAL_BUILD`.**
The Wave 0 `sdkconfig.defaults` set `CONFIG_ESP_WIFI_ENABLED=n` and
`CONFIG_BT_ENABLED=n` to satisfy D-3. The generated `sdkconfig` came back with
`CONFIG_ESP_WIFI_ENABLED=y` regardless — the symbol is derived from
`SOC_WIFI_SUPPORTED`, not freely user-settable — and the build duly compiled
`esp_wifi` and `wifi_provisioning`. **D-3 says "not compiled in, not merely
disabled", so a config knob was never going to be the right mechanism.**
Resolution: `idf_build_set_property(MINIMAL_BUILD ON)` restricts the build to
`main` and its transitive `REQUIRES`; nothing in this firmware requires a radio
stack, so none is built. Verified: the component graph dropped from ~130 to 75
with no `esp_wifi`, `bt`, `wifi_provisioning`, `openthread`, `esp_coex`, or
`ieee802154` present. `CONFIG_BT_ENABLED=n` stays as defence in depth because
that symbol *is* settable. C-16's ELF scan remains the check that proves the
invariant — this finding is precisely why the gate is a symbol scan and not a
config assertion.

**F-C2 · A header named `strings.h` shadows POSIX `<strings.h>`. RESOLVED —
renamed `chaski_strings.h`.**
Found by building the Wave 0 scaffold, not by review. Both the spec (§0) and
this plan (§2) named the strings table `strings.c`/`strings.h`; with
`firmware/chaski/main/` on the include path, `#include <strings.h>` resolves to
ours, so any translation unit reaching for `strcasecmp` fails to compile —
GoogleTest does, which broke the entire host test tier at once. The `.c` and
`.h` are now `chaski_strings.*`; spec and plan references were updated in the
same change. Worth recording because the failure is remote from its cause: the
error surfaces inside gtest internals, naming neither our file nor our include
path.

## 5. Risks tracked during the build

- **walter-modem library fit is the big unknown.** The plan assumes it exposes
  TLS-profile management with custom CAs, HTTP POST through the modem stack,
  PSM control, and SMS drain. Any gap becomes AT commands in `modem/` —
  contained by ground rule 3's wrapper, but budget for it. The Wave-4
  verify-with-vendor items exist to surface this before code depends on it.
- **Unicode version skew** between utf8proc and `rivo/uniseg` shows up exactly
  at grapheme boundaries — C-9's generated vectors are the tripwire; both
  versions are pinned and recorded (B.7). Regenerate vectors on either bump.
- **Flash-encryption interplay** (LittleFS on an encrypted partition, NVS
  encryption keys) is exercised in **development mode from Wave 3**, not
  discovered at CM4. Release-mode fuses only ever burn in the ceremony, on a
  sacrificial board first.
- **KeebDeck Basic I2C protocol** may need its alternate firmware flashed
  before it speaks I2C; treat keyboard bring-up as part of Wave 3's 3B
  estimate, not a surprise.
- **Panel waveform timing** is copied from GxEPD2's SSD1680 support as
  reference (B.1) but verified against the actual -FL02 panel; the ghosting
  pass-count is a measured privacy parameter, not a constant to trust.
- **Bench tests are not CI.** They need a human and a device. The mitigations
  are structural: the host tier carries every assertion it can (ground rule
  3), and `RUNLOG.md` makes bench coverage visible and dated instead of
  assumed.
- **PSM/eDRX timers are requested, not guaranteed** (design §11) — if the
  carrier mangles them, the power budget's idle line moves; PPK2 numbers in
  Wave 4 are the early warning.
- **Single-device serialisation** makes Wave 4 the schedule risk; Waves 1–3
  front-load everything host-testable precisely so the bench-bound tail is
  short.
