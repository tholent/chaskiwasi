# Plan — Wasi server specification

## Context

`specs/chaskiwasi-design-spec.md` describes the whole system but treats the server (§3) at
the level of intent: "contact resolution," "quarantine, do not bounce," "SQLite, two tables
plus a device cursor." That is enough to argue the design and not enough to build it. There
is no wire format for `POST /sync`, no schema, no failure semantics, and no story for
developing the server before hardware exists.

The repo is currently just the spec, a LICENSE, and a stub README. This plan produces two
things: **`specs/wasi-server-spec.md`**, a build-ready specification for the server, and the
**monorepo skeleton** it will live in. No server code is written here — this is the document
that makes writing it mechanical.

Firmware does not exist yet, so the spec must be executable against a simulated device.
That constraint shapes several decisions below.

---

## Topology

Confirmed with the user, and it is the load-bearing decision:

- **One Wasi container per Chaski device.** No `device_id` anywhere inside Wasi. The
  container *is* the device's identity. Contact list, cursor, mailbox credentials, guardian
  accounts, and the SQLite file are all singular.
- **Two shared support services**, each serving N Wasi instances over a private network:
  - `strip` — Python 3-slim + `talon.quotations` (§3.4)
  - `cell` — OpenCelliD + GeoNames resolver (§4.2), specified now, built in v2

Shared services are stateless w.r.t. any one device and hold **no** device identity, contact
data, or letter content beyond the request being processed. They authenticate with a shared
bearer token and are never exposed publicly.

**Standing invariant, stated once and enforced throughout:** *no component of the server
system persists letter content.* Wasi derives the device's view from IMAP at read time and
keeps only what the mailbox cannot represent (contacts, cursor, config, guardian accounts);
the shared services hold request-scoped data only. This follows from Principle 5 and §3.6,
and it is the constraint most likely to erode quietly under "just cache it" pressure — so the
spec states it as an invariant with a test behind it, not as a design note.

```
  [ Chaski ] --HTTPS--> [ Wasi (one per device) ] --IMAP/SMTP--> [ Fastmail ]
                              |         |
                        [ strip ]   [ cell ]        <- shared, N instances each
```

## Confirmed stack

| Decision | Choice |
|---|---|
| Mail | IMAP + SMTP (`emersion/go-imap` v2 with IDLE, `go-smtp`, `jhillyerd/enmime`) |
| Device auth | Per-device bearer token over TLS, server cert pinned on the modem |
| Quote stripping | `talon.quotations` in the shared `strip` service |
| Guardian UI | `html/template` + htmx, `embed.FS`, no build step |
| Tenancy | Strictly one device per Wasi; no `device_id` |
| Cell DB | Shared `cell` service, runtime-imported dataset on a volume |
| Carrier/SIM | Pluggable `Carrier` interface + registry; `hologram` for v1, `fake` for tests, `soracom` as a drop-in later |
| Guardian auth | Local accounts, argon2id, session cookie, `wasi useradd` |
| **Wasi state** | **No database. Plain files: one human-owned TOML, two server-owned files under `/data`** (see State) |
| Durability | Nightly `cp` of `/data` to a timestamped directory, retained N days |
| Go style | Stdlib-heavy: `net/http` 1.22 routing, `log/slog`, `BurntSushi/toml`, `encoding/json` |

**v1 scope:** core letter path + guardian web UI + `pututu` SMS. The `kipu` and location
rendering are v2 — but see the forward-compat assumption below.

---

## Monorepo layout

```
chaskiwasi/
├── go.mod                      # single root module, covers wasi/ tools/ services/cell/
├── specs/
│   ├── chaskiwasi-design-spec.md
│   └── wasi-server-spec.md     # <- the deliverable
├── wasi/
│   ├── cmd/wasi/               # serve, useradd, backup, contacts
│   ├── internal/{state,mail,ayllu,sync,pututu,web,config,strip,cell}
│   │   └── carrier/{carrier.go,carriertest,hologram,soracom,fake}
│   ├── web/{templates,static}  # embedded
│   └── Dockerfile              # distroless static, non-root, ro rootfs except /data
├── services/
│   ├── strip/                  # Python + talon, Dockerfile
│   └── cell/                   # Go, OpenCelliD importer + resolver
├── tools/chaskisim/            # device simulator (Go)
├── chaski/                     # firmware — placeholder in this plan
├── hardware/{pcb,enclosure,bom}
├── deploy/                     # compose files, systemd unit
└── docs/
```

