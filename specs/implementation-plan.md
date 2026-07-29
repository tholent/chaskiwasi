# Wasi — Implementation Plan

Derived from `specs/wasi-server-plan.md` (authoritative) and `specs/chaskiwasi-design-spec.md`
(context; superseded wherever the server plan speaks).

This plan is the contract subagents code against. Section references below (§4.7, I-2, V-9…)
point at `specs/wasi-server-plan.md`.

---

## 0. Ground rules for every task

1. **The spec is the requirements doc.** Every exported symbol that implements a numbered
   clause carries a comment naming it (`// §4.7: all ack statuses are terminal.`).
2. **Invariants I-1…I-5 are testable, not aspirational.** Any code that could persist letter
   content, expose an address device-side, or send SMTP that isn't a child letter is a defect.
3. **No database.** Files + atomic rename + fsync (§3), behind one writer mutex.
4. **Vocabulary boundary (§0.1):** `pututu`/`ayllu`/`kipu` may appear in Go identifiers and
   internal logs, never in `web/templates/` or any outgoing-mail rendering path.
5. Each package ships `go test` coverage for its own clauses; the numbered V-cases in §15 are
   named `TestV<n>_<Name>` so the table maps to the suite mechanically.

## 1. Toolchain and dependencies

| | |
|---|---|
| Module | `github.com/tholent/chaskiwasi` |
| Go | 1.26.5, installed to `~/.local/go` (apt ships 1.19; `log/slog` needs ≥1.21) |
| IMAP | `github.com/emersion/go-imap/v2` — IDLE, MOVE, APPEND, UIDVALIDITY |
| SMTP | `github.com/emersion/go-smtp` client for submission |
| MIME parse | `github.com/jhillyerd/enmime` (§5.2) |
| TOML | `github.com/pelletier/go-toml/v2` — needs marshal, the guardian UI rewrites `ayllu.toml` |
| Graphemes | `github.com/rivo/uniseg` (§0, named by the spec) |
| Passwords | `golang.org/x/crypto/argon2` (§9.2) |
| Web | stdlib `html/template` + htmx vendored into `embed.FS` — no build step, no CDN (§9.1) |
| Logging | stdlib `log/slog`, JSON to stdout (§14) |
| `strip` | Python 3-slim + `talon.quotations` (§11.1) |
| Test fixture | `maddy` as local IMAP/SMTP, via `deploy/compose.dev.yml` (§15) |

## 2. Repository layout

```
cmd/wasi/                  main, subcommands: serve, backup, useradd, contacts
internal/atomicfile/       temp→fsync→rename→fsync(dir), single writer mutex (§3)
internal/config/           wasi.toml load + hot reload (§13)
internal/secrets/          env / mounted-file secrets, never TOML (§3)
internal/state/            state.json: cursor mirror, ack ring, pututu counter, pending_notices
internal/kipu/             day-files + retention (§3, §4.8)
internal/graphemes/        UAX #29 counting and truncation
internal/protocol/         wire types for POST /sync (§4.2, §4.3) — shared with chaskisim
internal/ayllu/            contacts, tombstones, ayllu.toml, ayllu-log.jsonl (§7)
internal/letterid/         stable letter identity (§4.5)
internal/subject/          inbound normalisation (§5.4), outbound sanitise/generate (§6.2)
internal/mailbox/          IMAP client + SMTP sender
internal/strip/            HTTP client for the strip service + Go fallback (§5.3)
internal/derive/           read-time derivation (§5.2)
internal/filing/           IDLE, reconciliation, spam backstop (§5.1)
internal/notice/           announcement letters + pending_notices flush (§7.4, §7.6)
internal/syncsvc/          POST /sync handler (§4)
internal/carrier/          Carrier interface, hologram, fake, carriertest (§10.4)
internal/pututu/           coalescing, signed counter token, reconciliation (§10)
internal/web/              guardian UI: sessions, throttling, pages, templates
internal/guardians/        guardians.toml, argon2id, session_epoch (§9.2)
services/strip/            Python service + golden corpus
tools/chaskisim/           device simulator driving the e2e suite
deploy/                    compose.dev.yml, maddy config, Dockerfiles
test/e2e/                  the V-table suite
```

