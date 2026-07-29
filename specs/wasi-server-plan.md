# Wasi Server Specification

Status: draft for review · Companion to `specs/chaskiwasi-design-spec.md`
Scope: the per-device server ("Wasi"), its wire protocol, state, shared services, and trust model. Firmware requirements are noted only where the server contract imposes them.

This document is build-ready: it supersedes the design spec where they conflict, and every supersession is recorded in Appendix A with its reasoning. A future contributor who reads design-spec §3.5 and finds it contradicted here should land in A.1, not conclude something was dropped by accident.

---

## 0. Reading guide and vocabulary

- **Chaski** — the child's e-ink device. **Wasi** — this server, one container per device. **Ayllu** — the contact list. **Pututu** — the SMS doorbell. **Kipu** — device health telemetry.
- These four words are internal vocabulary. Per design-spec §0.1 they MUST NOT appear in guardian-facing UI text or in any mail the system generates. This boundary is enforced by test (§15, V-14), not discipline.
- RFC 2119 keywords (MUST/SHOULD/MAY) are used with their usual meaning.
- "Grapheme" below always means an extended grapheme cluster per Unicode UAX #29 — the unit a reader perceives as one character. All character caps in this spec count graphemes, never bytes or code points. (Implementation note: `rivo/uniseg` is the accepted small dependency for this; byte and rune counts silently disagree with what the e-ink panel renders the moment an emoji or combining accent appears.)

## 1. Invariants

Stated once, enforced everywhere, each backed by a named test in §15.

**I-1 · No component of the server system persists letter content.** The mailbox (IMAP at Fastmail) is the sole store of letters. Wasi derives the device's view at read time and persists only what the mailbox cannot represent: contacts, cursor mirror, config, guardian accounts, counters. The shared services hold request-scoped data only. This invariant explicitly covers **logs**: letter bodies and subjects MUST NOT appear in log output of Wasi or any shared service, at any log level. (Test V-11 greps both `/data`, backups, and captured container logs.)

**I-2 · The device never sees an email address.** Addresses exist in exactly one place: `/data/ayllu.toml` (and its append-only change log). No sync response field, no notice letter, and no error string delivered to the device may contain an address. Notice letters announcing an address change name the person, never the address (§7.4).

**I-3 · SMTP carries only child-authored letters**, plus one narrow, documented exception: optional operational copies to guardian addresses fixed in human-owned config (§7.5, §12.4). No auto-replies, no bounces, no courtesy notifications to relatives (see A.4).

**I-4 · Nothing about the ayllu changes silently.** Every add, deactivation, reactivation, and address change produces a notice letter into the canonical mailbox (§7.4) and a line in the change log. Neither party can alter the list behind the other's back.

**I-5 · Removal never deletes.** A removed contact becomes a permanent tombstone (`active = false`, address retained). History remains renderable forever; only filing and sending consult the active flag (§7.2).

### 1.1 Anti-requirements

The following are omitted from the wire protocol and the UI **on purpose**. They are the easiest things to "helpfully" add later, and each one would break a promise the system makes:

| Deliberately absent | Because |
|---|---|
| Any Held-folder count, flag, or hint in the sync response | The device must not become a surveillance indicator; quarantine is a guardian-side concept only. |
| Email addresses anywhere device-visible | I-2. |
| Read/unread state, delivery receipts, per-contact counters on the server | The device owns its view; the server holds no engagement state. |
| Threading headers (`In-Reply-To`, `References`, `Re:` prefixes) on outbound | A.1 — the product is passing notes, not conducting email. |
| Reply linkage in the wire format (no parent-letter id anywhere) | Same. |
| Letter content in kipu, state, logs, or backups | I-1. |

## 2. Topology

One Wasi container per Chaski device. The container **is** the device's identity: there is no `device_id` anywhere in Wasi's code, config, or state. Contact list, cursor, mailbox credentials, guardian accounts, and all files are singular.

```
  [ Chaski ] --HTTPS (private CA)--> [ Wasi ] --IMAP/SMTP--> [ Fastmail ]
  [ Guardian browser ] --HTTPS (LE)-->  |
                                        |--> [ strip ]   shared, stateless
                                        '--> [ cell ]    shared, v2
```

Shared services (`strip`: Python + `talon.quotations`; `cell`: OpenCelliD + GeoNames, specified in §11, built in v2) serve N Wasi instances over a private network, authenticate with a shared bearer token, are never exposed publicly, and hold no device identity or content beyond the request being processed.

Wasi exposes two TLS listeners from one binary (§12): the device sync endpoint (private-CA certificate) and the guardian web UI (public certificate). They share nothing but the process.

## 3. State — plain files, no database

IMAP is the store (design-spec Principle 5, §3.6). What the mailbox cannot represent fits in a handful of files, organised by **who writes them**:

| File | Owner | Contents |
|---|---|---|
| `/config/wasi.toml` | **Human.** Bind-mounted read-only to the container; hot-reloaded on change; never written by Wasi. | Mail endpoints, carrier block, device-token hash, owner name, protocol knobs (§13), pushed device config, shared-service URLs, guardian copy addresses. |
| `/data/ayllu.toml` | **Server**; hand-editable when Wasi is stopped. | Contact id → address, active flag, plus the youth's cosmetic overlay (nickname, order, pinned, portrait glyph id). The only place addresses live. Rewritten atomically by the guardian UI; comments/formatting are not preserved across a UI save, and the generated file says so in a header comment. |
| `/data/ayllu-log.jsonl` | **Server**, append-only. | One JSON line per ayllu change event: timestamp, actor (guardian username), action, contact id, and for address changes the old and new address. This is the guardian-facing audit trail (§7.4); addresses here are permitted by I-2 because the file is never device-visible. |
| `/data/guardians.toml` | **Server.** | Per guardian: argon2id hash and a `session_epoch` integer (§9.2). Written by `wasi useradd` and by self-service password change. |
| `/data/state.json` | **Machine**, never hand-edited. | Cursor mirror (`uidvalidity` + last delivered UID), the outbound ack ring (§4.7), `pututu_counter` (§10.2), `last_sync_at`, and `pending_notices` (§7.6). **Ids, integers, and timestamps only — no bodies, no subjects, no addresses.** |
| `/data/kipu/YYYY-MM-DD.jsonl` | **Machine.** | One line per sync: the request's kipu block plus a timestamp. Retention is enforced by deleting whole day-files older than `kipu.retention_days` (default 14), at startup and daily. Whole-file `rm` is the erasure story (design-spec §3.7): no freelist pages, no WAL, nothing to VACUUM. |

