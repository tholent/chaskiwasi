# chaskiwasi — working notes

Wasi is the per-device server for the Chaski e-ink letter device. **One container
per device**: the container is the device's identity, so there is no `device_id`
anywhere and every state file is singular.

## Specs

| File | Standing |
|---|---|
| `specs/wasi-server-plan.md` | **Authoritative** for the server. Supersedes the design spec wherever they conflict; Appendix A records each supersession. |
| `specs/chaskiwasi-design-spec.md` | Context: hardware, device UX, principles. Superseded on threading (A.1), pagination (A.10), storage (A.9). |
| `specs/implementation-plan.md` | Build order, package ownership, dependency choices. |

Cite clauses in code comments the way the existing files do (`// §4.7: ...`,
`// I-2: ...`). The section numbers are the requirements ids.

## Toolchain

Go 1.26.5 lives at `~/.local/go` (symlinked into `~/.local/bin`); apt's 1.19 is
too old for `log/slog`. `make check` (fmt, vet, build, test) is the gate.

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

## Vocabulary boundary

`pututu`, `ayllu`, and `kipu` are internal identifiers — greppable on purpose.
They must never appear in `internal/web/templates/` or any outgoing-mail
rendering path. Test V-14 enforces this.
