#!/bin/sh
# deploy/wasi/ceremony/gen-ca.sh — mints ONE device-path root CA (§12.2).
#
# Run this OFFLINE: on a machine with no network connection — a live-boot
# USB stick wiped afterward, or a dedicated air-gapped host kept for exactly
# this purpose. This script produces correct cryptographic material; it
# cannot enforce the physical and procedural discipline the ceremony
# actually depends on. Read ../README.md before running this for real —
# in particular:
#
#   - CA-A and CA-B must be generated independently — different sessions,
#     ideally different offline media, ideally not both witnessed by only
#     one person — so that whatever compromised the offline environment
#     once (a bad USB image, a coerced operator) does not automatically
#     compromise both roots. Escrow that shares a generation event with the
#     thing it's meant to survive isn't escrow.
#   - The resulting ca.key must move to offline storage (paper/QR in a
#     safe, or a hardware token) and never again touch a network-connected
#     host, this one included, once the ceremony is done.
#
# This script itself is exactly as safe as the environment it's run in and
# not one bit safer — that's the honest limit of what a script can do here.
set -eu

LABEL="${1:?usage: gen-ca.sh <label: a|b> [out-dir]}"
OUT_DIR="${2:-./ca-$LABEL}"
LABEL_UPPER=$(echo "$LABEL" | tr '[:lower:]' '[:upper:]')

mkdir -p "$OUT_DIR"
if [ -f "$OUT_DIR/ca.crt" ]; then
    echo "gen-ca: $OUT_DIR/ca.crt already exists, refusing to overwrite" >&2
    exit 1
fi

# RSA-4096: this key signs nothing but a leaf roughly once every two years,
# for up to twenty years, so slow keygen and slow signing cost nothing —
# there's no reason not to spend the margin on a root that outlives every
# device it will ever provision.
#
# pathlen:0 on basicConstraints: this CA may sign leaf (end-entity)
# certificates only, never another intermediate CA — there is no
# intermediate tier in §12.2's design, and the constraint makes that a fact
# a validator checks rather than an assumption every future reader of this
# script has to re-derive.
openssl req -x509 -newkey rsa:4096 -nodes \
    -keyout "$OUT_DIR/ca.key" \
    -out "$OUT_DIR/ca.crt" \
    -days 7300 \
    -subj "/CN=Chaskiwasi Device CA-$LABEL_UPPER" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"

chmod 0400 "$OUT_DIR/ca.key"
chmod 0444 "$OUT_DIR/ca.crt"

echo "gen-ca: wrote $OUT_DIR/ca.crt (public, ships in every device's firmware"
echo "trust store) and $OUT_DIR/ca.key (move this to offline storage now and"
echo "leave no copy on this machine's disk)."
