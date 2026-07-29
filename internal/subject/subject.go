// Package subject implements the two subject paths in the spec, which share
// nothing but the grapheme cap because they run in opposite trust directions:
//
//   - Inbound (§5.4): a real subject written by a relative in an ordinary mail
//     client, arriving RFC 2047-encoded and prefix-encrusted. Normalisation
//     decodes and tidies it for the device's list view; subjects are never
//     generated for inbound mail.
//   - Outbound (§6.2): the one place child-supplied text enters an email
//     header (V-3). Sanitisation must make header injection impossible, not
//     merely unlikely, because the input crosses a trust boundary straight
//     into a raw SMTP header.
package subject

import (
	"fmt"
	"mime"
	"regexp"
	"strings"
	"unicode"

	"github.com/tholent/chaskiwasi/internal/graphemes"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// MaxGraphemes is the wire cap on subjects, both directions (§4.6, §5.4, §6.2).
const MaxGraphemes = protocol.MaxSubjectGraphemes

// generatedMaxGraphemes caps a body-derived generated subject (§6.2). Tighter
// than MaxGraphemes so a generated subject reads like a subject line, not a
// truncated paragraph.
const generatedMaxGraphemes = 40

// replyPrefixPattern matches one leading reply/forward prefix, in English and
// the localised variants named in §5.4: Re, Fwd, Fw, and the common
// non-English equivalents (RE, AW, SV, VS, RES, Antw, TR, WG). Matching is
// case-insensitive and applied repeatedly by NormalizeInbound, because real
// clients produce chains like "Re: AW: Fwd: camping?".
var replyPrefixPattern = regexp.MustCompile(`(?i)^\s*(re|fwd|fw|aw|sv|vs|res|antw|tr|wg)\s*:\s*`)

// maxPrefixStrips bounds the strip loop. replyPrefixPattern always consumes at
// least one non-empty match, so this is a defensive ceiling, not a realistic
// count of chained prefixes.
const maxPrefixStrips = 50

// NormalizeInbound turns a raw, wire-format Subject header into the text the
// device shows in its list view (§5.4). Pipeline: RFC 2047 decode -> strip
// repeated reply/forward prefixes -> collapse whitespace -> cap at
// MaxGraphemes. Inbound subjects are real and are never generated; an empty
// result here just means the sender left the subject blank.
func NormalizeInbound(raw string) string {
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(raw)
	if err != nil {
		// A malformed encoded-word must not fail derivation (§5.2 never
		// silently drops mail): fall back to the raw header text and keep
		// going through the same cleanup as any other subject.
		decoded = raw
	}

	for i := 0; i < maxPrefixStrips; i++ {
		stripped := replyPrefixPattern.ReplaceAllString(decoded, "")
		if stripped == decoded {
			break
		}
		decoded = stripped
	}

	decoded = collapseWhitespace(decoded)
	out, _ := graphemes.Truncate(decoded, MaxGraphemes)
	return out
}

// Sanitize makes s safe to place in an outbound Subject header (§6.2, V-3):
// every control character (CR, LF, tab, and the rest of the C0/C1 set) is
// replaced with a space so a multi-line payload collapses to one flattened
// line rather than jamming words together, repeated whitespace is collapsed,
// and the result is capped at MaxGraphemes graphemes. This step alone is
// enough to make header injection impossible: there is no code path from here
// to EncodeHeader that can reintroduce a raw CR or LF.
//
// Sanitize operates on decoded text. Call EncodeHeader on the result
// immediately before writing it into a raw header.
func Sanitize(raw string) string {
	return sanitizeCapped(raw, MaxGraphemes)
}

func sanitizeCapped(raw string, capGraphemes int) string {
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, raw)
	stripped = collapseWhitespace(stripped)
	out, _ := graphemes.Truncate(stripped, capGraphemes)
	return out
}

// EncodeHeader RFC 2047-encodes s for literal use immediately after
// "Subject: " in a raw header (§6.2), so accented names and emoji survive
// SMTP's 7-bit transport. ASCII-clean input is returned unchanged. Callers
// MUST pass output that has already been through Sanitize: EncodeHeader does
// not strip control characters, it only makes non-ASCII text transportable.
func EncodeHeader(s string) string {
	return mime.QEncoding.Encode("utf-8", s)
}

// Generate produces a fallback outbound subject when the device sends none
// (§6.2): the first few words of body, capped at ~40 graphemes, falling back
// to "Letter from {ownerName}" when the body yields nothing usable (e.g.
// empty, or entirely whitespace/control characters). ownerName is passed in
// rather than read from config, keeping this package free of a config import.
func Generate(body, ownerName string) string {
	firstWords := strings.Join(strings.Fields(body), " ")
	generated := sanitizeCapped(firstWords, generatedMaxGraphemes)
	if generated != "" {
		return generated
	}
	return sanitizeCapped(fmt.Sprintf("Letter from %s", ownerName), MaxGraphemes)
}

// Outbound produces the final Subject header value for one outbound letter
// (§6.2): rawSubject verbatim (after Sanitize) if the device supplied one,
// otherwise Generate's fallback, then RFC 2047-encoded and ready to write
// after "Subject: " in the raw header.
func Outbound(rawSubject, body, ownerName string) string {
	plain := Sanitize(rawSubject)
	if plain == "" {
		plain = Generate(body, ownerName)
	}
	return EncodeHeader(plain)
}

// collapseWhitespace replaces every run of Unicode whitespace (which
// includes CR and LF) with a single space and trims the ends, turning
// multi-line input into one flattened line. Shared by both subject paths.
func collapseWhitespace(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}
