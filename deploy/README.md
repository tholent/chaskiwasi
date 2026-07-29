# deploy/ — the local stack

Everything needed to run Wasi, `strip`, and a local IMAP/SMTP fixture
(`maddy`) with **zero hardware and no Fastmail account** (wasi-server-plan
§15). Driven by the Makefile at the repo root:

```
make up     # docker compose -f deploy/compose.dev.yml up -d --build
make down   # ... down -v
make e2e    # up, then go test -tags e2e ./test/e2e/...
```

## What's in here

| Path | What |
|---|---|
| `compose.dev.yml` | The stack: `maddy-certs` → `maddy-init` → `maddy`, `strip`, `wasi` |
| `maddy/maddy.conf` | maddy's own stock Docker config, unmodified except a header comment — see "Why maddy" below |
| `maddy/gen-certs.sh` | One-shot: mints a self-signed cert for maddy into a shared volume |
| `maddy/init.sh` | One-shot: creates the test account and the `Held` mailbox |
| `wasi.example.toml` | A fully commented `wasi.toml`, wired to this stack's service names, covering every key in §13's table |
| `Dockerfile` | Wasi's own image: distroless, static, non-root (§14) |
| `wasi/gen-certs.sh` | Dev-only: mints throwaway TLS material for this stack. **Not** the production ceremony. |
| `wasi/ceremony/` | The production TLS ceremony: two offline root CAs, an offline-signed server leaf, and the cutover procedure (§12.2). See "TLS in production" below. |

`services/strip/Dockerfile` builds the strip service; it lives under
`services/strip/`, not here, since that's where its source and golden
corpus are.

## Credentials in this stack

Everything below is a **dev-only default**, overridable via a `.env` file
next to `compose.dev.yml` or exported in your shell before `make up`. None of
it is fit for anything but this fixture.

| Variable | Default | Used by |
|---|---|---|
| `MADDY_TEST_USER` | `kid@chaski.test` | maddy account = `mail.address` in wasi.toml |
| `MADDY_TEST_PASSWORD` | `devpassword123` | maddy account password = `WASI_IMAP_PASSWORD` |
| `WASI_SERVICE_TOKEN` | `dev-shared-service-token` | shared bearer token, both `strip` and `wasi` |
| `WASI_COOKIE_SIGNING_KEY` | `dev-cookie-signing-key-not-for-prod` | guardian session cookies (§9.2) |

## Why maddy, and where it diverges from Fastmail

maddy (`foxcpp/maddy`) is a real, RFC-compliant IMAP4rev2 + SMTP server, not
a mock — the point of using it is that `internal/mailbox`'s actual client
code (IDLE, MOVE, APPEND, UIDVALIDITY, SMTP submission over TLS with AUTH)
gets exercised against wire protocol, not a stand-in. It is also a genuinely
different codebase from Fastmail's, so two things are known to differ and
the affected tests must inject the condition directly rather than relying on
maddy's own behavior to produce it:

1. **No spam foldering.** maddy never classifies anything as spam — there is
   no equivalent to Fastmail's provider-side filter for the backstop in §5.1
   to catch. The `Junk` folder exists (maddy creates it for every account by
   default) but nothing ever puts a message there on its own. **V-16
   requires injecting a message into `Junk` directly** — see below.
2. **IDLE and MOVE semantics.** maddy's IDLE/MOVE implementation is its own
   (`imapserver`/`imapmemserver` machinery upstream, or the SQLite-backed
   storage in this image), not Dovecot's or Fastmail's. Behaviour that
   depends on a specific server's IDLE re-sync quirks or MOVE's exact
   UID-reassignment timing should be treated as maddy-specific if it's not
   also asserted against the wire-contract text in the spec.

The real-world control for (1) is §5.1's own setup step: disabling
Fastmail's spam filtering on the mailbox in production. That's a deployment
doc concern, not something this fixture can stand in for.

## Injecting a message directly into a folder

The e2e suite (and anything exercising reconciliation, the spam backstop, or
Held-folder scenarios) needs to put a message into a specific folder without
going through SMTP delivery — most directly because maddy never files
anything into `Junk` on its own (V-16), but the same mechanism is the
general tool for any test that wants to seed exact IMAP state.

