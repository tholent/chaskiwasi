# Vendored front-end assets

`htmx.min.js` is committed to this repository on purpose. A CDN link would put
an external dependency — and an external observer — on a page that must work on
a home LAN with the WAN unplugged (§9.1: "no build step"; §9.2: the guardian
listener binds for LAN/VPN reach).

| | |
|---|---|
| Package | [htmx](https://htmx.org/) |
| Version | 2.0.10 |
| Source | `https://unpkg.com/htmx.org@2.0.10/dist/htmx.min.js` |
| SHA-256 | `71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de` |
| License | Zero-Clause BSD (0BSD) — permits redistribution without attribution; recorded here anyway |

To update: download the new file, record the version and SHA-256 above, and
re-run `go test ./internal/web/...`. There is no build step and nothing to
regenerate.

`app.css` is hand-written and has no upstream.
