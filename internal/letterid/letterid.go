// Package letterid derives the stable letter identity carried on the wire as
// Letter.ID (§4.5).
//
// The id must survive UIDVALIDITY resets, re-derivation, and window resyncs —
// none of which are stable identifiers on their own — while never exposing the
// raw Message-ID header to the device, because that header can leak the
// sender's mail-client hostname (I-2 is about addresses; this is the same
// instinct applied to a header that is not an address but still identifies the
// sender's infrastructure).
package letterid

import (
	"crypto/sha256"
	"encoding/hex"
)

// prefix is prepended to every letter id on the wire (§4.5).
const prefix = "l-"

// hexLen is the number of lowercase hex characters kept from the SHA-256 sum.
const hexLen = 10

// FromMessageID derives a letter id from the raw Message-ID header (§4.5).
// raw is the full header value, e.g. "<abc123@example.com>", byte-for-byte as
// it appeared on the wire; it is never echoed back, only hashed.
//
// The id is stable across UIDVALIDITY resets and window resyncs because it
// depends only on a header the sending MTA fixed once, not on any mailbox
// position.
func FromMessageID(raw string) string {
	return hashToID([]byte(raw))
}

// FromFallback derives a letter id when Message-ID is absent — rare, but
// legal per RFC 5322, and derivation must not drop such a letter (§5.2 never
// silently drops mail). The hash input is From + Date + the first 1 KB of the
// raw body: fields likely to be present and, taken together, unlikely to
// collide across distinct letters even without a Message-ID to anchor on.
//
// This is a separate exported function, rather than a fallback hidden inside
// FromMessageID, so callers make the "no Message-ID" case an explicit,
// visible branch rather than an implicit one.
func FromFallback(from, date string, rawBodyPrefix []byte) string {
	const bodyCap = 1024
	if len(rawBodyPrefix) > bodyCap {
		rawBodyPrefix = rawBodyPrefix[:bodyCap]
	}

	h := sha256.New()
	h.Write([]byte(from))
	h.Write([]byte{0}) // field separator: prevents "ab"+"c" colliding with "a"+"bc"
	h.Write([]byte(date))
	h.Write([]byte{0})
	h.Write(rawBodyPrefix)
	return prefix + hex.EncodeToString(h.Sum(nil))[:hexLen]
}

func hashToID(data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + hex.EncodeToString(sum[:])[:hexLen]
}
