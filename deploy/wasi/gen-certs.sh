#!/bin/sh
# gen-certs.sh — mints Wasi's two TLS identities for the dev stack, once.
#
# §12.1 gives Wasi two listeners with two different trust models, and this
# script models both rather than flattening them into one self-signed pair:
#
#   device.crt   — a leaf signed by a local CA, standing in for the private
#                  dual-CA setup of §12.2. The device trusts the CA, not the
#                  leaf, so chaskisim (and the e2e suite) can pin ca.crt and
#                  exercise the real trust path instead of skipping
#                  verification. A test that runs with verification disabled
#                  cannot catch a trust bug, which is the one thing §12.2 is
#                  built to prevent.
#   guardian.crt — self-signed. In production this is a Let's Encrypt leaf
#                  (§12.1) because browsers must trust it without a manual CA
#                  install; there is no dev equivalent worth simulating, and
#                  a browser warning on a localhost fixture is not a finding.
#
# Only CA-A is minted here. The escrow CA-B of §12.2 is a production
# ceremony — an offline key in a safe — and has nothing to stand in for in a
# container that regenerates on `make down -v`. For the real ceremony (two
# independent offline CAs, an offline-signed ~2-year leaf, and the cutover
# procedure), see ceremony/README.md alongside this script — do not adapt
# this file for production use; it is a dev fixture on purpose (single
# online CA, RSA-2048, no offline step at all).
#
# Idempotent: an existing set is left alone rather than rotated, so restarts
# don't invalidate a CA the simulator has already pinned.
set -eu

OUT_DIR="${1:-/config/tls}"

if [ -f "$OUT_DIR/device.crt" ] && [ -f "$OUT_DIR/guardian.crt" ]; then
    echo "gen-certs: $OUT_DIR already provisioned, leaving it alone"
    exit 0
fi

apk add --no-cache openssl >/dev/null

# CA-A (dev stand-in). 20-year validity per §12.2 — the point of the long
# life is that a device in a pocket never needs its trust store touched.
openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$OUT_DIR/ca.key" \
    -out "$OUT_DIR/ca.crt" \
    -days 7300 \
    -subj "/CN=Chaskiwasi Dev CA-A" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"

# Device leaf, signed by CA-A. SANs cover both the compose service name and
# localhost, so the suite can reach it from inside the network or from the
# host's published port.
openssl req -newkey rsa:2048 -nodes \
    -keyout "$OUT_DIR/device.key" \
    -out "$OUT_DIR/device.csr" \
    -subj "/CN=wasi"

openssl x509 -req \
    -in "$OUT_DIR/device.csr" \
    -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
    -out "$OUT_DIR/device.crt" \
    -days 730 \
    -extfile /dev/stdin <<EOF
subjectAltName=DNS:wasi,DNS:localhost,IP:127.0.0.1
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
EOF

rm -f "$OUT_DIR/device.csr"

# Guardian leaf, self-signed — see the header note.
openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$OUT_DIR/guardian.key" \
    -out "$OUT_DIR/guardian.crt" \
    -days 730 \
    -subj "/CN=wasi-guardian" \
    -addext "subjectAltName=DNS:wasi,DNS:localhost,IP:127.0.0.1"

# The Wasi container runs as 65532 (§14) and reads these; keys stay
# owner-readable only.
chmod 0640 "$OUT_DIR"/*.key
chmod 0644 "$OUT_DIR"/*.crt
chown -R 65532:65532 "$OUT_DIR"

echo "gen-certs: wrote device (CA-signed), guardian (self-signed) and ca.crt to $OUT_DIR"