## 3. Key seams (defined in the scaffold, before any parallel work)

```go
// internal/ayllu
type Contact struct {
    ID, Name, Address string
    Active            bool
    Pinned            bool
    Order             int
    Portrait          string
}
// Resolve consults the FULL table incl. tombstones (derivation, §7.2).
// ResolveActive consults active rows only (filing and sending, §7.2).
type Store interface {
    List() (version int, contacts []Contact)
    Resolve(addr string) (Contact, bool)
    ResolveActive(addr string) (Contact, bool)
    ByID(id string) (Contact, bool)
    Mutate(actor string, m Mutation) (Event, error) // add | deactivate | reactivate | readdress
}

// internal/state — the only writer of state.json
type Store interface {
    Snapshot() State
    Update(func(*State) error) error // atomic + fsync before return
}

// internal/mailbox
type Mailbox interface {
    UIDValidity(ctx context.Context) (uint32, error)
    FetchAbove(ctx context.Context, uid uint32, max int) ([]Raw, error)
    Recent(ctx context.Context, n int) ([]Raw, error) // window resync (§4.4)
    Move(ctx context.Context, uid uint32, folder string) error
    Append(ctx context.Context, folder string, msg []byte) error
    Idle(ctx context.Context, notify chan<- struct{}) error
}
type Submitter interface{ Send(ctx context.Context, from, to string, msg []byte) error }

// internal/derive
type Deriver interface{ Derive(ctx context.Context, r mailbox.Raw) (protocol.Letter, error) }

// internal/carrier (§10.4 — verbatim from the spec)
type Carrier interface {
    Name() string
    Pututu(ctx context.Context, payload string) error
    Balance(ctx context.Context) (Balance, error) // ErrUnsupported is normal
}
```

## 4. Execution waves

Each wave runs up to 3 `coder` subagents in parallel. **File ownership is disjoint within a
wave** — the seam types above exist before wave 1 starts, so nobody edits another agent's
package. A wave ends with a build + `go test ./...` gate before the next begins.

### Wave 0 — scaffold (serial, no subagents)
`go.mod`, package skeletons carrying the §3 seam types and doc comments, `protocol` wire
structs, sample `wasi.toml`/`ayllu.toml`, Makefile, CI-less test targets.

### Wave 1 — M0 + M1 foundations
| Agent | Owns | Delivers |
|---|---|---|
| 1A | `atomicfile`, `config`, `secrets`, `state`, `kipu` | Atomic write discipline, hot reload, ack ring (4096, terminal statuses), pututu counter, `pending_notices`, kipu day-files + retention sweep |
| 1B | `graphemes`, `letterid`, `subject`, `ayllu` | Grapheme cap/truncate, letter id (§4.5 incl. missing-`Message-ID` fallback), subject normalise + **header-injection-proof** sanitise (V-3), ayllu store with tombstones, id reuse on re-add, append-only change log |
| 1C | `mailbox`, `strip`, `services/strip`, `deploy/` | IMAP client (IDLE/MOVE/APPEND/UIDVALIDITY), SMTP submitter, strip service + client + Go fallback, compose stack with maddy, golden corpus |

### Wave 2 — M1 letter path
| Agent | Owns | Delivers |
|---|---|---|
| 2A | `derive` | Fetch→enmime→text/plain→resolve (full table)→strip→normalise→truncate; `trimmed`/`truncated`/`degraded`; determinism under fixed config |
| 2B | `syncsvc`, `cmd/wasi` serve | Bearer auth (constant-time SHA-256), coarse status codes, cursor semantics + window resync, budget assembly with `more`, ayllu block on version change, pushed `config`, outbound send + ack idempotency and crash ordering (§4.7), kipu acceptance |
| 2C | `filing`, `tools/chaskisim` | IDLE notification path, startup + per-sync reconciliation, spam-folder backstop; device simulator; e2e harness plumbing |

**Gate:** V-1, V-2, V-4, V-5, V-9, V-10, V-15, V-16, V-21 pass against the compose stack.

