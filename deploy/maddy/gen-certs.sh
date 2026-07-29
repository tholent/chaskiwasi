#!/bin/sh
# gen-certs.sh — mints a self-signed cert for the maddy fixture, once.
#
# Idempotent: if the pair already exists (from a previous `make up`), it's
# left alone rather than rotated, so the volume survives restarts without
# every other container needing to re-trust a new cert mid-session.
#
# Runs as its own one-shot compose service (see ../compose.dev.yml) because
# the maddy image itself has no openssl (it's a static Go binary on a bare
# Alpine base) — see deploy/README.md for why this is a separate container.
set -eu

OUT_DIR="${1:-/tls}"
HOSTNAME_VALUE="${MADDY_HOSTNAME:-mail.chaski.test}"

if [ -f "$OUT_DIR/fullchain.pem" ] && [ -f "$OUT_DIR/privkey.pem" ]; then
    echo "gen-certs: $OUT_DIR already has a cert, leaving it alone"
    exit 0
fi

apk add --no-cache openssl >/dev/null

openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$OUT_DIR/privkey.pem" \
    -out "$OUT_DIR/fullchain.pem" \
    -days 3650 \
    -subj "/CN=$HOSTNAME_VALUE" \
    -addext "subjectAltName=DNS:$HOSTNAME_VALUE,DNS:maddy,DNS:localhost"

echo "gen-certs: wrote $OUT_DIR/fullchain.pem and $OUT_DIR/privkey.pem"