maddy's own CLI does this in one shot, reading the raw RFC 5322 message from
stdin:

```sh
docker compose -f deploy/compose.dev.yml exec -T maddy \
  maddy imap-msgs add kid@chaski.test Junk < message.eml
```

Prints the new UID on success. Swap `Junk` for `Held`, `INBOX`, or any other
folder name. `--date` sets INTERNALDATE if a test needs a specific one;
`--flag` adds IMAP flags (e.g. `-f '\Seen'`). Full options:
`docker compose -f deploy/compose.dev.yml exec maddy maddy imap-msgs add --help`.

The same account/mailbox CLI is what `maddy/init.sh` uses to provision the
account in the first place (`maddy creds create`, `maddy imap-acct create`,
`maddy imap-mboxes create`) — useful if a test needs a second mailbox or a
second account that isn't part of the default fixture.

## TLS in this fixture

Both maddy's IMAP (993) and submission (465) listeners use implicit TLS with
a certificate `gen-certs.sh` mints fresh on first `make up` (RSA-2048,
self-signed, 10-year validity, SAN covering `maddy`/`mail.chaski.test`/
`localhost`). This is deliberate: it means `internal/mailbox`'s real
`TLSImplicit` code path — the actual `tls.Client(...).HandshakeContext`
production uses — gets exercised, not a plaintext shortcut. Any client
connecting from outside the fixture (a manual `go run` from the host, say)
needs `TLSConfig: &tls.Config{InsecureSkipVerify: true}` — never do this
against a real mail provider; it's only correct here because the fixture's
private network is the actual trust boundary, not the certificate.

## The `wasi` container

`make up` brings up a `wasi` container running `serve`: two TLS listeners
(device sync and guardian UI), config hot-reload, the kipu retention
sweeper, and the filing/IDLE loop. It waits on `maddy` and `strip` health
checks and on a one-shot `wasi-certs` service that mints its dev
certificates. A healthy start logs `device listener starting` and `guardian
listener starting`; the device sync endpoint is on host port 18443 and the
guardian UI on 18444.

## What was verified live against this stack (Wave 1C)

- `maddy` accepts an IMAP login over implicit TLS, an `APPEND`, and a
  `FETCH` for the appended message, using `internal/mailbox.IMAPMailbox`
  directly (not just a raw client).
- `maddy` accepts an SMTP submission (implicit TLS + AUTH PLAIN) using
  `internal/mailbox.SMTPSubmitter`.
- `maddy imap-msgs add <user> Junk < message.eml` files a message into a
  named folder outside of IMAP/SMTP delivery, confirming the V-16 injection
  path above.
- The `strip` image builds, starts non-root with no logged errors, serves
  `/healthz` unauthenticated, rejects `/strip` without (and with the wrong)
  bearer token, and correctly strips a quoted reply when authenticated — and
  none of that traffic appears in `docker compose logs strip`.
- The `wasi` image builds, runs as `nonroot:nonroot` under `--read-only`,
  and `serve` starts both listeners; a letter injected into `maddy` syncs to
  a simulated device and a composed reply is delivered back through `maddy`.

The full end-to-end flow across all services is now exercised by the `e2e`
suite (`test/e2e/`, run with `make e2e`), which brings this stack up and
drives the §15 V-table against it.

## `wasi backup` (§3, §14)

```sh
wasi backup [-data /data] [-config /config/wasi.toml] [-dir DIR] [-retain-days N] [-skip-retention]
```

Copies `-data` — **excluding `kipu/`** — into a new timestamped directory
under the backup destination (`backup.dir` / `backup.retain_days` from
`wasi.toml` by default; `-dir` / `-retain-days` override them), then deletes
whole backup directories older than the retention window. The kipu
exclusion is not configurable anywhere — no flag, no config key — on
purpose: kipu's day-files are retained for exactly `kipu.retention_days` so
that whole-file deletion is the erasure story (design-spec §3.7); a backup
that captured them would silently extend that window by `backup.retain_days`
on top, putting deleted day-files back within reach of exactly the case
§3.7 exists to guard against — an accumulated location history in a
contested household.

