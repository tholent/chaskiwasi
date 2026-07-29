# Deploying Wasi

This is the operator's guide: setting up one Wasi container for one Chaski
device, once. It assumes you're comfortable with a shell, TLS, and DNS. For
the guardian web UI itself, see `docs/guardians.md`.

Wasi holds no letter content of its own — the Fastmail mailbox is the
canonical store (wasi-server-plan §1, I-1). Getting the mailbox side of the
setup right matters more than anything in this document, which is why it
comes first.

## 1. The Fastmail mailbox

Create (or dedicate) one Fastmail mailbox for this device. It needs:

- An **app password** for IMAP/SMTP (not your Fastmail login password) —
  Settings → Password & Security → App Passwords in Fastmail's web UI. Scope
  it to Mail only.
- A **Held** folder, for mail from strangers and removed contacts to
  quarantine into (§5.1). Create it now; Wasi never creates folders itself.

### Disable the provider's spam filter — before anything else

This mailbox only ever receives mail from an allowlist Wasi itself enforces
(§3.1 of the design spec): every sender either resolves to an active contact
or gets quarantined to Held. A provider-side spam filter sitting in front of
that has no good mail left to protect and only one way left to fail: silently
dropping a letter from family into a Spam folder nobody — not the device, not
a guardian — is told to look at. Disabling it is not a nice-to-have; **until
it is done, the provider's filter may already be silently discarding good
mail**, invisibly to both the device and the guardian UI (wasi-server-plan
§5.1).

In Fastmail: Settings → Junk Mail → set spam handling to **"Don't move
messages"** (or the equivalent "off" setting for this mailbox) rather than
filing to Spam.