One root `go.mod` rather than per-component modules: `wasi`, `cell`, and `chaskisim` share
the sync wire types, and a single dependency set is easier to audit for a decade-lived
project. Non-Go directories are inert to Go tooling.

---

## What the spec document must pin down

### 1. `POST /sync` wire format

The whole device protocol. Requirements the spec has to satisfy explicitly:

- **Idempotent under retry.** Device sends `local_id` per outbound letter; server dedupes on
  it. A sync whose response was lost replays cleanly: same cursor, same `local_id`s.
- **One SQLite transaction per sync.**
- **~2KB budget** (§6.1). The `ayllu` payload ships *only* when `ayllu_version` differs;
  letters are capped per sync by count and byte budget with a `more: true` flag.
- **Server paginates** (§3.6). Response carries letters already split into panel-sized pages
  using a `chars_per_page` config value — so the unresolved 2.7"-vs-4.2" question (§11)
  becomes a server config change, not a firmware release.
- **No reply linkage.** Outbound carries a `contact_id`, an optional `subject`, and a body —
  never a parent letter id. See §4 below; this supersedes design-spec §3.5.
- Inbound letters carry a normalised `subject` alongside the pages, since the device list view
  needs it to tell one letter from another.
- **Contacts ship with tombstones.** The `ayllu` payload includes deactivated contacts flagged
  `active: false`, so the device can render the name on an old letter while excluding them
  from the compose picker.
- Response includes `config` (RAT per §6.2, timeouts, cover-screen options) and
  `server_time` (device has no RTC chip, §5.6).

### 2. State — three files, no database

**IMAP is the store.** Principle 5 makes the mailbox canonical and §3.6 puts every transform
at read time *against that mailbox*. Wasi therefore holds only what the mailbox cannot
represent — and that turns out to be ~50 contacts, a cursor, and a list of ids. SQLite,
migrations, and a `store` package are more apparatus than that deserves, so there is no
database. State is plain files, organised by **who writes them**:

| File | Owner | Contents |
|---|---|---|
| `/config/wasi.toml` | **human**, bind-mounted, read-only to the container | IMAP/SMTP endpoints, carrier block, device token hash, `chars_per_page`, timeouts, pushed device config (RAT, cover options), shared-service URLs. Hot-reloaded on change; never written by Wasi. |
| `/data/ayllu.toml` | **server**, hand-editable when stopped | contact id → address, plus the youth's cosmetic overlay (nickname, order, pinned, portrait). The only place addresses exist (§3.1). Guardian UI rewrites it atomically. |
| `/data/guardians.toml` | **server** | argon2id hashes, written by `wasi useradd` and by self-service password change. |
| `/data/state.json` | **machine**, never hand-edited | sync cursor (`uidvalidity` + last delivered UID) and a capped ring of delivered `local_id`s for outbound idempotency. **Ids only — no bodies, no addresses.** |

`/data` is a volume, but bind-mount it too if you want every byte of server state visible on
the host. Sessions are **stateless signed cookies** — no session store at all; a restart logs
guardians out, which at two or three accounts is a non-event.

**Writes are atomic (temp file + `rename`) behind a single writer mutex.** At ~10 syncs a day
and a handful of contact edits, that is not a compromise — it is more than the load requires.

What this buys beyond compactness:
- The storage invariant becomes **inspectable rather than asserted**. Anyone can `cat` the
  complete server state in a few seconds and confirm no letter is in it.
- Backup is `cp -r /data`. Restore is `cp` back.
- One large dependency (`modernc.org/sqlite`) leaves a project meant to still build in a
  decade.

**Deliberate cost:** `ayllu.toml` is rewritten by the guardian UI, so comments and formatting
are not preserved across a UI save. Document that at the top of the generated file.

**v2 `kipu` — day-files, not a table.** `/data/kipu/YYYY-MM-DD.jsonl`, one line per sync,
retention enforced by deleting whole day-files. This is a *better* fit for §3.7 than a
database would be: SQLite `DELETE` leaves rows in freelist pages and the WAL until a
`VACUUM`, whereas §3.7 argues short retention is a safety feature against a hostile
household — "a store that cannot accumulate one cannot have it taken from it." `rm` of a
day-file is an erasure story that survives someone with the disk.

Device bearer token stored as a SHA-256 hash, not plaintext. Guardian passwords argon2id.

### 3. Inbound — read-time derivation, nothing persisted