**Safe to run against a live server.** Unlike `wasi contacts` below, backup
does not require (or check for) a stopped server — routine backups of a
running deployment are the normal case. What it actually does is a plain
recursive copy against files a running `serve` may be concurrently
rewriting, and the guarantee that gives you is worth stating precisely:

- Every individual file it copies — `ayllu.toml`, `state.json`,
  `guardians.toml`, `ayllu-log.jsonl` — is always a complete, non-torn
  generation of that file, because `internal/atomicfile`'s write discipline
  (temp file, fsync, rename, fsync directory) never leaves a reader able to
  observe a half-written one.
- Across files, it is a "fuzzy" snapshot, not one consistent instant. For
  `state.json` that's fine by design: §10.3 exists specifically so a
  restored `state.json`, arbitrarily stale relative to the mailbox, heals
  itself over the next sync rather than silently breaking the doorbell. For
  the `ayllu.toml`/`ayllu-log.jsonl` pair, the exposure is narrow — the
  width of one contact-list mutation, not the width of the whole backup —
  because the store holds its own lock across writing both. A backup taken
  while nobody happens to be mid-edit on the contact list (the overwhelming
  majority of the time, since this list changes rarely) sees the pair
  consistently. A backup taken with the server stopped is the only way to
  get a formally guaranteed-consistent snapshot of that pair; this command
  does not claim more than the above for a live one.

**Restore is `cp` back, plus one sync — deliberately no more machinery than
that (§3).** Copy a chosen backup directory's contents over `/data` and
start `wasi serve`. The pututu counter in `state.json` self-heals over the
sync wire even from an arbitrarily old backup (§10.3); nothing else needs
operator repair, for the reasons above.

**Ownership note for the `/backups` volume:** exactly like `/data` (see
"Image invariants" below), a Docker named volume mounted at `/backups` with
nothing staged there in the image comes out root-owned, and `wasi backup`
(running as uid 65532) cannot write its first backup into it. The image
stages an empty, correctly-owned `/backups` for the documented default path;
pointing `backup.dir` at a different volume or a host bind mount means
arranging that ownership yourself, the same as for any other host-side mount.

## `wasi contacts` (§14)

```sh
wasi contacts list   [-data /data] [-config /config/wasi.toml]
wasi contacts add        -name NAME -address ADDR [-actor NAME] [-pinned] [-order N] [-portrait ID]
wasi contacts deactivate <contact-id> [-actor NAME]
wasi contacts reactivate <contact-id> [-actor NAME]
wasi contacts readdress  <contact-id> -address ADDR [-actor NAME]
```

Contact-list maintenance for when hand-editing `ayllu.toml` isn't the right
tool — recovering from a mistake without hand-editing TOML at all.
`list` shows every contact, tombstones marked `TOMBSTONE` rather than left
to be inferred from a blank status column. `reactivate` undoes an accidental
`deactivate`; `readdress` fixes a mistyped or changed address without ever
touching the file by hand.