**Write discipline.** All server-owned files are written atomically: temp file in the same directory, `fsync` the file, `rename`, `fsync` the directory — behind a single writer mutex. At ~10 syncs a day this is more than the load requires; it is what a database would otherwise have provided, and it gets its own crash test (V-12).

**Backups.** `wasi backup` copies `/data` — **excluding `kipu/`** — to a timestamped directory under the backup volume (`/backups` by default), retained `backup.retain_days` (default 7). Kipu is excluded so that its retention window means what it says; backing it up would silently extend it by the backup retention and put deleted day-files back within reach of exactly the adversary §3.7 contemplates. Restore is `cp` back plus one sync (the pututu counter self-heals over the wire, §10.3). The storage-invariant test greps `/data` **and** `/backups`.

**Secrets** (IMAP password, carrier API key, cookie-signing key, pututu HMAC key, shared-service token) come from environment variables or mounted secret files, never from TOML — so both TOML files remain safe to keep in the family's own git repository if they want version history on the contact list. The device-token *hash* (SHA-256 of a high-entropy random token) lives in `wasi.toml` and is safe there.

## 4. Device protocol — `POST /sync`

The entire device↔server surface is one endpoint. The device initiates every exchange; the server never contacts the device except through the pututu doorbell (§10), which carries no information other than "sync now."

### 4.1 Transport

- HTTPS, TLS ≥ 1.2, on the device listener (§12.1). The device trusts only the two pinned private CAs.
- Authentication: `Authorization: Bearer <device-token>`. Wasi compares SHA-256 of the presented token against the hash in `wasi.toml`, constant-time.
- `Content-Type: application/json; charset=utf-8`, both directions. No content-encoding: at ~2 KB payloads, gzip saves a few hundred KB/month against a per-MB bill and buys a firmware dependency in exchange (A.5). Revisit only if billing data says otherwise.
- HTTP status semantics are deliberately coarse:

| Status | Meaning | Device behaviour |
|---|---|---|
| `200` | Sync processed; full response body | Apply response |
| `401` | Bad/missing token | Show provisioning-fault state; do not retry hot |
| `503` (+`Retry-After`) | IMAP or a required upstream unreachable | Retry same request after backoff |
| anything else | Transient failure | Retry the identical request later |

Retrying the identical request is always safe: inbound is dedup-keyed by letter id (§4.5), outbound by the ack ring (§4.7). There are no partial-success HTTP codes; partial outcomes live in `acks`.

- Server-side request-size cap: 64 KB (defensive; a full 12-letter outbox is under 10 KB).

### 4.2 Request schema

```json
{
  "cursor": "b64…",              // opaque, server-issued; "" = window resync (§4.4)
  "ayllu_version": 7,
  "pututu_counter_seen": 41,     // highest SMS counter the device has accepted (§10.3)
  "kipu": {                      // optional; stored per §3 from M1
    "battery_pct": 84, "rat": "ltem", "rssi": -97,
    "queue_depth": 1, "fw": "0.3.1"
  },
  "outbound": [
    {
      "local_id": "o-000123",    // device-assigned, ≤32 bytes, unique per letter
      "contact_id": "c_07",
      "subject": "camping!",     // optional; absent/empty ⇒ server generates (§6.2)
      "body": "…"                // ≤ max_letter_chars graphemes (§4.6)
    }
  ]
}
```

All fields except `cursor` and `ayllu_version` are optional. An empty sync (`{"cursor": "…", "ayllu_version": 7}`) is the normal heartbeat.

### 4.3 Response schema

```json
{
  "server_time": 1785420202,     // epoch seconds; device has no RTC
  "cursor": "b64…",              // store and echo next sync
  "pututu_counter": 41,          // server's current counter (§10.3)
  "acks": [
    { "local_id": "o-000123", "status": "sent" }
  ],
  "letters": [
    {
      "id": "l-9f3a2c41d0",      // stable letter identity (§4.5)
      "contact_id": "c_07",
      "subject": "camping",      // normalised (§5.4), ≤100 graphemes
      "date": 1785349200,        // mailbox Date, sanity-checked vs INTERNALDATE
      "body": "…",               // ≤ max_letter_chars graphemes; device reflows (§4.9)
      "trimmed": true,           // quoted tail removed by strip
      "truncated": false,        // body cut at max_letter_chars (§4.6)
      "degraded": false          // strip unreachable; Go fallback rules used (§5.3)
    }
  ],
  "more": false,
  "ayllu": {                     // present ONLY when request ayllu_version ≠ current
    "version": 8,
    "contacts": [
      { "id": "c_07", "name": "Rosa", "active": false,
        "pinned": false, "order": 3, "portrait": "p04" }
    ]
  },
  "config": {                    // pushed device config, from wasi.toml
    "max_letter_chars": 500,
    "sync_interval_s": 21600,
    "rat": "ltem",
    "cover": "…"
  }
}
```

`trimmed` and `truncated` are distinct flags for distinct events — quote-stripping versus length-capping — and the device may render them differently ("…" vs "letter continues in the archive"). Note there is no `pages` field: see §4.9. `portrait` is a glyph identifier from the device's built-in set; image bytes never cross this wire in v1.

### 4.4 Cursor semantics

- The cursor is **opaque to the device**: a base64 string the server mints and the device stores verbatim and echoes. Internally it encodes `(uidvalidity, last-delivered UID)`; the device MUST NOT parse it.
- The **device is authoritative**: whatever cursor arrives in the request is where delivery resumes. The copy in `state.json` is a mirror used only for the pututu skip-if-recently-synced check and operator display — it never overrides the request.
- `cursor: ""` means **window resync**: the server delivers the most recent `resync_window` letters (default 200, §13) rather than the full multi-year archive. A device recovering from factory reset re-syncs a bounded, recent view; the deep archive lives in the mailbox and graduates with it.
- A cursor whose `uidvalidity` no longer matches the mailbox is treated exactly as `""`. UIDVALIDITY resets are therefore invisible on the wire: the server re-delivers, the device dedups (§4.5), and no special firmware path exists.

### 4.5 Letter identity and device-side dedup

