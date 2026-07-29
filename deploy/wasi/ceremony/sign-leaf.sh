#!/bin/sh
# deploy/wasi/ceremony/sign-leaf.sh — signs a device-listener CSR against an
# offline CA, producing the ~2-year server leaf §12.2 describes. This is the
# "take a key out of a safe" ceremony, run roughly once every two years, or
# immediately if cutting over from CA-A to CA-B.
#
# Run this OFFLINE, on the same machine (or an equivalently offline one)
# that holds the CA private key from gen-ca.sh. Its two inputs — the CSR and
# the .ext sidecar from gen-csr.sh — contain no secret material, which is
# why they're the only things that need to cross the boundary onto this
# machine (by hand, on removable media). Its one output, the signed
# certificate, is equally non-secret and is the only thing that needs to
# cross back. At no point does the CA private key leave this machine —
# that is the entire point of the ceremony, and nothing below should make
# it more convenient to violate that.
set -eu

CA_DIR="${1:?usage: sign-leaf.sh <ca-dir> <csr-file> <ext-file> [out-dir] [days]}"
CSR="${2:?usage: sign-leaf.sh <ca-dir> <csr-file> <ext-file> [out-dir] [days]}"
EXT="${3:?usage: sign-leaf.sh <ca-dir> <csr-file> <ext-file> [out-dir] [days]}"
OUT_DIR="${4:-./leaf}"
DAYS="${5:-730}"

mkdir -p "$OUT_DIR"

# 730 days (~2 years) by default, per §12.2: short enough that a compromised
# leaf ages out on its own on a human timescale, long enough that renewal
# stays a rare, deliberate ceremony rather than routine ops traffic.
openssl x509 -req \
    -in "$CSR" \
    -CA "$CA_DIR/ca.crt" -CAkey "$CA_DIR/ca.key" -CAcreateserial \
    -out "$OUT_DIR/device.crt" \
    -days "$DAYS" \
    -extfile "$EXT"

chmod 0444 "$OUT_DIR/device.crt"

echo "sign-leaf: wrote $OUT_DIR/device.crt, valid $DAYS day(s)."
echo "Carry this file (only) back to the server and install it as"
echo "device.tls_cert in wasi.toml. It is not sensitive."
echo
echo "Sanity-check before leaving the offline machine:"
echo "  openssl x509 -in $OUT_DIR/device.crt -noout -dates -subject -ext subjectAltName"