**The server must be stopped, and this is enforced, not just documented.**
`ayllu.toml` is hand-editable — and therefore CLI-editable — only while Wasi
is stopped (§3): two writers to one file is exactly the failure mode
implementation-plan.md's F-8 found as a real bug in `guardians.toml` (`wasi
useradd` wrote the file while the running server held a stale copy, and the
server's next write deleted the account the CLI had just added). F-8 was
fixed by making the guardian store re-read the file on change; `wasi
contacts` deliberately does **not** copy that fix, because a contact-list
mutation carries an announcement obligation (I-4) that a guardian account
doesn't, and re-reading the file on the server side would do nothing about
the running server's notice machinery having no way to learn a CLI-made
change happened at all. So `contacts` takes an exclusive, non-blocking
advisory lock (`flock(2)`) on `/data/.wasi.lock` and refuses outright if
`wasi serve` already holds it — `serve` takes and holds the same lock for
its entire run. The lock is released automatically by the kernel if the
holding process dies for any reason, including `SIGKILL`, so there is no
stale-lock state to clean up by hand.

**No mutation sends a notice letter itself.** A stopped-server CLI has no
live IMAP connection, so it cannot APPEND one — that would be a promise
this command can't keep. What every mutation *does* guarantee is the durable
record I-4 depends on: `ayllu.toml` is rewritten atomically and
`ayllu-log.jsonl` gets a new line before the command exits successfully
(§7.6). The next `wasi serve` start already reads that log and announces
anything with no matching notice yet in INBOX (`notice.Service.Reconcile`,
built for exactly this kind of crash recovery — a durable change with no
notice yet looks identical whether the previous process crashed mid-announce
or the change came from this CLI). Every mutation prints as much, so this is
never mistaken for I-4's one forbidden outcome: a change with no
announcement, ever.

## TLS in production (§12.2)

The device listener and the guardian listener use two unrelated trust
models — do not conflate them:

- **Device listener:** a private dual-CA pin, `deploy/wasi/ceremony/` is the
  full procedure — two independent ~20-year offline root CAs (the firmware
  trust store ships both, so a lost or compromised CA-A can be cut over
  without touching a single device already in the field), signing a ~2-year
  server leaf in an offline ceremony. `deploy/wasi/gen-certs.sh` is the dev
  stand-in for this and is **not** adaptable for production use — see its
  own header comment.
- **Guardian listener:** an ordinary Let's Encrypt leaf via your DNS-01
  proxy of choice, because guardians connect from ordinary browsers that
  need to trust it without installing a private CA. It rotates on Let's
  Encrypt's own ~90-day cadence and has nothing to do with the device path.
  This repo does not script that half — it's exactly what any other web
  service behind Let's Encrypt does, with `guardian.tls_cert` /
  `guardian.tls_key` in `wasi.toml` pointed at whatever your ACME client
  writes.

## Image invariants (§14)

`deploy/Dockerfile` builds a **distroless, statically linked, non-root**
image with a **read-only root filesystem except `/data` and `/backups`**.
Verified live against a built image: the binary is a static ELF with no
dynamic interpreter; the container has no shell (`/bin/sh` does not exist);
the process runs as `nonroot:nonroot` (uid/gid 65532); `docker run
--read-only` succeeds and both `/data` and `/backups` are writable while
everything else is not.

**The `/data` and `/backups` ownership fix.** Docker seeds a *fresh* named
volume from whatever the image has at that mount path, ownership included.
With nothing staged there, a fresh volume comes out root-owned and the
non-root process gets "permission denied" on its very first write — not a
theoretical concern, it's exactly what an empty `/backups` volume does
without the fix below. The Dockerfile stages empty `/data` and `/backups`
directories owned by uid/gid 65532 in the build stage and `COPY
--chown=65532:65532`s them into the final image *before* the matching
`VOLUME` declaration, specifically so a fresh named volume inherits correct
ownership. Do not remove either `COPY --chown` step or reorder it after the
`VOLUME` line — distroless has no shell, so there is no `RUN
mkdir/chown` fallback available if this regresses.

## Operational facts learned the hard way

- **`wasi useradd` works against a running server.** `guardians.FileStore`
  re-reads `guardians.toml` when its mtime or size changes, so a guardian
  account created by `useradd` while `serve` is up is usable immediately —
  no restart needed (implementation-plan.md's F-8). This is the one
  CLI-vs-running-server case in this codebase where re-reading was the
  right fix; `wasi contacts` above explains why the identical-looking
  problem for `ayllu.toml` was fixed the other way instead.
- **The guardian UI's login form field is `guardian`**, not `username` or
  `name` — `r.PostFormValue("guardian")`. Scripting a login needs that exact
  field name.
- **Its CSRF hidden field is `csrf_token`.** Every rendered page carries a
  fresh one (`internal/web/csrf.go`); it's bound to the signed-in guardian
  and their session epoch, so a token survives across pages in one session
  but not across a password change.
- **Every unsafe (state-changing) request needs an `Origin` header whose
  host matches the request's own `Host`.** The guardian UI's CSRF defense
  checks `Sec-Fetch-Site` first and falls back to `Origin`; a request
  carrying neither is refused outright, including the login POST itself —
  this UI is browser-only by construction. A `curl` script against it needs
  `-H "Origin: https://<guardian-listen-host>:<port>"` matching the `Host`
  header on the same request, or every POST (including sign-in) gets a 403
  regardless of credentials.