The IDLE goroutine is a **notification** path, not an ingest path: on new mail it resolves
the sender to decide whether to fire `pututu`, and files non-allowlisted mail. It stores no
bodies.

Filing (on arrival):
- **Unresolved sender** → IMAP MOVE to the `Held` folder, no bounce (§3.3). The Held folder
  *is* the quarantine; the guardian UI reads it live over IMAP. No `held` table.
- **Resolved sender** → left in INBOX, `pututu` fired.

Derivation (at sync time, per §3.6) — for each UID above the device cursor:

1. FETCH from IMAP → `enmime` parse → `text/plain` part only.
2. Resolve From against `ayllu`; drop anything unresolved (default-deny, §3.1).
3. POST to `strip`; honour `format=flowed` soft breaks.
4. **Normalise the subject** — RFC 2047 decode, strip accumulated `Re:`/`Fwd:` prefix chains,
   collapse whitespace, truncate to the device's list width. Inbound subjects are written by
   relatives in ordinary mail clients, so they are real and are never generated; they just
   arrive encoded and prefix-encrusted. With threading gone, a list of *"Re: Re: Fwd: Re:
   camping"* is the failure mode to avoid.
5. Paginate to `chars_per_page`; emit with a `trimmed` flag.

This is deterministic and repeatable, which is what makes a replayed sync safe: re-running it
against the same UIDs produces byte-identical output. Attachments ignored in v1; remote
images never fetched (free — we never render HTML). A UIDVALIDITY change forces a full
resync rather than silent divergence.

**Strip fallback (my assumption, flag if you disagree):** if `strip` is unreachable, apply a
minimal Go rule set (`-- ` delimiter, leading `>` blocks) and mark the letter
`strip_degraded`. A Python container being down should not delay a letter from family, and
since nothing is cached, the next sync re-derives it correctly once the service returns.

### 4. Outbound — the device owns the queue

There is no server-side outbound table, and §4 already says why: *"letters visibly sit there
until the runner takes them."* The Chaski outbox is the queue. Wasi acks only what it has
actually handed to SMTP; anything unacked stays on the device, visible to the kid, and is
resent next sync. A failed send therefore needs no server-side storage to survive — it
survives on the device, which is also the only place the failure is legible as *"still on the
road."*

Per letter, during the sync: resolve `contact_id` → address (**active contacts only** — see
§5); generate a `Message-ID`; take the device's subject or generate one (below); SMTP send;
record `local_id` in the `state.json` ring; ack.

**Subjects: child-authored, server-generated as fallback.** The outbound payload carries an
optional `subject`. If it is present and non-empty the server uses it verbatim; if blank, the
server generates one from the first few words (or "Letter from Rosa" when the body gives it
nothing to work with). Design-spec §3.5 assumed generation was the only source; it should be
the default, not the ceiling.

This follows the pattern the design spec already uses everywhere — guardian-managed canonical,
youth owns the rest (§3.2). Zero friction for a kid who ignores the field, full control for
one who wants it, and no empty-subject archive either way. It matters more here than it would
have under §3.5 because, with threading gone, **the subject is the only context handle the
system has left**.

Three things the spec must pin down, because a child-authored string is now entering an email
header:

- **Header injection.** Strip CR/LF unconditionally — a subject containing a newline can
  inject arbitrary headers. This is the one place untrusted input reaches the SMTP layer.
- **RFC 2047 encoding** for non-ASCII, so accented names and emoji survive the trip.
- **A hard length cap** (~100 chars, well past the ~78 that renders cleanly), enforced
  server-side rather than trusted from the device.

**Screen-budget consequence, flagged for the device side:** §5.3's tidy property that one
500-character letter is exactly one 2.7" screen assumes the whole panel is body text. A
subject row costs one to two of thirteen rows. Either the subject shares the header line with
the recipient name, or usable body drops to ~460 characters. The server's `chars_per_page`
accounting must be told which — it is a layout decision, but it changes a number the server
owns.

**Threading: this spec supersedes design-spec §3.5.** Outbound carries a `Message-ID` and
nothing else — no `In-Reply-To`, no `References`, no `Re:` prefix. §3.5 argues threading
headers are "trivial now, unfixable later," and that is true; the reversal is deliberate
anyway, because the product is passing notes, not conducting email conversations. There is no
reply concept on the device — only "write to this person" — and the humans on both ends hold
the context, which is how paper mail has always worked. The graduated account is then a flat
inbox of notes that looks like the device did, and threading begins from the day they start
using a normal client.

