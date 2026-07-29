#!/bin/sh
# deploy/wasi/ceremony/gen-csr.sh — generates the device listener's leaf
# private key and a signing request for it (§12.2).
#
# Run this ONLINE, on (or for) the Wasi host — never on the offline CA
# machine. The whole point of a CSR is that the private key it produces
# never has to leave here: only device.csr and device.ext, both public,
# travel to the offline signing ceremony (sign-leaf.sh). Keep device.key on
# the server (or wherever `wasi serve` will read device.tls_key from) and
# nowhere else.
set -eu

OUT_DIR="${1:-/config/tls}"
HOSTNAME="${2:-wasi}"

mkdir -p "$OUT_DIR"
if [ -f "$OUT_DIR/device.key" ]; then
    echo "gen-csr: $OUT_DIR/device.key already exists, refusing to overwrite" >&2
    echo "(renewing onto a fresh key is fine, but move or remove the old" >&2
    echo "key/csr first so that's a deliberate choice, not an accident)" >&2
    exit 1
fi

openssl req -newkey rsa:2048 -nodes \
    -keyout "$OUT_DIR/device.key" \
    -out "$OUT_DIR/device.csr" \
    -subj "/CN=$HOSTNAME"

# The extensions the signing step needs travel as a plain-text sidecar
# rather than inside the CSR itself: whether `openssl x509 -req` carries a
# CSR's own extensions forward differs across OpenSSL versions, and this
# file is exactly as non-secret as the CSR, so nothing is gained by relying
# on that instead of just handing both files to the signing ceremony. This
# mirrors how gen-certs.sh (the dev fixture) supplies extensions at signing
# time via -extfile, for the same portability reason.
cat > "$OUT_DIR/device.ext" <<EOF
subjectAltName=DNS:$HOSTNAME,DNS:localhost
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
EOF

chmod 0600 "$OUT_DIR/device.key"
chmod 0644 "$OUT_DIR/device.csr" "$OUT_DIR/device.ext"

echo "gen-csr: wrote $OUT_DIR/device.key — keep it here, send it nowhere."
echo "Carry $OUT_DIR/device.csr and $OUT_DIR/device.ext (neither is secret)"
echo "to the offline signing ceremony (sign-leaf.sh)."
