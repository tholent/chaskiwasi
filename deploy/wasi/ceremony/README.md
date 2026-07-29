# The device-path TLS ceremony (§12.2)

This is the production counterpart to `../gen-certs.sh`, which mints
throwaway certificates for the dev/e2e fixture in seconds and is never
appropriate here. Read `../../README.md`'s TLS section first for the
one-paragraph summary of *why* the device path uses a private dual-CA
instead of public PKI; this file is the *how*.

**What this buys, stated plainly:** a device's trust store, once flashed,
never has to change for the life of the deployment — even if the CA that
signed its leaf is later lost or compromised — because every device already
trusts both roots from day one. That guarantee is worth nothing if the two
roots aren't actually independent, so the discipline below matters more
than the scripts do.

## What a script can and cannot do here

`gen-ca.sh`, `gen-csr.sh`, and `sign-leaf.sh` get the cryptography right —
correct key sizes, correct extensions (`CA:TRUE` on the roots and nothing
else, `serverAuth` on the leaf, the right SANs), correct validity periods.
None of them can make the *ceremony* safe, because the ceremony's safety is
almost entirely physical and procedural, not cryptographic:

- that CA-A and CA-B are actually generated independently, not as two
  invocations of the same script in the same sitting on the same machine —
  the second CA is only escrow if whatever could go wrong with the first
  one's generation (a compromised live-boot image, a coerced or careless
  operator) didn't also happen to the second;
- that the offline machine really is offline for the whole session;
- that the private keys go to physically separate secure storage (two
  safes, not one folder) once minted, and that nobody keeps a "just in
  case" copy on a laptop;
- that access to each key requires more than one person, if that matters to
  your household or organization.

No script can verify any of that from the inside. Treat the three scripts
here as the last ten minutes of a ceremony that is mostly about where you
run them and who is in the room, not the ceremony itself.

## Roles

| | Runs where | Touches |
|---|---|---|
| `gen-ca.sh` | **Offline.** Air-gapped machine or wiped live-boot media. | Mints one CA's key + cert. Run twice, independently, for CA-A and CA-B. |
| `gen-csr.sh` | **Online.** The Wasi host itself (or wherever `device.tls_key` will live). | Mints the leaf's own private key, which never leaves this environment. |
| `sign-leaf.sh` | **Offline.** Same machine (or an equivalent) as whichever CA is signing. | Reads the CA private key. Never sees the leaf's private key — only its public CSR. |

Only two kinds of file ever cross the online/offline boundary, in either
direction: certificates and certificate signing requests. Neither contains
secret material. If a step in your process would move a `.key` file across
that boundary, stop — something has gone wrong.

## 1. Mint CA-A and CA-B (once, ever, per fleet)

On an offline machine:

```sh
./gen-ca.sh a ./ca-a
```

Move `ca-a/ca.key` to offline storage (paper/QR in a safe, or a hardware
token). Keep `ca-a/ca.crt` — it's public and goes into every device's
firmware trust store (see §3 below).

Then, **on a separate occasion, ideally a separate offline environment**:

```sh
./gen-ca.sh b ./ca-b
```

Move `ca-b/ca.key` to a *different* secure location than CA-A's. `ca-b.crt`
is public, same as CA-A's.

Both `ca.crt` files are ~20-year (7300-day) roots. You will not run this
step again unless you are replacing an escrow key that was itself lost or
consumed by a cutover (§4).

## 2. Provision the firmware trust store

Concatenate both public certs into the bundle your firmware build (or
provisioning tool) embeds as the device's trust anchors:

```sh
cat ca-a/ca.crt ca-b/ca.crt > device-trust-bundle.crt
```

Every device that ships with this bundle trusts a leaf signed by *either*
CA, from day one, whether or not CA-B is ever actually used to sign
anything. That's the entire mechanism §12.2 relies on for a cutover that
touches zero devices.

## 3. Issue (or renew) the server leaf — the once-every-two-years step

On the Wasi host, online:

```sh
./gen-csr.sh /config/tls wasi.your-family.example
```

(replace the hostname with whatever the device actually dials — a dynamic
DNS name, a static IP's reverse name, whatever's stable). This writes
`device.key` (stays here, forever) and `device.csr` + `device.ext` (carry
both to the offline machine — by USB stick, by hand, however your process
moves non-secret files across that boundary).

On the offline machine holding CA-A:

```sh
./sign-leaf.sh ./ca-a /path/to/device.csr /path/to/device.ext ./out
```

Carry `out/device.crt` back to the server and point `wasi.toml`'s
`device.tls_cert` / `device.tls_key` at it and the key from step one. `wasi
serve`'s own certificate-expiry check (§12.3) watches this leaf from here
on and warns the guardian UI, and optionally sends the SMTP guardian copy,
starting 45 days before it needs doing again.

## 4. Cutover: CA-A is lost or compromised

Because every device already trusts CA-B (step 2 happened once, at
provisioning, and never again), cutover touches **only future leaf-signing
operations** — no device firmware update, no re-flash, no truck roll:

1. Stop signing new or renewed leaves with CA-A. Treat its key as
   compromised: if you still have it, destroy it; if it's simply lost,
   there's nothing further to do to it.
2. Every subsequent run of step 3 uses `./ca-b` instead of `./ca-a`.
3. Mint a fresh CA-C at your convenience to restore two-CA escrow for the
   *next* possible loss — CA-B is now carrying the whole fleet alone, the
   same way CA-A used to.
4. Rebuild the firmware trust bundle for any device provisioned *after*
   this point as `ca-b.crt` + `ca-c.crt`. Devices already in the field need
   nothing: they already trust CA-B.

This is the whole reason §12.2 specifies two CAs instead of one long-lived
one: the failure mode that would otherwise require touching every device in
the field is designed away before it happens, not handled after.

## Not this ceremony: the guardian listener

The guardian web UI's certificate is a Let's Encrypt leaf obtained through
the operator's own DNS-01 proxy (§12.1) — ordinary public PKI, because
guardians connect from ordinary browsers that need to trust it without a
manual CA install. It rotates on Let's Encrypt's ~90-day cadence, has
nothing to do with CA-A/CA-B, and none of the scripts or keys in this
directory are relevant to it. Conflating the two trust models — for
instance, "simplifying" by putting the device listener behind the same
public certificate — throws away the entire reason §12.2 exists: a public
cert's security rests on the *client's* hostname verification being
correct, and an embedded modem's TLS stack is exactly the client that
history says gets that wrong.