**Record this reversal in the spec explicitly**, with the reasoning, so a future contributor
reading §3.5 finds out it was overturned on purpose rather than dropped by accident. Note the
interaction with the subject decision above: dropping threading is what makes a real,
human-authored subject line worth the screen row it costs.

**Idempotency without storing content:** a replayed sync is deduped against the delivered-id
ring on `local_id`. Without a transaction to lean on, the spec must fix the ordering
explicitly — **record the id and fsync *before* acking, and accept that a crash between SMTP
and the write costs a duplicate send rather than a lost letter.** Duplicate delivery is a
relative seeing a letter twice; the alternative is a letter the kid watched leave that never
arrives. That is the right way to fail, and it is the kind of thing a database would have let
us leave unstated.

### 5. The ayllu lifecycle — removal is deactivation, and changes are announced

**The latent bug:** because §3.1 is default-deny and derivation happens at read time (§3),
*deleting* a contact row would mean their historical letters stop resolving on the very next
sync — and silently get dropped, exactly like spam. The archive would appear to lose them.
Nothing in the design spec catches this, because §3.2 talks about "removing a contact" as if
the table were the only thing it touched. In a read-time architecture it is also the
rendering key for every letter that person ever sent.

**So removal never deletes.** `active = false`, the address is retained, and the row is a
permanent tombstone. The semantics split by *when*:

| Path | Resolves against | Effect of deactivation |
|---|---|---|
| **Filing** (on arrival) | active only | new mail from them goes to `Held`, not INBOX |
| **Derivation** (read time) | **full table, incl. inactive** | old letters still render, with the right name |
| **Sending** | active only | server rejects outbound to an inactive id |

The decision is therefore made once, at arrival, and history is immutable — which is what
makes "you can still read Rosa's old letters, you just can't write to her" fall out of the
architecture instead of needing a special case. The device gets tombstones in the sync payload
so it can show the name on an old letter while hiding the person from the compose picker;
§3.2's deferred "greyed out and unsendable" state is the same UI affordance, arrived at from
the other direction.

**Re-adding the same address reuses the original contact id**, so a person who leaves and
returns stays one person in the archive rather than splitting in two.

**Every ayllu change generates letters in both directions**, reusing §3.7's mechanism rather
than inventing a second one: add, remove, and **address change** each produce a letter to the
child and a notice to guardians. Address change matters as much as the other two — silently
repointing an existing contact id at a new address is precisely how this system would be
turned into a redirection attack, and §3.7's principle ("nothing changes silently, and neither
party can change it behind the other's back") applies with no modification.

- The child's copy is plain and readable by a child: *"Rosa was removed from your list. You
  can still read her old letters, but you can't send her new ones."*
- Notices are delivered by **IMAP APPEND into INBOX**, from a reserved system contact id that
  always resolves, cannot be removed, and cannot be written to. Guardians hold the canonical
  mailbox, so one append notifies both parties — and, importantly, this keeps SMTP as the
  single outbound path, preserving §3.1's "enforced by construction" claim. An optional SMTP
  copy to guardian addresses from `wasi.toml` is available, and must be documented as the one
  narrow exception: fixed addresses from human-owned config, never carrying child-authored
  content.
- These letters **are content and do graduate** — they are the record of who was in the family
  list and when. That is the opposite of the kipu (§9), and correctly so.

Build this as one `internal/notice` package: v1 uses it for ayllu changes, v2 reuses it
unchanged for the tier-2 position toggle.

**One question this raises and the spec should name rather than bury:** a contact removed for
a *safety* reason still has their history readable by the child, by the rule above. That is
the correct default — Principle 5 forbids rewriting the mailbox, and a child discovering
letters had been silently removed is the trust failure §8 warns about. But a guardian removing
someone dangerous may want otherwise, and the honest answer is that this system cannot offer
it: the letters live in a mailbox the guardian controls, so any hiding would be a device-view
filter that the canonical archive contradicts. Say so in the spec instead of leaving it to be
discovered during an argument.

### 6. Guardian web UI

Contact CRUD writing `ayllu.toml` (the only place addresses exist), `Held` review and release
**read live from the IMAP folder**, device status, recent deliveries, guardian account
management. Because the UI reads the mailbox rather than a local mirror, a guardian releasing
a Held message acts on the canonical article — there is no second copy to fall out of sync.

