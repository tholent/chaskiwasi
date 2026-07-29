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

## `wasi`'s container exits immediately, and that's expected right now

`cmd/wasi`'s `serve` subcommand isn't implemented yet (that's Wave 2B/3B in
`specs/implementation-plan.md`); today it prints "not implemented yet" and
exits 1. The `wasi` service is still fully wired up in `compose.dev.yml` —
config mounted, secrets set, `depends_on` health checks on `maddy` and
`strip` — so nothing here needs to change when `serve` lands. Until then,
`make up` bringing up a `wasi` container that immediately stops is normal,
not a broken fixture.

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
  and exits with the expected "not implemented yet" message.

Not yet verified live (nothing to verify yet): the full `docker compose up`
of all five services together end-to-end, since `wasi serve` doesn't run a
server yet. Wave 2B/3B should re-run the checks above as part of standing up
`test/e2e`.