Wasi still checks the Spam folder as a backstop — at startup, at every sync,
and at least every 15 minutes — and quarantines anything found there to Held
rather than trusting the filter to have been turned off correctly (this is
what covers the gap between "you did the setup step" and "you're sure you
did it right"). But the backstop is a net under a net: the filter should
already be off.

## 2. Secrets — never in `wasi.toml`

`wasi.toml` is meant to be safe to keep in the family's own git repository,
so nothing that authenticates anything lives there (wasi-server-plan §3).
Every secret comes from an environment variable, or a mounted file if you'd
rather not put secret material in `docker run -e` / your process supervisor's
config:

| Variable | Required | What it's for |
|---|---|---|
| `WASI_IMAP_PASSWORD` | yes | The Fastmail app password (§1 above). Used for both IMAP and SMTP submission — Wasi treats it as one mailbox credential. |
| `WASI_COOKIE_SIGNING_KEY` | yes | HMAC key for guardian session cookies and CSRF tokens (§9.2). Generate with `openssl rand -base64 32` or similar; treat it like a password. |
| `WASI_SERVICE_TOKEN` | yes | Shared bearer token authenticating to `strip` (and `cell`, in v2). Must match what `strip` is configured to expect. |
| `WASI_CARRIER_API_KEY` | only if `[carrier]` is configured | The SMS provider's API key (§10.4). |
| `WASI_PUTUTU_KEY` | only if `[carrier]` is configured | A **separate** HMAC key for doorbell tokens (§10.2). Wasi only ever stores a *hash* of the device bearer token, so it cannot reuse that for the doorbell MAC — this has to be its own secret. Startup fails loudly if a carrier is configured without it, rather than shipping a doorbell that silently never rings. |

Each variable also accepts a `_FILE` suffix (`WASI_IMAP_PASSWORD_FILE`, and
so on) pointing at a file to read the value from instead — the usual
Docker/Kubernetes secrets convention. A trailing newline in the file is
trimmed automatically, so a value produced by `echo` or an editor is fine.

Wasi refuses to start if a required secret is missing, and names the missing
variable in the error — never its value.

## 3. `wasi.toml`

`deploy/wasi.example.toml` is a fully commented reference covering every key
(wasi-server-plan §13); copy it, then edit the `[mail]`, `[services]`, and
`[carrier]` blocks for your deployment. It's bind-mounted **read-only** into
the container and hot-reloaded on change — editing it and saving is enough,
no restart needed, *except* for the two TLS key pairs and the two listen
addresses, which are read once at process start.

`ayllu.toml` (the contact list) lives alongside it in `/data` and is
hand-editable while Wasi is stopped, if you'd rather script an initial
contact list than click through the web UI once per person.

## 4. TLS: two listeners, two trust models

Wasi serves two separate HTTPS listeners from one process, sharing nothing
but the binary (§12.1):

| Listener | Serves | Certificate | Reachable from |
|---|---|---|---|
| Device sync | `POST /sync` only | Leaf signed by your **private CA** | Wherever the device's LTE-M connection reaches Wasi |
| Guardian UI | The web UI | A **publicly-trusted** leaf (Let's Encrypt) | Home network / VPN by default; see §6 below |

They are configured independently in `wasi.toml`
(`device.listen`/`device.tls_cert`/`device.tls_key` and the `guardian.*`
equivalents) and can be different ports or different hostnames.

### The device path: private dual-CA pinning

The device never validates a public certificate authority — it trusts only
certificates signed by a CA you generate and control (§12.2). This is
deliberate, not a shortcut: staking a pocket device's security on an embedded
modem's TLS stack doing strict hostname verification against the *entire
public CA ecosystem* is a much larger attack surface than trusting only what
you signed yourself, and a private CA sidesteps the 90-day rotation churn a
public leaf would otherwise impose on a device that's hard to reach.

The ceremony, done once (or once every couple of decades):

1. Generate two root CAs, **CA-A** and **CA-B**, each with a long validity
   (~20 years). Keep both private keys **offline** — a printed/QR'd key in a
   safe, or a hardware token, is genuinely adequate at this device count.
2. Ship **both** root certificates in the firmware's trust store. CA-B is
   pure escrow: if CA-A's key is ever lost or compromised, you cut over to
   CA-B without touching a single device in the field.
3. Sign a server leaf off CA-A (~2 year validity) and configure it as
   `device.tls_cert`/`device.tls_key`. Renewal is a deliberate, infrequent,
   offline operation — not something that runs on a timer.

`deploy/wasi/gen-certs.sh` is the **development** stand-in for this — it
mints a single short-lived dev CA and a signed leaf automatically on
container start, entirely inside the compose fixture, with no offline key
and no escrow CA. It exists to exercise the real trust *path* (the device
pins the CA and verifies a real chain) in tests and demos, not to model the
production ceremony above. Do not point a real device at anything that
script produced. See `deploy/README.md` for what it actually does.

**Firmware side of this bargain:** the device must show a clear "can't reach
home" state on a trust failure rather than failing silently, and its trust
store must be field-updatable through whatever the firmware-update path
turns out to be — both requirements the server spec places on firmware
(§12.2), not something this document can enforce.

### The guardian path: a certificate browsers already trust

The guardian UI needs an ordinary publicly-trusted leaf so a relative's
browser shows no warning and nobody has to install a CA on a phone. Wasi
does not speak ACME itself — there's no embedded Let's Encrypt client in this
codebase. Run your own ACME client (`certbot`, `lego`, your reverse proxy's
built-in one, whatever you already trust) against a domain you control, using
**DNS-01** validation so the guardian listener doesn't need to be reachable
from the public internet just to prove domain ownership, and point
`guardian.tls_cert`/`guardian.tls_key` at the resulting files.

Both listeners load their certificate once at process start — renewing
either one (the device leaf every ~2 years, the guardian leaf every ~90
days if you're using Let's Encrypt) requires restarting the `wasi` process
to pick up the new file. Wire your ACME client's renewal hook to restart the
container; don't rely on a hot reload that doesn't exist for this file.

### Certificate expiry alarm

Wasi checks the *device* listener's certificate at startup and daily, and
shows a persistent banner in the guardian UI once fewer than 45 days remain
(§12.3). This deliberately never becomes a letter in the child's inbox —
certificate operations are operator noise, not family record. If you've
configured `guardian.copy_addresses` (§5 below), the same warning also goes
out as an SMTP copy.

## 5. The carrier block — the SMS doorbell

The pututu doorbell is optional: an unconfigured `[carrier]` is a normal,
supported deployment state. Wasi still files and delivers mail correctly;
the device just waits for its next scheduled sync instead of being nudged
early. Skip this section for a first deployment and add it later if you
want the SMS ping.

```toml
[carrier]
name = "hologram"          # or "fake" for the dev/e2e stack; "soracom" unimplemented

[carrier.options]
device_id = 123456         # Hologram's device id — required
org_id = "your-org-id"     # optional; needed only for the credit-balance panel
```

The API key comes from `WASI_CARRIER_API_KEY` (§2 above), never from this
file. Provider identity (Hologram keys off a device id; a future Soracom
integration would key off IMSI) stays inside `[carrier.options]` by design,
so it never leaks into the core config schema (§10.4).

The doorbell payload is an opaque signed counter — no sender name, no
content, ever (§10.2) — so a carrier account being compromised or a text
being intercepted leaks nothing about who wrote to the child.

## 6. Guardian access

The guardian listener binds for LAN/VPN reach by default — it is not meant
to be a public web service. The recommended path for reaching it away from
home is a VPN (WireGuard is a reasonable default choice).

Exposing the guardian listener to the public internet is possible — nothing
in the code prevents it — and is **at your own risk**. If you do it anyway,
the login throttling described in `docs/guardians.md` (fixed delay on
failure, exponential backoff after five failures) is the minimum bar this
system provides, not a reason to consider it hardened. It was designed
against a family member with the wrong incentives, not against the open
internet (§9.2).

## 7. Running it

The included `deploy/Dockerfile` builds a distroless, static, non-root image
(§14). Mount `wasi.toml` and your TLS material read-only at `/config`, and a
persistent volume for `/data`; run with a read-only root filesystem except
for that one volume. `deploy/compose.dev.yml` shows every mount and
environment variable this needs for the local fixture — the same shape
applies to a real deployment, with `maddy` and its generated certs replaced
by your real Fastmail credentials and CA-signed device certificate.

### First run

1. Bring the container up. It listens on both TLS ports and starts filing
   mail immediately.
2. Create the first guardian account from the host, since there's no
   guardian account yet to sign in and create one from the UI:

   ```sh
   docker exec -it <container> wasi useradd <your-name>
   ```

   This prompts for a password (12 characters minimum) with echo off, or
   accepts `-password-file <path>` (`-` for stdin) for scripting. It works
   against an already-running server — no restart needed — and is also the
   password-reset path (`wasi useradd -reset <name>`) if a guardian is
   ever locked out. Nothing else in the system can reset another guardian's
   password; that boundary is deliberate (§9.2) — a guardian who could would
   be able to lock a co-parent out of the record of their own child's
   contact list.
3. Sign in on the guardian listener and add the first contacts from
   **Contacts**.

### Health checks

`/healthz` (process is up) and `/readyz` (config parsed, mailbox reachable)
are unauthenticated on the guardian listener, for a container orchestrator
or load balancer's own probing — never registered on the device listener.

### Backups

`wasi backup` copies `/data`, **excluding `kipu/`**, to a timestamped
directory under `backup.dir` (default `/backups`), and sweeps directories
older than `backup.retain_days` (default 7). It runs as the same non-root
user as the server, so if you point `backup.dir` at a volume of your own
rather than the default, give that volume the same ownership `/data` has.

The `kipu/` exclusion is not configurable, by design: including the health
telemetry in a backup would silently extend its retention window past what
`kipu.retention_days` promises — an accumulated location-adjacent history is
exactly what §3.7 keeps un-keepable in a contested household. Restoring is a
plain copy back into `/data` plus one device sync: the doorbell counter
self-heals over the wire (§10.3), so nothing else needs manual repair.

`wasi contacts` (`list`, `add`, `deactivate`, `reactivate`, `readdress`)
edits the contact list from the command line. It **refuses to run against a
live server** — two writers to `ayllu.toml` is the failure the file's
ownership rules exist to prevent — so stop the container first, or use the
guardian web UI while it is running. A change made this way is durable
immediately but its notice letter (the announcement every contact change
owes, §7.4) is sent by the *next* `wasi serve` startup, which reconciles the
change log against the mailbox; each command says so when it runs.