Settings that live in the human-owned `wasi.toml` are **displayed read-only** in the UI, with
the file path shown. Two writers to one file is the failure mode this whole ownership split
exists to prevent, and the UI should make the boundary visible rather than silently omitting
those fields.

**Enforce the §0.1 hard boundary structurally:** a `go test` that fails if `pututu`, `ayllu`,
or `kipu` appear in `wasi/web/templates/` or in any outgoing-mail rendering path. The spec
calls this boundary a matter of discipline; a test makes it not one.

### 7. `pututu` and the pluggable carrier interface

**Policy lives in the core; the provider is a thin transport.** Coalescing (at most one SMS
per ~15 min window), skip-if-recently-synced, retry and backoff are all core `pututu` logic
and are identical across providers. A provider only knows how to ring one SIM and, if it can,
report a balance.

```go
// internal/carrier
type Carrier interface {
    Name() string
    // Pututu rings the device. The payload is an opaque token by contract:
    // no sender, no content. See the privacy note below.
    Pututu(ctx context.Context, payload string) error
    // Balance reports remaining prepaid credit. Returns ErrUnsupported
    // if the provider has no such concept.
    Balance(ctx context.Context) (Balance, error)
}
```

- **Registry by config.** A `[carrier]` block in `wasi.toml` selects the provider by name and
  carries a provider-specific sub-table. This matters because the providers don't agree on
  device identity — Hologram addresses a device id, Soracom addresses an IMSI/SIM id — and
  that difference must not leak into core config or the `pututu` code.
- **Optional capabilities degrade, they don't panic.** `ErrUnsupported` from `Balance` means
  the guardian UI hides the balance panel and the low-credit alert goes quiet. §6.1's
  preloaded-balance strategy and §6.3's billing alerts are Hologram-shaped; a future provider
  may bill differently, and the interface shouldn't pretend otherwise.
- **`carriertest` conformance suite.** A shared table of behaviours every provider must pass
  (rings once, surfaces transport errors, honours context cancellation, returns
  `ErrUnsupported` rather than a zero value). This is what makes "add Soracom easily" a
  property of the code rather than an intention — a new provider is one package plus a
  passing conformance run.
- **`fake` provider** records calls in memory and is what the e2e stack uses. This removes
  the live-account dependency from M3 entirely.

Implement `hologram` for v1 (§6.1 is the shipping choice); leave `soracom` unimplemented but
have the interface shaped by both from the start, so the second one doesn't force a redesign.

**Privacy constraint, provider-independent:** the SMS body carries no sender name and no
content — it is an opaque doorbell token. SMS is unencrypted and buffered by the carrier, so
a name in it would leak exactly what §3.1 protects. This belongs in the `Carrier` contract
docs, not in each implementation, so a future provider can't quietly reintroduce it.

### 8. Shared service contracts

- `POST /strip` → `{text, format_flowed}` → `{body, trimmed, removed_bytes}`
- `POST /resolve` → `{cells:[{mcc,mnc,lac,cid,rssi}]}` → `{lat, lon, radius_m, place, rank}`

**SQLite survives where it earns its place.** The `cell` service holds millions of OpenCelliD
rows needing an index on `(mcc,mnc,lac,cellid)` — that is a real database workload, and it is
a read-only dataset in a shared service that holds no device state. Dropping SQLite from Wasi
is a judgement about Wasi's data, not a ban on the tool.

Reverse geocoding must also be local (§4.2 forbids third-party lookups for a named minor).
Spec **GeoNames `cities1000`** (~10MB) for nearest-populated-place rather than a full
Nominatim stack — it produces exactly the "near Ferndale, MI (within about 3 km)" rendering
the spec asks for, and nothing more precise, which is the point.

### 9. Ops

`/healthz`, `/readyz`, JSON `slog` to stdout, `wasi backup` (a `cp -r` of `/data` to a
timestamped directory, N-day retention), distroless non-root container with a read-only
rootfs except `/data`. Secrets (IMAP password, carrier API key, cookie signing key) come from
env or mounted secret files, never from the TOML — so both TOML files stay safe to keep in the
family's own git repo if they want version history on the contact list, which pairs well with
tombstones never being deleted.

---

## Assumption to confirm at review

