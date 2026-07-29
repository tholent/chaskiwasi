package syncsvc

import (
	"encoding/base64"
	"encoding/binary"
)

// cursorBytes is the fixed decoded length of a cursor: uidvalidity and the
// last delivered UID, each a big-endian uint32 (§4.4).
const cursorBytes = 8

// encodeCursor mints the opaque cursor the device stores verbatim and echoes
// (§4.4). It encodes (uidvalidity, last-delivered UID) and nothing else.
//
// The encoding is deliberately unauthenticated. A cursor is not a capability:
// the bearer token (§4.1) is what decides whether a caller reaches this
// endpoint at all, and the only thing a forged cursor can do is change which
// of its own letters the device is sent — which a device can already ask for
// by sending "" (§4.4). Signing it would buy nothing and add a key to rotate.
func encodeCursor(uidValidity, lastUID uint32) string {
	var b [cursorBytes]byte
	binary.BigEndian.PutUint32(b[0:4], uidValidity)
	binary.BigEndian.PutUint32(b[4:8], lastUID)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// decodeCursor parses a cursor minted by encodeCursor. ok is false for "" and
// for anything that does not decode to exactly cursorBytes.
//
// A malformed cursor is reported, never rejected: the caller treats it exactly
// as "" — a window resync — for the same reason §4.4 gives for a stale
// uidvalidity. Any other choice would invent a firmware path for a state the
// device cannot repair on its own, and a bounded resync repairs it in one
// round-trip with the device's own dedup (§4.5) absorbing the overlap.
func decodeCursor(s string) (uidValidity, lastUID uint32, ok bool) {
	if s == "" {
		return 0, 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != cursorBytes {
		return 0, 0, false
	}
	return binary.BigEndian.Uint32(raw[0:4]), binary.BigEndian.Uint32(raw[4:8]), true
}
