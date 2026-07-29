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

**Status:** F-1, F-2, F-5, F-6, F-7 were resolved during the build. F-3 and F-9
were resolved after review, on the user's decision (tell the child on a
permanent send failure; close the removal-during-outage gap). All are now folded
into `wasi-server-plan.md` as decision-log entries A.11–A.18 and inline edits to
§4.7, §5.1, §7.2, §7.6, and §8 — the authoritative spec and this plan agree. F-4
(V-8 vs §6.2 wording) stands as a binding test-writing note, not a code change.


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

**F-3 · §4.7 cannot express a permanent SMTP rejection. RESOLVED (A.11) — new
terminal ack `rejected_undeliverable`.**
The four ack statuses are all terminal, and the send in step 4 precedes the ack
in step 5. That covers transient failure (retry) and pre-send rejection
(`invalid`, `rejected_inactive`, `rejected_unknown_contact`), but nothing covers
a **permanent** SMTP rejection after the send is attempted — a 5xx for a
recipient that will never accept mail. The letter cannot be honestly acked with
any existing status, so it stays in the outbox and the device resends it every
sync, forever, while the child sees "still on the road" indefinitely.

Wave 2 chose infinite retry over mis-acking `invalid`, because losing a letter is
the one failure §4.7 refuses to buy — but that is the least-bad reading of a
contract with a hole in it, not a fix. The fix is a fifth status
(`rejected_undeliverable`), which changes the wire contract and the firmware's
ack handling, so it is the owner's call. Until then the behaviour above stands
and is documented in `internal/syncsvc`.

**F-4 · V-8 conflicts with §6.2 as literally worded.**
V-8 asserts outbound carries no `Re:`; §6.2 says a child-supplied subject is used
*verbatim*. A child who types "Re: camping" produces a letter that fails a literal
substring grep while breaking no rule. The binding reading: **the server must
never add threading headers or a `Re:` prefix**, and V-8 tests that, not the
absence of the substring. The e2e implementation of V-8 must be written the same
way or it will fail on a legitimate letter.