**v1 accepts and stores tier-1 `kipu` health even though `kipu` is a v2 milestone.** Battery
%, RAT, signal, queue depth, and last-sync are the fields that make a prototype debuggable,
and accepting the field from day one means firmware never changes shape when v2 lands.
Tier-2 position, the opt-out flow, and cell resolution all stay in v2 — but the symmetric
notification machinery no longer does, since ayllu changes need it in v1 (§5). v2's tier-2
toggle becomes a second caller of an existing `notice` package rather than new mechanism. If
you'd rather v1 reject the kipu field entirely, say so — it's a one-line change.

---

## Milestones

| | Contents |
|---|---|
| **M0** | Monorepo skeleton, TOML config + atomic file state, `chaskisim` skeleton, compose stack with a local IMAP/SMTP fixture |
| **M1** | Core letter path: `/sync`, `ayllu` resolution + tombstones, IDLE + SMTP, strip service, subjects, `Held` |
| **M2** | Guardian web UI + the `notice` package (ayllu change letters) |
| **M3** | `pututu` SMS |
| *v2* | `kipu` tiers, `cell` service, location rendering, tier-2 toggle (reusing `notice`) |

---

## Verification

The point of `chaskisim` is that all of this is testable with zero hardware and no Fastmail
account:

1. `docker compose -f deploy/compose.dev.yml up` — Wasi, `strip`, and **maddy** (Go IMAP+SMTP
   server) as a local mail fixture.
2. `go run ./tools/chaskisim compose --to c_07 --body "..."` → assert the letter lands in
   maddy's mailbox for the right address with a valid `Message-ID`, once with `--subject` set
   (used verbatim) and once without (generated from first words).
3. **Header-injection test:** compose with a subject containing `\r\nBcc: attacker@evil.test`
   → assert the sent message has no injected header and the subject is flattened. Repeat with
   non-ASCII → assert RFC 2047 encoding, and with a 500-char subject → assert the cap holds.
   This is the only path where child-supplied text reaches the SMTP layer, so it gets its own
   test rather than riding along with the compose case.
4. Inject an inbound letter into maddy with a quoted tail and a subject of
   `Re: Re: Fwd: =?utf-8?B?...?=` → assert `/sync` returns it stripped, paginated to the
   configured width, `trimmed: true`, resolved to `c_07`, with the subject decoded and the
   prefix chain collapsed.
5. Send from a non-allowlisted address → assert no bounce, the message in maddy's `Held`
   folder, and nothing on the device.
6. **Deactivation test — the one that guards the bug above.** Deliver two letters from `c_07`,
   remove the contact in the UI, then deliver a third and force a full device resync from
   cursor zero. Assert: the first two still appear *with Rosa's name attached*, the third is
   in `Held`, an outbound to `c_07` is rejected, and the contact arrives at the device flagged
   `active: false`. Re-add the same address → assert the id is reused and history stays under
   one person.
7. Add, remove, and re-point a contact → assert three notice letters appear in INBOX from the
   system contact, that the system contact is unwritable, and that its text contains none of
   `pututu`/`ayllu`/`kipu`.
8. Assert outbound messages carry a `Message-ID` and **no** `In-Reply-To`, `References`, or
   `Re:` prefix — the §3.5 reversal is a deliberate behaviour, so it gets a test that fails
   loudly if someone later "fixes" it.
9. Replay an identical sync request → assert no duplicate SMTP send (idempotency), and that
   re-derived inbound pages are byte-identical.
10. Kill the `strip` container mid-run → assert letters still deliver flagged
    `strip_degraded`, then restart it and assert the next sync re-derives them cleanly with no
    stale cached copy.
11. **Storage invariant test:** run the full e2e flow, then assert no file under `/data`
    contains a substring of any letter sent through the system. Now trivially checkable — and
    eyeball-verifiable by `cat` — which is most of the argument for dropping the database.
12. **Crash-consistency test:** kill Wasi mid-write to `ayllu.toml` and mid-sync, restart, and
    assert the file parses, no contact is lost, and a replayed sync doesn't double-send. The
    temp-file-plus-rename discipline is the thing a database would otherwise have given us for
    free, so it needs its own test.
13. Deliver two letters 30s apart with the `fake` carrier → assert exactly one `pututu` call
    (coalescing), and none at all if the device synced after arrival.
14. `go test ./...` — including the strip golden corpus (`testdata/replies/*.eml`), the
    `carriertest` conformance suite, and the §0.1 vocabulary-boundary test.

With the `fake` carrier, M3 is fully testable offline; the only thing deferred to hardware
bring-up is confirming Hologram's live SMS delivery end to end, which is a single manual
check rather than a blocked milestone.
