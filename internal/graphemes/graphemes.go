// Package graphemes counts and cuts text in extended grapheme clusters per
// Unicode UAX #29 — the unit a reader perceives as one character (§0).
//
// Every character cap in this system (letter bodies, subjects) is specified in
// graphemes, never bytes or runes: byte and rune counts silently disagree with
// what the e-ink panel renders the moment an emoji ZWJ sequence, a flag made of
// two regional indicators, or a combining accent appears. This package is the
// one place that distinction is allowed to matter; callers elsewhere just call
// Count and Truncate.
package graphemes

import "github.com/rivo/uniseg"

// Count returns the number of extended grapheme clusters in s.
func Count(s string) int {
	return uniseg.GraphemeClusterCount(s)
}

// Truncate returns the longest prefix of s that contains at most n grapheme
// clusters, cutting only at a cluster boundary, and reports whether it cut
// anything. n <= 0 always yields ("", s != "").
//
// Cutting inside a cluster is the failure mode this function exists to
// prevent: it would split an emoji ZWJ sequence or a base character from its
// combining accent, producing bytes that are invalid to render as the
// original grapheme on either side of the cut.
func Truncate(s string, n int) (string, bool) {
	if s == "" {
		return "", false
	}
	if n <= 0 {
		return "", true
	}

	g := uniseg.NewGraphemes(s)
	count := 0
	end := 0
	for g.Next() {
		count++
		if count > n {
			return s[:end], true
		}
		_, to := g.Positions()
		end = to
	}
	return s, false
}