**F-5 · §7.6's ordering cannot make I-4 true on its own. RESOLVED — startup
reconciliation against the change log.**
§7.6 specifies: write `ayllu.toml` → append the change-log line → add to
`pending_notices` → APPEND the notice → remove from `pending_notices`. There is
no transaction, so a crash between the ayllu write and the `pending_notices`
write loses the notice **entirely** — the contact list changed and nothing ever
announced it. That is precisely the I-4 failure ("neither party can alter the
list behind the other's back"), and it survives any amount of care at the call
site: the window is small but never zero, and the package split makes it two
calls rather than one.

`pending_notices` is a fast path, not the durable record. The durable record is
`ayllu-log.jsonl`, which is append-only and written *before* the gap. The fix is
a startup reconciliation: derive each notice's `Message-ID` deterministically
from the event (action + contact id + version + timestamp) rather than randomly,
then at startup compare recent change-log lines against notices present in INBOX
and append any that are missing. That closes the window completely and makes
`pending_notices` a pure optimisation. Implemented: `notice.Reconcile`, called
at startup after `Flush`, over `ayllu.ReadLog` with a 90-day window. Covered by
`TestF5_ChangeWithNoPendingRecordIsAnnouncedLate` and a companion test proving
an already-announced change is not re-announced — without which every restart
would spam the child's inbox with old news.

**F-6 · `c_sys` had no device-visible identity. RESOLVED — ships as a tombstone.**
Notice letters arrive at the device with `contact_id: "c_sys"`, but `c_sys` is
deliberately excluded from the ayllu payload — it is a resolution-only identity,
not a guardian-manageable row. So the device receives letters from a contact id
it has never heard of and cannot render a sender name for. Two Confirmed live: a notice letter reached the simulator
with a `contact_id` no entry in its contact list matched.

Resolved with no wire change. `DeviceView` now includes a synthetic `c_sys`
entry named "Home" (already defined as `ayllu.SystemName`; it satisfies §0's
pronounceability rule and the §0.1 vocabulary boundary) with `active: false`.
That is not a fiction: the device's rule for an inactive contact — render the
name on letters they sent, hide them from the compose picker — is exactly what
§7.4 requires of a contact that cannot be written to. Reusing the tombstone
semantics gets both halves right with no new wire field and no firmware special
case. Verified end to end: the notice renders as "Home" and does not appear in
the compose picker.

The display name is **"Wasi"**, decided rather than defaulted. "Home" was the
first choice and is the wrong one: this device exists for a young person who
moves often, and "home" is a loaded word for a reader who may not have a
settled one, or has more than one, or is being asked to call somewhere home
that they don't. A letter arriving "from Home" quietly asserts something about
the reader's life. "Wasi" asserts nothing, and is legal where the internal
vocabulary is not — it is one of design-spec §0's three public names, chosen to
survive cold reading, not one of §0.1's greppable identifiers.

The name appears in exactly two places a person sees: the sender on the
device, and the `From` display-name on notice letters in the mailbox — which
graduate, so it is the sender on those records for as long as the archive
lasts. That permanence is why it is pinned by a test rather than left to
judgement: an archive whose sender name changes halfway through reads as two
different correspondents.

**F-7 · §8 has no release flow for a sender who is already an active contact.
RESOLVED — `filing.ReleaseActive`.**
Found by 3B while building the Held UI, and reachable in ordinary use: a
guardian adds a stranger on the contacts page, and that person's earlier letter
is still in Held with a sender that now resolves as active. §8 names only
stranger and deactivated, so the message could only be routed through
"reactivate, then release" — which works mechanically and produces a **false
notice letter**: "Rosa was added back to your list" for a change that never
happened. A notice describing a non-change is worse than a missing one, because
I-4's whole promise is that the list changed exactly when a letter says it did.

`filing.ReleaseActive` moves the message and rings the doorbell while mutating
nothing, and returns no event, so nothing is announced. The §8 precondition is
still checked — the sender must resolve to an active contact; here it simply
already does. Covered by `TestF7_ReleasingAnAlreadyActiveSenderAnnouncesNothing`,
which also asserts the letter still arrives: "announce nothing" must not become
"do nothing".

**F-8 · `wasi useradd` on a running server created an account nobody could use.
RESOLVED — the guardian store re-reads on change.**
Found by driving the real UI, not by any unit test. `guardians.FileStore`
parsed `guardians.toml` once at `Open`, but `wasi useradd` is a **separate
process** writing that same file (§9.2, §14). So the account existed on disk,
the CLI printed "sign in now", and the server answered "incorrect username or
password" until it was restarted — with nothing anywhere saying a restart was
needed. The second face of the same bug was worse: the running server's next
write would persist its stale table and **delete** the account the CLI added.

The store now re-reads when the file's mtime or size changes, before every read
and every write, and re-stamps after its own writes. A malformed file keeps the
last good table rather than locking every guardian out. Covered by two tests,
one per face of the bug.

**F-9 · A deactivated contact's new mail is delivered, not held, if Wasi is
down when it arrives. RESOLVED (A.13) — reconciliation holds inactive-resolving
mail above the device's delivery cursor.**
Found by the e2e suite (V-6 and V-18 flaked on it). The split is deliberate and
mostly correct: the IDLE arrival path resolves active-only, so a deactivated
contact's new letter is quarantined to Held (§5.1, §7.2); reconciliation
resolves the full table and holds strangers only, because an active-only
reconcile would sweep that contact's already-delivered history into Held, which
V-6 forbids (this is finding F-2). The consequence: **the deactivated-arrival
quarantine depends entirely on IDLE running.** If Wasi is down at the moment a
deactivated contact sends new mail, reconciliation on the next startup/sync
resolves them against the full table, finds the tombstone, and delivers the
letter as history — the channel a guardian closed reopens for one letter, with
no announcement.

Why this resists a quick fix: distinguishing a deactivated contact's *new* mail
from their *already-delivered history*, both sitting in INBOX resolving to the
same tombstone, needs a per-message "was this examined at arrival while the
sender was active" signal. The IDLE path has it (it sees each message live);
reconciliation, a batch catch-up, does not, and reconstructing it from the
arrival high-water mark breaks when mail arrived-while-active but was not yet
synced before deactivation. A cursor-mirror bound narrows the window but does
not close it and couples filing to state it does not currently hold. Because
this is safeguarding logic (a contact is often deactivated *for* a reason), it
should be designed deliberately, not patched at the end of a build. Options to
weigh: (a) accept it and document that deactivation quarantine is best-effort
during an outage; (b) give filing the delivery-cursor mirror and hold
inactive-resolving mail above it; (c) persist a per-arrival filed-set. Left for
the owner.

## 5. Risks tracked during the build

- **maddy ≠ Fastmail.** No spam foldering, IDLE and MOVE semantics differ. V-16 injects the
  condition directly (§15 says so); everything else must not accidentally depend on maddy quirks.
- **`talon` on Python 3.11** may need pinning or a small patch; the Go fallback (§5.3) plus V-10
  means a broken `strip` degrades rather than blocks.
- **Grapheme boundary cases** (emoji ZWJ sequences, combining marks) at the cap are the most
  likely silent divergence between server truncation and panel rendering — V-22 covers them.
- **Crash-ordering tests** (V-9, V-12, V-17) need real process kills, not mocked failures, or
  they test nothing.