### Wave 3 — M2 + M3
| Agent | Owns | Delivers |
|---|---|---|
| 3A | `notice`, release flows in `filing`/`ayllu` wiring | `c_sys` system contact, address-free notice text, `pending_notices` crash ordering (§7.6), optional guardian SMTP copy (§7.5), Held release semantics (§8) |
| 3B | `web`, `guardians`, `cmd/wasi useradd` | Signed stateless cookies + `session_epoch`, argon2id, login throttling, contact CRUD, live Held review + add/reactivate-then-release, change log, device status, read-only `wasi.toml` display, cert-expiry banner, `/healthz` `/readyz` |
| 3C | `carrier`, `pututu` | Registry by config, `hologram`, `fake`, `carriertest` conformance, `ErrUnsupported` degradation, `CH1.<counter>.<mac>` token, coalescing + skip-if-synced, counter reconciliation over the wire (§10.3) |

**Gate:** V-6, V-7, V-13, V-17, V-18, V-19, V-20 pass.

### Wave 4 — verification, ops, docs
| Agent | Owns | Delivers |
|---|---|---|
| 4A | `test/e2e` | Full V-1…V-22 suite wired to the compose stack, including V-11 storage/log grep and V-12 crash consistency |
| 4B | `deploy/`, `cmd/wasi backup`, `contacts` | Distroless static non-root image, read-only rootfs, backup (excluding `kipu/`) + retention, TLS listener wiring and dual-CA tooling notes |
| 4C | docs | Deployment guide (Fastmail spam-filter disable step, CA ceremony, VPN guidance), guardian docs incl. §7.3's stated limitation |

## 4a. Findings against the spec

Recorded here rather than fixed silently, in the spirit of Appendix A. Both were
found while implementing §7 and both change behaviour the spec did not pin down.

**F-1 · A readdress would have deleted history. Extends §7.2 and §7.4.**
§7.1 establishes that in a read-time architecture the contact row is the rendering
key for every letter a person ever sent, and I-5 answers it for *removal*. It does
not answer it for *addresses*. Replacing `Address` on a readdress leaves every
letter that person sent from the old address unresolvable, so the next
reconciliation pass (§5.1) sweeps their entire history into `Held`, and a window
resync after a factory reset loses it — the exact "silently, exactly like spam"
failure §7.1 exists to prevent, arriving through the one door it left open.

Resolution: `Contact.PastAddresses`, retained forever, consulted by `Resolve`
(read time) and never by `ResolveActive` (filing and sending). History renders;
new mail from the old address still goes to `Held`, because a readdress usually
means the person lost that account and mail from it afterwards is precisely what
a guardian should review. Re-adding someone at a past address reuses their id, so
they stay one person in the archive. Covered by `TestReaddress_HistoryStillResolves`
and an I-2 leak test over the device view.

**F-2 · Reconciliation resolves against the full table; only arrival is
active-only. Binding interpretation of §5.1.**
§5.1 says reconciliation quarantines any message "whose sender does not resolve to
a contact", while arrival filing quarantines anything not resolving to an *active*
contact. Read as active-only, reconciliation would sweep a deactivated contact's
already-delivered letters into `Held` on the next pass — which V-6 explicitly
forbids ("first two render with the name, third in `Held`"). §7.2 settles it: "the
decision is made once, at arrival; history is immutable." So reconciliation
quarantines strangers only — senders that do not resolve against the full table,
tombstones and past addresses included. Wave 2 implements it that way.

## 5. Risks tracked during the build

- **maddy ≠ Fastmail.** No spam foldering, IDLE and MOVE semantics differ. V-16 injects the
  condition directly (§15 says so); everything else must not accidentally depend on maddy quirks.
- **`talon` on Python 3.11** may need pinning or a small patch; the Go fallback (§5.3) plus V-10
  means a broken `strip` degrades rather than blocks.
- **Grapheme boundary cases** (emoji ZWJ sequences, combining marks) at the cap are the most
  likely silent divergence between server truncation and panel rendering — V-22 covers them.
- **Crash-ordering tests** (V-9, V-12, V-17) need real process kills, not mocked failures, or
  they test nothing.
