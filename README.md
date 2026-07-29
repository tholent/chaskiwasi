# chaskiwasi

A small, text-only letter device for a young person who moves often, so they
can stay in touch with family without a phone number, a monthly bill, or a
social feed. Letters travel over ordinary email; nobody in the family needs
an app.

## The three names

| Name | Refers to |
|---|---|
| **Chaskiwasi** | the project as a whole |
| **Chaski** | the child's handheld device (the runner) |
| **Wasi** | this repository — the per-device server (the house) |

A *chaski* was a relay runner on the Inca road system, handing off at a
*chaskiwasi* — a waystation where the next runner waited. Store-and-forward,
in 1450. See `specs/chaskiwasi-design-spec.md` §0 for the full naming
rationale, including why the internal words `pututu`, `ayllu`, and `kipu`
appear in this codebase but never in anything a guardian or child reads.

## Architecture

```
  [ Chaski ] --HTTPS (private CA)--> [ Wasi ] --IMAP/SMTP--> [ Fastmail ]
  [ Guardian browser ] --HTTPS (public CA)--> |
                                               |--> [ strip ]  quote-stripping
                                               '--> [ cell ]   cell → place (v2)
```

One Wasi container per Chaski device — the container *is* the device's
identity. Wasi exposes two TLS listeners: a device-sync endpoint
(`POST /sync`, private-CA pinned) and a guardian web UI (public-CA, meant for
a home network or VPN). The mailbox at Fastmail is the canonical store —
Wasi keeps no letter content of its own, deriving the device's view from
IMAP at read time. See `specs/wasi-server-plan.md` for the full contract.

## Running the dev stack

Zero hardware, no Fastmail account: `deploy/compose.dev.yml` brings up Wasi,
the `strip` quote-stripping service, and `maddy` as a local IMAP/SMTP
fixture.

```sh
make up      # docker compose -f deploy/compose.dev.yml up -d --build
make e2e     # up, then the full test/e2e suite
make down    # tear down
make check   # fmt, vet, build, test — the gate for every change
```

See `deploy/README.md` for what the fixture diverges from a real mail
provider on, and `tools/chaskisim` for a CLI that plays the device's side of
the sync protocol against a running server.

## Docs

| Audience | Where |
|---|---|
| Operator standing up a real deployment | `docs/deploying.md` |
| Guardian using the web UI | `docs/guardians.md` |
| The family, on what the device knows about the child | `docs/what-the-device-knows.md` |

## Specs

| File | Standing |
|---|---|
| `specs/wasi-server-plan.md` | Authoritative for the server. Supersedes the design spec wherever they conflict; Appendix A records each supersession. |
| `specs/chaskiwasi-design-spec.md` | Context: hardware, device UX, principles. |
| `specs/implementation-plan.md` | Build order, package ownership, and findings against the spec discovered during implementation. |

`CLAUDE.md` has the short version of the invariants and the vocabulary
boundary for anyone working in the code.
