# chaskiwasi — working notes

Wasi is the per-device server for the Chaski e-ink letter device. **One container
per device**: the container is the device's identity, so there is no `device_id`
anywhere and every state file is singular.

## Specs

| File | Standing |
|---|---|
| `specs/wasi-server-plan.md` | **Authoritative** for the server. Supersedes the design spec wherever they conflict; Appendix A records each supersession. |
| `specs/chaski-client.spec.md` | **Authoritative** for the device firmware. The wire contract stays in the server plan (§4, §10, §12); Appendix B records client decisions and design-spec supersessions. |
| `specs/chaskiwasi-design-spec.md` | Context: hardware, device UX, principles. Superseded on threading (A.1), pagination (A.10), storage (A.9). |
| `specs/implementation-plan.md` | Server build order, package ownership, dependency choices. |
| `specs/chaski-implementation-plan.md` | Firmware build order: waves mapped to CM0–CM4, seams, host/bench test split, risks. |

Cite clauses in code comments the way the existing files do (`// §4.7: ...`,
`// I-2: ...`). The section numbers are the requirements ids.

## Toolchain

Go 1.26.5 lives at `~/.local/go` (symlinked into `~/.local/bin`); apt's 1.19 is
too old for `log/slog`. `make check` is the gate: Go fmt/vet/build/test **plus**
the firmware host tests and text gates, which need no ESP-IDF.

Firmware (`firmware/chaski/`, ESP-IDF v5.5.5 at `~/esp/esp-idf`):

| Command | Needs |
|---|---|
| `make fw-hosttest` | CMake + g++ only — the whole letter path, no hardware |
| `make fw-gates` | Go only — C-15 vocabulary/address gates, Unicode-skew check |
| `make fw-build` / `make fw-check` | ESP-IDF (`. ~/esp/esp-idf/export.sh` first) |
| `make fw-vectors` | regenerates grapheme vectors + wire fixtures after a wire or segmenter change |

The host/target split is load-bearing, not convenience: logic components under
`firmware/chaski/components/` include **no `esp_*` or FreeRTOS headers**, so
firmware logic stays testable on any laptop and server work never acquires a
cross-toolchain dependency.

## The five invariants, in code terms

- **I-1 — no letter content is persisted anywhere.** Not in `/data`, not in
  backups, not in logs at any level. Log letter *ids* when correlation is needed.
- **I-2 — the device never sees an email address.** Addresses exist only in
  `ayllu.toml` and `ayllu-log.jsonl`. No wire field, notice letter, or error
  string may carry one.
- **I-3 — SMTP carries only child-authored letters**, plus optional operational
  copies to guardian addresses fixed in `wasi.toml`.
- **I-4 — nothing about the contact list changes silently.** Every mutation
  produces a notice letter and a change-log line.
- **I-5 — removal never deletes.** Deactivation, tombstone, address retained.

## Things that look like bugs and are not

- Resolution is deliberately split: **active-only** for filing and sending,
  **full table including tombstones** for read-time derivation (§7.2). "You can
  still read Rosa's old letters, you just can't write to her" falls out of that.
- Outbound carries `Message-ID` and nothing else — no `In-Reply-To`, no
  `References`, no `Re:` (A.1). V-8 fails loudly if anyone "fixes" this.
- A crash between SMTP send and ack write costs a **duplicate send**, on purpose
  (§4.7). Never reorder those two steps to avoid it.
- No page counts, no `chars_per_page`, no layout numbers anywhere on the server.
  Reflow is device-owned because font size is a runtime accessibility setting
  (§4.9, A.10).
- There is no database, and adding one is a spec reversal, not a refactor (A.9).

## Things that look like bugs and are not — firmware side

- The sync response is applied in a fixed order with the **cursor written last**
  (client §5.2). A crash before that costs a re-delivery the seen-ring absorbs.
  Reordering to "avoid" the re-delivery trades a duplicate for a lost letter.
- `EvictBeyond` deletes a letter file but keeps its seen-ring id, so an evicted
  letter is never re-downloaded. That asymmetry is deliberate.
- Contact cosmetics never leave the device — the wire has no device→server
  mutation and the server holds no engagement state (client B.3).
- The dev build has no WiFi. The USB bridge is the dev transport, so **no**
  build variant links a second radio (client B.2); `fwgates symbols` enforces it.
- `main/chaski_strings.h` is not named `strings.h` because that shadows POSIX
  `<strings.h>` and breaks anything reaching for `strcasecmp` (F-C2).

## Vocabulary boundary

`pututu`, `ayllu`, and `kipu` are internal identifiers — greppable on purpose.
They must never appear in `internal/web/templates/` or any outgoing-mail
rendering path. Test V-14 enforces this.

The same boundary holds on the device: every user-visible word lives in
`firmware/chaski/main/chaski_strings.c`, those three words may not appear in it,
and no UI literal may live anywhere else. `make fw-gates` (C-15) enforces both
halves and fails the build on a violation.