- Every inbound letter carries `id`: `"l-"` + the first 10 lowercase hex characters of SHA-256 over the raw `Message-ID` header. Stable across UIDVALIDITY resets, re-derivation, and window resyncs; never exposes the raw header (which can leak the sender's hostname).
- If `Message-ID` is absent (rare, legal), the hash input is `From` + `Date` + the first 1 KB of the raw body, and the letter is treated normally.
- **Firmware requirement (wire contract):** the device MUST remember at least its **1000** most recently seen letter ids (≈12 KB of flash) and silently drop repeats. The server MAY re-send any previously delivered letter at any time; correctness never depends on it not doing so.

### 4.6 Budgets and limits

| Limit | Value | Enforced by |
|---|---|---|
| Letter body | `max_letter_chars` = 500 graphemes | Server, both directions (§5.2 inbound truncation; outbound `invalid` ack beyond it) |
| Subject on the wire | 100 graphemes | Server (§5.4, §6.2) |
| Contacts | `max_contacts` = 24 | Guardian UI, with a clear error (A.3) |
| Response size | ~2 KB steady-state **target** | Server assembly, below |
| `more` loop | ≤ 10 immediate rounds | Device |

The 2 KB figure is a cost target (LTE-M billed per MB), not a transport ceiling. Assembly rule: add whole letters until the next would push the response past `sync_budget_bytes` (default 2048), **but always include at least one complete letter**; set `more: true` if UIDs remain. Letters are atomic on the wire — a 500-grapheme body is ≤ ~2 KB even for a worst-case all-emoji letter, and sub-letter continuation is complexity without a customer. The `ayllu` block is exempt from the budget: at ≤24 contacts it is ~1.2 KB worst-case, it ships only on version change, and a half-applied contact list would be worse than a 3 KB response.

On `more: true` the device SHOULD sync again immediately, looping until `false`, hard-capped at 10 rounds per wake as a defense against server bugs. A drain loop counts as **one** "recently synced" event for pututu coalescing, not N.

### 4.7 Outbound handling and idempotency

Per outbound letter, in order:

1. Resolve `contact_id` against the ayllu — **active contacts only** (§7.2). Failure ⇒ ack `rejected_inactive` or `rejected_unknown_contact`.
2. Validate: non-empty body, ≤ `max_letter_chars` graphemes, known fields. Failure ⇒ `invalid`.
3. Sanitise the subject or generate one (§6.2). Generate a `Message-ID`.
4. SMTP send via Fastmail submission. A **permanent** rejection (a 5xx reply — the recipient address no longer exists) ⇒ ack `rejected_undeliverable`. A **transient** failure (a 4xx reply, or the submission server unreachable) is not an ack: the letter is left unacked to be retried, and an unreachable server ends the whole exchange in a 503 (§4.1). See A.11.
5. Record `(local_id, status)` in the ack ring in `state.json`, **fsync, then ack**.

The ack ring holds the last **4096** entries — comfortably above any plausible device outbox — and rejects are recorded in it too, so a replayed sync receives the *same* rejection rather than a retry. **All ack statuses are terminal**: on any ack the device removes the letter from its outbox. `sent` means handed to SMTP; the reject statuses — `rejected_inactive`, `rejected_unknown_contact`, `invalid`, and `rejected_undeliverable` — mean the device should surface "couldn't send — ask your guardians" while keeping the text visible to the child rather than vanishing it (firmware behaviour, but the wire contract states that an ack implies no resend, ever).

**Crash ordering is explicit because there is no transaction:** a crash between step 4 and step 5 costs a **duplicate send** on replay, never a lost letter. A relative seeing a letter twice is the correct failure; a letter the kid watched leave that never arrives is not. (Test V-9.)

There is no server-side outbound queue (design-spec §4): the Chaski outbox is the queue, anything unacked stays on the device — visible to the child as "still on the road" — and is re-sent next sync.

### 4.8 Kipu acceptance

The `kipu` block is accepted from day one and stored to the day-files from **M1** (A.6). v1 stores tier-1 fields only (battery, RAT, signal, queue depth, firmware); tier-2 position, the opt-out flow, and cell resolution remain v2, which reuses the `notice` package (§7.4) for its toggle announcements rather than new machinery. Unknown fields inside `kipu` are stored as received (forward-compatible), but the total block is capped at 512 bytes.

### 4.9 Rendering is device-owned — the server ships text, not layout

Each letter arrives as a single `body` string. Pagination, line breaking, and font metrics live entirely in firmware, because screen capacity is not a per-device constant: font size is an accessibility setting the reader may change at runtime, and a page break computed on the server is stale the moment they do — fixable only by re-downloading over a per-MB link. Reflowing ≤ 500 graphemes is trivial work for 2 MB of RAM, and it makes a font change instant and free.

The server retains the two *content* knobs — `max_letter_chars` (how much of a letter exists on the device at all) and the subject cap — and owns zero layout numbers. **Firmware requirement (wire contract):** line-break on grapheme-cluster boundaries, never inside one. (History: A.10.)

## 5. Inbound — read-time derivation, nothing persisted

### 5.1 Filing (on arrival) and reconciliation

The IDLE goroutine on INBOX is a **notification** path, not an ingest path. On new mail it resolves the sender and:

- **Resolved to an active contact** → leave in INBOX, fire pututu (subject to coalescing, §10.1).
- **Anything else** (stranger, or deactivated contact) → IMAP `MOVE` to the `Held` folder. No bounce, ever (design-spec §3.3). The Held folder *is* the quarantine; there is no `held` table.

**Reconciliation — filing must not depend on uptime.** At startup and at the top of every sync, Wasi scans INBOX and MOVEs to Held any message that does not belong there. Derivation (§5.2) never silently drops mail: if Wasi was down when such mail arrived, reconciliation quarantines it before the cursor can pass it by. (Test V-15.) Two rules, resolving against the **full** table, never `ResolveActive` (A.12):

1. **Strangers** — a sender that resolves against nothing (tombstones and past addresses included) — are always quarantined.
2. **A removed contact's not-yet-delivered mail** — a sender that resolves to an *inactive* contact, for a message whose UID is **above the device's own delivery cursor** — is quarantined too (A.13). This is the mail that arrived while Wasi was down, that the arrival path (active-only, above) never saw, and that would otherwise resolve against the tombstone at read time and be delivered as history. Mail at or below the delivery cursor — the child's already-delivered history — is left untouched, so this never sweeps history (§7.2). The delivery boundary is the device's authoritative cursor and is applied only when that cursor is concrete; a window resync (§4.4) falls back to strangers-only, so a factory-reset device cannot trigger a sweep.

Reconciliation deliberately does **not** re-decide a message whose arrival the active-only path already ruled on — see §7.2 and A.12.

**Spam-folder backstop.** Provider-side spam filtering is the one path by which family mail could vanish without anyone — device or guardian — ever learning it existed. Two defenses, both required:

1. **Setup step:** the deployment docs instruct disabling Fastmail's spam filtering for this mailbox (it is allowlist-only by construction; a spam filter can only ever cost it good mail), with a prominent note that until this is done, the provider's filter may be clobbering good mail invisibly.
2. **Backstop:** Wasi checks the provider's Spam/Junk folder at startup, at each sync, and at least every 15 minutes, MOVE-ing anything found there to Held — where the guardian review flow will see it. (Test V-16.)

### 5.2 Derivation (at sync time)

For each INBOX UID above the request cursor, in UID order:

1. `FETCH` → `enmime` parse → `text/plain` part only. HTML is never rendered; remote resources are never fetched (free — nothing renders them). Attachments ignored in v1.
2. Resolve `From` against the **full ayllu, tombstones included** (§7.2) — after reconciliation, everything left in INBOX resolves. Compute the letter id (§4.5).
3. `POST /strip` (§11.1); honour `format=flowed` soft breaks.
4. Normalise the subject (§5.4).
5. **Truncate** the body at `max_letter_chars` graphemes; set `truncated` if cut. The full text remains untouched in the mailbox and graduates intact — truncation shapes the device's view only.
6. Emit the body with flags; layout is device-owned (§4.9).

Derivation is deterministic: the same UIDs under the same config yield byte-identical output, which is what makes replays and window resyncs safe. **Determinism is scoped to unchanged config** — a changed `max_letter_chars` legitimately changes body bytes on the next resync, and the device's dedup-by-letter-id (not by body bytes) is what makes that a non-event.

### 5.3 Strip fallback

If `strip` is unreachable, apply a minimal in-process rule set — cut at an `-- ` signature delimiter, drop leading `>`-quoted blocks — deliver the letter flagged `degraded`, and let the next sync after the service returns re-derive it cleanly. A Python container being down must not delay a letter from family; since nothing is cached, there is no stale copy to invalidate. (Test V-10.)

### 5.4 Inbound subject normalisation

Inbound subjects are written by relatives in real mail clients: they arrive RFC-2047-encoded and prefix-encrusted, and with threading gone (A.1) they are the device's only list-view context. Normalisation: RFC 2047 decode → strip repeated `Re:`/`Fwd:`/`Fw:` (and common localised variants) case-insensitively → collapse whitespace → cap at 100 graphemes. Subjects are real and are never generated for inbound mail.

## 6. Outbound — subjects and the SMTP boundary

### 6.1 What outbound is

`Message-ID` and nothing else: no `In-Reply-To`, no `References`, no `Re:` prefix, no reply concept on the wire (A.1, test V-8). From: the child's address; To: the resolved contact address; Date: server clock.

### 6.2 Subjects — child-authored, server-generated fallback

If the outbound payload carries a non-empty `subject`, use it verbatim (after sanitisation). If not, generate one from the first few words of the body, capped at ~40 graphemes, falling back to `"Letter from {owner.name}"` when the body gives nothing to work with. Design-spec §3.5 assumed generation was the only source; it is the default, not the ceiling (A.2) — zero friction for a kid who ignores the field, full control for one who doesn't, no empty-subject archive either way.

**This is the one place child-supplied text enters an email header**, so sanitisation is specified here and tested on its own (V-3):

- Strip CR, LF, and all other control characters unconditionally — a newline in a subject is a header-injection vector.
- RFC 2047 encode non-ASCII so accented names and emoji survive.
- Hard cap at 100 graphemes, enforced server-side, never trusted from the device.

**Screen-budget note for the device team:** a subject row still costs one to two rows of a small panel at the default font — but with layout fully device-owned (§4.9, A.10), whether the subject shares the header line with the sender name is a firmware decision that no longer changes any number the server holds.

## 7. The ayllu lifecycle

### 7.1 The latent bug this section exists to prevent

Default-deny resolution (design-spec §3.1) plus read-time derivation means *deleting* a contact row would make their historical letters stop resolving on the next resync — silently, exactly like spam. In a read-time architecture the contact row is the rendering key for every letter that person ever sent. So:

### 7.2 Removal is deactivation

`active = false`; the address is retained; the row is a permanent tombstone. The semantics split by *when* resolution happens:

| Path | Resolves against | Effect of deactivation |
|---|---|---|
| **Filing / reconciliation** (arrival) | active only | new mail from them → `Held` |
| **Derivation** (read time) | full table, incl. inactive | old letters still render, correct name |
| **Sending** | active only | outbound to an inactive id → `rejected_inactive` |

The decision is made once, at arrival; history is immutable. "You can still read Rosa's old letters, you just can't write to her" falls out of the architecture. The device receives tombstones in the ayllu payload (`active: false`) so it can show the name on an old letter while hiding the person from the compose picker. The reserved system contact `c_sys` (§7.4) ships in that payload the same way — `active: false` — so the device can name the sender of a notice letter without a new wire field or a firmware special case (A.15). Its device-visible name is **Wasi**, chosen deliberately over "Home" (A.16).

**Re-adding an address reuses the original contact id** (matched against tombstones), so a person who leaves and returns stays one person in the archive.

**Address change retains the old address, forever, for read-time resolution only (A.14).** A readdress moves the previous address to a retained `past_addresses` list, which `Resolve` (read time) consults and `ResolveActive` (filing, sending) does not. Without this, replacing the address would make every letter that person sent from the old one stop resolving — on the next reconciliation pass their whole history would be swept into Held, and a window resync would lose it. That is §7.1's failure arriving through the address rather than the row, and I-5 ("removal never deletes") is what forbids it. New mail from a past address still goes to Held, because a readdress usually means the person lost access to the old account.

### 7.3 The honest limitation, stated rather than discovered

A contact removed for a *safety* reason still has their history readable by the child. That is the correct default — Principle 5 forbids rewriting the mailbox, and a child discovering letters were silently removed is the trust failure design-spec §8 warns about — and it is also a hard limit: the letters live in a mailbox the guardians control, so any hiding would be a device-view filter the canonical archive contradicts. This system cannot offer retroactive removal, and guardians should learn that from the documentation, not during an argument.

### 7.4 Every change is announced

Add, deactivate, reactivate, and **address change** each produce, via the single `internal/notice` package:

- One letter, **APPEND**ed to INBOX from the reserved system contact `c_sys` — which always resolves, cannot be deactivated, and cannot be written to. Guardians hold the canonical mailbox, so one append informs both parties, and SMTP remains the single outbound path (I-3).
- The letter is worded for the child and **contains no addresses** (I-2): *"Rosa was removed from your list. You can still read her old letters, but you can't send her new ones."* / *"Rosa's address was updated by Dad."* Guardians who need the actual old/new address find it in the change log (`ayllu-log.jsonl`), surfaced in the web UI.
- One line in `ayllu-log.jsonl` with full detail, including addresses.

Address change is announced with the same weight as add/remove because silently repointing an existing contact id at a new address is precisely how this system would be turned into a redirection attack; design-spec §3.7's principle applies unmodified.

Notice letters **are content and do graduate** — they are the record of who was in the list and when. (The kipu, by contrast, is designed to be forgotten; the asymmetry is deliberate.)

### 7.5 The narrow SMTP exception

An optional SMTP copy of each notice to guardian addresses listed in `wasi.toml` MAY be enabled. It is the single exception to I-3 and is bounded by construction: fixed addresses from human-owned config, system-generated text only, never carrying child-authored content.

### 7.6 Crash ordering for changes

Order: write `ayllu.toml` (atomic, fsync) → append the change-log line → add the event to `pending_notices` in `state.json` (fsync) → APPEND the notice → remove from `pending_notices`. At startup, Wasi flushes any surviving `pending_notices`. The purchasable failure is therefore "notice arrives a little late," never "change happened silently" (I-4) and never "notice for a change that didn't stick." (Test V-17.)

`pending_notices` alone does not make I-4 literally true (A.18): a crash *between* the `ayllu.toml` write and the `pending_notices` write leaves a changed list with no record that a notice was owed, and the flush has nothing to recover. The change log — append-only, written before that gap — is the durable record that closes it. Each notice's `Message-ID` is derived deterministically from its event (action + contact id + version + timestamp), so at startup Wasi reconciles recent change-log events against the mailbox and APPENDs any whose notice is not already there. This is also what lets `wasi contacts` mutate the list with the server stopped: the change is durable, and the next `serve` announces it (§14).

## 8. Held and release

The guardian UI reads the Held folder **live over IMAP** — there is no mirror to fall out of sync, and releasing acts on the canonical article.

**Release requires the sender to resolve to an active contact.** The UI enforces this as one flow, with no dead ends:

- Sender is a **stranger** → the release control becomes **"add as contact, then release"**: one action creates the contact (subject to `max_contacts`), fires the add notice (§7.4), and MOVEs the message to INBOX.
- Sender is a **deactivated contact** → the control becomes **"reactivate, then release"**, firing the reactivation notice. A guardian who wants to deliver one old letter *without* re-opening the channel reactivates, releases, and deactivates again — three deliberate, announced actions; the UI documents this sequence rather than offering a silent one-off override.
- Sender is **already an active contact** → release moves the message and rings pututu, and **announces nothing**, because nothing about the list changed (A.17). This case is reachable and ordinary — a guardian adds a stranger on the Contacts page, and that person's earlier letter is still in Held with a now-resolving sender. Routing it through "reactivate, then release" would work mechanically and put a false statement into the child's inbox ("added back to your list" for a change that never happened), which corrupts the family record worse than a missing notice would.

A released message receives a new UID above the cursor and flows through derivation like any arrival (its sender now resolves — for delivery, even a re-deactivated tombstone resolves at read time). **Release fires pututu**, subject to normal coalescing: a released letter is an arriving letter from the child's point of view. Nothing ever silently vanishes on release (test V-18); the pre-fix behaviours — stranger-release evaporating at derivation, inactive-release delivering by accident of the table split — are both specified away.

Held messages have no retention limit; they sit until a guardian acts. The Held folder is mailbox content and graduates with it.

## 9. Guardian web UI

### 9.1 Scope

`html/template` + htmx from `embed.FS`, no build step. Surfaces: contact CRUD (writing `ayllu.toml`, the only place addresses exist), Held review and release (§8), the ayllu change log, device status (last sync, battery, signal — read from the newest kipu line), recent deliveries (ids and timestamps only, never content), carrier balance when supported (§10.4), certificate status (§12.3), and guardian account management.

Settings that live in the human-owned `wasi.toml` are displayed **read-only with the file path shown**. Two writers to one file is the failure mode the ownership split exists to prevent; the UI makes the boundary visible instead of silently omitting those fields.

The §0.1 vocabulary boundary is enforced structurally: a `go test` fails if `pututu`, `ayllu`, or `kipu` appears in `wasi/web/templates/` or in any outgoing-mail rendering path (V-14).

### 9.2 Sessions and login

- **Stateless signed cookies** — HMAC (key from secrets) over `{guardian, expiry, session_epoch}`. No session store; a server restart logs everyone out, which at two or three accounts is a non-event.
- **Password change invalidates every existing session for that account:** the per-guardian `session_epoch` in `guardians.toml` is incremented on password change, and cookies carrying a stale epoch are rejected. This matters in exactly the hostile-household scenario the design spec contemplates — a lock change that leaves old keys working is not a lock change. (Test V-19.)
- **Login throttling:** constant-time verification, a ~1 s fixed delay on failure, and per-account exponential backoff after 5 consecutive failures (capped at 60 s). argon2id already prices offline attack; this prices online guessing.
- **Exposure:** the guardian listener binds for LAN/VPN reach by default. Public exposure is possible and documented as **at your own risk**, with the rate limits above as the minimum bar; the deployment docs recommend a VPN (e.g. WireGuard) as the intended remote path.

## 10. Pututu and the carrier interface

### 10.1 Policy (core, provider-independent)

Coalescing — at most one SMS per ~15-minute window; skip entirely if the device synced since the triggering arrival; retry with backoff on transport error. A `more`-drain loop counts as one sync for the skip check. Release from Held triggers the same path as arrival (§8).

### 10.2 The token — signed, counter-based

The SMS body is an opaque doorbell token: **no sender name, no content** — SMS is plaintext and carrier-buffered, and a name in it would leak exactly what design-spec §3.1 protects. This constraint lives here, in the contract, so no future provider can quietly reintroduce it.

Format: `CH1.<counter>.<mac>` where `counter` is a decimal monotonic integer from `state.json` (incremented per SMS sent) and `mac` is base64 of the first 12 bytes of HMAC-SHA256 over the ASCII counter, keyed by the **pututu key** — a dedicated shared secret provisioned alongside the bearer token and supplied to Wasi via secrets, never TOML. (It must be a separate key: Wasi deliberately stores only a *hash* of the bearer token and therefore cannot MAC with it.) The whole token fits comfortably in one GSM-7 SMS.

**Device rules (wire contract):** verify the MAC; accept only if `counter` is strictly greater than the highest previously accepted value; persist that value across power loss; ignore failures **silently** — no response, no wake. Independently, rate-limit SMS-triggered wakes to at most one per 5 minutes regardless of validity, so even a validation bug cannot become a battery- or balance-drain attack. Counter-based rather than time-based because the device has no trustworthy clock except just after a sync.

### 10.3 Counter reconciliation — why it is on the sync wire

Restoring `state.json` from backup rolls the server's counter backward; every subsequent SMS would then carry an already-seen counter and the device would correctly ignore it — a permanently silent doorbell with nothing visibly broken. Hence the two integer fields in §4.2/§4.3: the response carries the server's `pututu_counter`; if the device's stored value is higher, the device reports it as `pututu_counter_seen` and the server jumps its counter past it. Restore-plus-one-sync makes the system coherent again, which is what keeps "restore is `cp` back" true. (Test V-20.)

### 10.4 Carrier interface

```go
// internal/carrier
type Carrier interface {
    Name() string
    // Pututu rings the device. The payload is an opaque token by contract:
    // no sender, no content. See §10.2.
    Pututu(ctx context.Context, payload string) error
    // Balance reports remaining prepaid credit. ErrUnsupported if the
    // provider has no such concept.
    Balance(ctx context.Context) (Balance, error)
}
```

- **Registry by config:** the `[carrier]` block in `wasi.toml` selects a provider by name with a provider-specific sub-table — necessary because providers disagree on device identity (Hologram: device id; Soracom: IMSI/SIM id) and that difference must not leak into core config or `pututu` code.
- **Optional capabilities degrade, they don't panic:** `ErrUnsupported` from `Balance` hides the UI balance panel and silences the low-credit alert.
- **`carriertest` conformance suite:** rings once, surfaces transport errors, honours context cancellation, returns `ErrUnsupported` rather than zero values. A new provider is one package plus a passing conformance run.
- **Providers:** `hologram` implemented for v1; `fake` (records calls in memory) for the e2e stack, removing any live-account dependency from M3; `soracom` left unimplemented but shaping the interface from day one.

## 11. Shared service contracts

### 11.1 `strip`

`POST /strip` · request `{text, format_flowed}` · response `{body, trimmed, removed_bytes}`. Python 3-slim + `talon.quotations`. Shared bearer token; private network only; no persistence; **request bodies never logged** (I-1, verified by V-11's log grep). Golden corpus at `services/strip/testdata/replies/*.eml`.

### 11.2 `cell` (specified now, built in v2)

`POST /resolve` · request `{cells:[{mcc,mnc,lac,cid,rssi}]}` · response `{lat, lon, radius_m, place, rank}`. SQLite survives here because it earns its place: millions of read-only OpenCelliD rows behind an index on `(mcc,mnc,lac,cellid)` is a real database workload in a shared service holding no device state — dropping SQLite from Wasi was a judgement about Wasi's data, not a ban on the tool. Reverse geocoding is local (design-spec §4.2 forbids third-party lookups for a named minor): GeoNames `cities1000` (~10 MB) nearest-populated-place, which produces exactly the "near Ferndale, MI (within about 3 km)" rendering and nothing more precise — which is the point.

## 12. TLS and trust

### 12.1 Two listeners, two trust models

| Listener | Serves | Certificate | Why |
|---|---|---|---|
| Device sync | `POST /sync` only | Leaf signed by the **private CA** | §12.2 |
| Guardian UI | Web UI | **Let's Encrypt** leaf, obtained via the operator's DNS-01 proxy | Browsers trust it; no warnings, no manual CA installs on family phones |

One binary, two ports (or two hostnames); nothing shared but the process.

### 12.2 Device path: private dual-CA pinning

Public PKI is a liability on the device path, not a convenience: if the device trusted a public root, its security would hinge entirely on the modem's TLS stack doing strict hostname verification — historically the weak joint of embedded AT-command stacks — because anyone can obtain a public cert for *some* name. With a private CA, even a broken hostname check trusts only certificates the operator signed. Public certs also rotate every ~90 days; each rotation is a moving part between a pocket device and home.

- Generate **CA-A** and **CA-B**, each ~20-year validity, keys stored **offline** and separately (paper/QR in a safe, or a hardware token, is genuinely adequate at this scale). The firmware trust store ships containing **both** roots. CA-B is escrow: if CA-A's key is lost or compromised, cut over without touching devices. Cost: one extra certificate in flash.
- Server leaf: ~2-year validity, signed offline. Renewal is a once-per-two-years ceremony of taking a key out of a safe.
- **Firmware requirements imposed by this spec:** (1) on TLS trust failure, the device MUST show a distinct, visible "can't reach home" state — a silently dead device is design-spec §8's failure in hardware form; (2) whatever the firmware-update path turns out to be, the trust store MUST be updatable through it. These are the only two requirements the server spec places on firmware beyond the wire contract.

### 12.3 Expiry alarm

Wasi checks its own device-listener certificate at startup and daily. At < 45 days remaining: persistent guardian-UI banner, plus the optional SMTP guardian copy (§7.5) if configured. **Not** an INBOX notice letter — certificate operations are operator noise, not family record, and the child's inbox is not an ops channel.

### 12.4 Related fixed points

Device bearer token: stored as SHA-256 hash (§3). Guardian passwords: argon2id (§9.2). Shared services: bearer token from secrets, private network only.

## 13. Configuration reference (`wasi.toml`)

| Key | Default | Notes |
|---|---|---|
| `owner.name` | — | Child's name; used only for generated subjects (§6.2) |
| `mail.imap`, `mail.smtp` | — | Endpoints; credentials via secrets |
| `mail.held_folder` | `"Held"` | |
| `device.token_hash` | — | SHA-256 hex of the bearer token |
| `sync.max_letter_chars` | `500` | Graphemes; both directions |
| `sync.budget_bytes` | `2048` | Steady-state response target |
| `sync.resync_window` | `200` | Letters delivered on empty cursor |
| `sync.interval_s` | `21600` | Pushed to device |
| `ayllu.max_contacts` | `24` | Includes tombstones; UI-enforced |
| `kipu.retention_days` | `14` | Whole day-file deletion |
| `backup.dir` / `backup.retain_days` | `/backups` / `7` | Excludes `kipu/` |
| `pututu.coalesce_min` | `15` | Minutes |
| `guardian.listen` / `device.listen` | — | Two listeners (§12.1) |
| `guardian.copy_addresses` | `[]` | The I-3 exception (§7.5) |
| `[carrier]` | — | Provider name + provider sub-table (§10.4) |
| `device_config.rat`, `device_config.cover`, timeouts | — | Passed through in `config` (§4.3) |

Hot-reloaded on change. Secrets are never here (§3). Changing `max_letter_chars` affects re-derivation on the next window resync only; letters already on the device are unaffected (dedup is by letter id, §5.2).

## 14. Ops

`/healthz` (process up) and `/readyz` (IMAP reachable, config parsed) on the guardian listener. JSON `slog` to stdout — **no letter bodies or subjects at any level** (I-1); log letter *ids* where correlation is needed. `wasi backup` per §3; `wasi useradd` per §9; `wasi contacts` for stopped-server list maintenance. Container: distroless, static, non-root, read-only rootfs except `/data`.

## 15. Verification

All runnable with zero hardware and no Fastmail account: `deploy/compose.dev.yml` brings up Wasi, `strip`, and **maddy** as the local IMAP/SMTP fixture, driven by `tools/chaskisim`. (Where maddy's behaviour diverges from Fastmail — notably spam foldering, which maddy does not do — the affected test injects the condition directly, and §5.1's setup step is the real-world control.)

| # | Asserts |
|---|---|
| V-1 | Compose lands in maddy for the right address with a valid `Message-ID`; once with `--subject` (verbatim), once without (generated) |
| V-2 | Empty-cursor sync returns at most `resync_window` letters, newest span, each body ≤ `max_letter_chars` graphemes |
| V-3 | **Header injection:** subject containing `\r\nBcc: attacker@evil.test` → no injected header, subject flattened; non-ASCII → RFC 2047; 500-char subject → capped at 100 graphemes |
| V-4 | Inbound with quoted tail and `Re: Re: Fwd: =?utf-8?B?…?=` subject → stripped, `trimmed: true`, subject decoded and collapsed; 5000-grapheme body → `truncated: true`, mailbox copy intact |
| V-5 | Non-allowlisted sender → no bounce, message in `Held`, nothing on the device |
| V-6 | **Deactivation:** two letters from `c_07`, remove contact, third letter, full window resync → first two render with the name, third in `Held`, outbound to `c_07` → `rejected_inactive`, tombstone arrives `active: false`; re-add same address → same id, one person in the archive |
| V-7 | Add, remove, re-point a contact → three notice letters in INBOX from `c_sys`; `c_sys` unwritable; notice text contains no address and none of the three internal words |
| V-8 | Outbound carries `Message-ID` and **no** `In-Reply-To`/`References`/`Re:` — fails loudly if anyone "fixes" A.1 |
| V-9 | Replayed identical sync → no duplicate SMTP send, identical acks (including replayed *rejections*), byte-identical bodies **under unchanged config** |
| V-10 | Kill `strip` mid-run → letters deliver `degraded: true`; restart → next sync re-derives cleanly, no stale copy |
| V-11 | **Storage invariant:** full e2e flow, then no file under `/data` or `/backups`, and no line of captured Wasi/strip container logs, contains a substring of any letter |
| V-12 | **Crash consistency:** kill Wasi mid-write to `ayllu.toml` and mid-sync → file parses, no contact lost, replayed sync doesn't double-send |
| V-13 | Two letters 30 s apart with `fake` carrier → exactly one pututu; none if the device synced after arrival; a 10-round `more` drain counts as one sync |
| V-14 | Vocabulary boundary: `pututu`/`ayllu`/`kipu` absent from templates and outgoing-mail paths |
| V-15 | **Reconciliation:** stranger's mail arrives while Wasi is down → on restart it is in `Held` before any sync delivers; nothing skipped |
| V-16 | Message placed in the Spam folder → moved to `Held` within one backstop interval |
| V-17 | Kill Wasi between ayllu write and notice APPEND → on restart the notice is sent exactly once (`pending_notices` flush) |
| V-18 | **Release flows:** stranger's Held letter → add-then-release delivers it, add notice fires, pututu rings; deactivated contact's letter → reactivate-release-deactivate delivers it and future mail still goes to `Held` |
| V-19 | Password change → previously issued session cookie rejected (epoch bump); five failed logins → backoff observed |
| V-20 | **Counter heal:** restore `state.json` from an older copy → next SMS ignored by simulated device; after one sync, counter jumps forward and the following SMS is accepted. Replayed/forged SMS ignored silently |
| V-21 | UIDVALIDITY reset mid-life → next sync behaves as window resync; device dedup leaves no duplicates |
| V-22 | `go test ./…` — strip golden corpus, `carriertest` conformance, grapheme-counting cases (emoji, combining accents at cap boundaries) |
| V-23 | **Permanent send failure (A.11):** a 5xx from submission → ack `rejected_undeliverable`, terminal and idempotent on replay (no re-send); a 4xx → letter stays unacked and is retried |
| V-24 | **Removed-contact outage arrival (A.13):** a deactivated contact's letter seeded into INBOX above the device cursor → the next concrete sync holds it before delivering, and it lands in Held, never on the device; a window resync does not trigger the hold |

With the `fake` carrier, M3 is fully testable offline; the only hardware-blocked check is one live Hologram SMS at bring-up.

## 16. Milestones

| | Contents |
|---|---|
| **M0** | Monorepo skeleton, TOML config + atomic file state, `chaskisim` skeleton, compose stack with maddy |
| **M1** | Core letter path: `/sync` (full schema §4), ayllu resolution + tombstones, IDLE + reconciliation + spam backstop, strip + fallback, subjects both directions, `Held`, **kipu day-files + retention** |
| **M2** | Guardian web UI, sessions/throttling, `notice` package + `pending_notices`, release flows, change log |
| **M3** | Pututu: carrier registry, `hologram`, signed counter tokens + wire reconciliation |
| *v2* | Kipu tiers + display, `cell` service, location rendering, tier-2 toggle (reusing `notice` unchanged) |

## Appendix A — Decision log

Recorded so reversals read as deliberate, with enough reasoning to re-litigate honestly if the facts change.

**A.1 · Threading dropped — supersedes design-spec §3.5.** Outbound carries `Message-ID` only. §3.5 argued threading headers are "trivial now, unfixable later," which is true; the reversal is deliberate anyway: the product is passing notes, not conducting email. There is no reply concept on the device — only "write to this person" — and the humans hold the context, as paper mail always has. The graduated account is a flat archive of notes that looks like the device did; threading begins the day they use a normal client. Guarded by V-8. Interaction: dropping threading is what makes a human-authored subject worth its screen row (A.2).

**A.2 · Subjects are child-authored with generated fallback — extends design-spec §3.5.** Generation was assumed to be the only source; it is the default, not the ceiling. With threading gone, the subject is the only context handle left, which raises the value of a real one.

**A.3 · 24-contact cap.** The device is an intimate object, not an address book; two dozen is generous for its purpose. The cap is also what lets the full ayllu payload (with tombstones) fit a single budget-exempt response with no chunking protocol. Raising it is a config-and-firmware conversation, not a redesign — but it should be a conversation.

**A.4 · Truncation, not sender notification.** Long inbound letters are cut at 500 graphemes with a visible flag; the archive keeps the full text. A "your letter was truncated" reply to the sender was considered and rejected for v1: Wasi is not the receiving MTA (Fastmail has fully accepted the message before Wasi sees it, so a true reject is impossible by construction), an auto-reply would come *from the child's address*, it would breach I-3, and it needs loop-protection machinery. The 24-person list is guardian-curated; "keep it postcard-length" is an onboarding norm, stated in the docs guardians share with new contacts. Revisit as a designed v2 feature if reality shows relatives blowing the cap constantly.

**A.5 · Plain JSON, no compression, no CBOR.** Gzip on ~2 KB payloads saves a few hundred KB/month against a per-MB bill and costs a firmware dependency of uncertain availability in the modem's HTTP stack. CBOR likewise unearned. Revisit only on billing evidence.

**A.6 · Kipu day-files in v1 (M1), storage-wise; kipu *product* in v2.** The wire accepts tier-1 health from day one so firmware never changes shape when v2 lands; storing it from M1 is what makes the prototype debuggable, and the day-file log is less code than a database while having a strictly better erasure story (whole-file `rm` vs. freelist/WAL residue). Backups exclude it so retention means what it says.

**A.7 · Private dual-CA for the device path; Let's Encrypt for humans.** Public PKI on the device path would stake everything on an embedded TLS stack's hostname verification and inherit 90-day rotation churn; a private CA bounds trust to operator-signed certs even under a weak client stack. Two 20-year offline CAs (one escrow) in the trust store is the no-brick insurance. The operator's LE DNS-01 proxy serves the guardian UI, where browser trust is what matters.

**A.8 · Counter-based pututu authentication.** Time-windowed tokens need a trustworthy clock the device lacks; a monotonic HMAC'd counter needs only flash. The backup-restore rollback it introduces is healed over the sync wire (§10.3) rather than by operational care, keeping "restore is `cp` back" true.

**A.9 · No database in Wasi — reaffirmed with kipu included.** ~24 contacts, a cursor, some counters, and an append-only health log never earned SQLite, migrations, or a `store` package. What the database would have provided — atomicity, crash consistency, idempotent replay — is specified explicitly instead (§3, §4.7, §7.6) and each clause carries its own test. The `cell` service keeps SQLite because its workload genuinely is one (§11.2).

**A.10 · Pagination moved to the device — supersedes design-spec §3.6's "server paginates" (and this spec's own first draft).** Server-side pagination existed to make the 2.7"-vs-4.2" panel question a config change rather than a firmware release — a rationale that assumed screen capacity is a per-device constant. Adjustable font sizes (an accessibility requirement: not every reader has sharp vision) make capacity a per-reader, runtime property, so server-computed page breaks would go stale on every font change and could only be repaired by re-download. Reflow belongs where the font lives. The original goal survives in stronger form: panel size, font size, and subject-row layout are now all firmware-local decisions that never touch the server, its config, or the wire. `chars_per_page` is removed everywhere; `max_letter_chars` remains the sole server-owned size, because it governs content, not layout.
**A.11 · A permanent SMTP rejection is a terminal ack; a transient one is not.** §4.7's original four statuses had no way to express "this address is permanently dead," so a 5xx left the letter unacked and the device retried it every sync forever, showing the child "on the road" for something that could never land. `rejected_undeliverable` closes that: a 5xx reply is terminal (device stops, surfaces "couldn't send — ask your guardians"), while a 4xx or an unreachable server stays transient and is retried, because losing a letter is the one outcome §4.7 refuses to buy and only a *permanent* refusal justifies giving up. The device needs no new logic — every ack was already terminal — so this is a wire addition, not a firmware behaviour change. (Test V-23.)

**A.12 · Reconciliation resolves against the full table; only arrival is active-only.** §7.2's table lists both "filing" and "reconciliation" as active-only, but an active-only *reconciliation* would sweep a deactivated contact's already-delivered history into Held on its next pass, which §7.1 and V-6 forbid. The binding reading: the active-only decision is made once, at *arrival* (the IDLE path); reconciliation is a catch-up that resolves against the full table. The two must never be collapsed into one resolution call — a regression test asserts it by counting `Resolve` vs `ResolveActive` calls, not by behaviour.

**A.13 · Reconciliation also holds a removed contact's undelivered mail, bounded by the delivery cursor.** A.12 left a gap: if Wasi is down when a *removed* contact sends, the arrival path never sees it, and full-table reconciliation resolves the tombstone and delivers it as history — so a contact removed for a reason could reach the child by timing a letter to an outage. The fix holds inactive-resolving INBOX mail whose UID is above the **device's own delivery cursor** (undelivered), leaving history (at or below) untouched. The cursor is device-authoritative, so a rolled-back `state.json` cannot fool it, and the rule is applied only for a concrete cursor — a window resync falls back to strangers-only, so a factory reset never sweeps history. The accepted cost is a conservative over-hold: a letter that arrived while its sender was active but that the child had not yet synced to receive, when the sender is then deactivated, is held for guardian review rather than delivered. That is the safe direction. (Tests: filing unit suite, and a sync-driven e2e that needs no IDLE.)

**A.14 · Address change retains the old address for read-time resolution.** See §7.2. `past_addresses` is consulted by `Resolve` and never by `ResolveActive`, so history renders while new mail from a lost account still goes to Held. Existing sequential contact ids and existing addresses are never rewritten — a contact id is the permanent rendering key for every letter that person ever sent (§7.1).

**A.15 · The system contact ships to the device as a tombstone.** Notice letters arrive with `contact_id: c_sys`, which is otherwise absent from the ayllu payload, leaving the device unable to name the sender. Shipping `c_sys` with `active: false` reuses the tombstone semantics exactly — render the name, hide from the compose picker — which is precisely what §7.4 requires of a contact that cannot be written to, with no new wire field.

**A.16 · The system contact is named "Wasi", not "Home".** This is the sender a child sees on every announcement about their contact list, and — since notice letters graduate — on those records for as long as the archive lasts. "Home" is loaded for a reader who moves often, may not have a settled one, or is being asked to call somewhere home that they don't; "Wasi" asserts nothing about the reader's life. It is legal where the internal vocabulary is not: Wasi is one of design-spec §0's three public, cold-readable names, not a §0.1 identifier. Pinned by test because the cost of changing a name in a permanent archive only grows.

**A.17 · Releasing a Held letter from an already-active contact announces nothing.** See §8. A notice describing a change that did not happen corrupts the family record worse than a missing one, because I-4's promise is that the list changed exactly when a letter says it did.

**A.18 · The change log, not `pending_notices`, is what makes I-4 literally true.** See §7.6. `pending_notices` is a fast path; the append-only change log is the durable record, and deterministic notice `Message-ID`s let startup reconcile the two. This is also the mechanism by which a stopped-server `wasi contacts` mutation gets announced by the next `serve`.
