package chaskisim

import "github.com/rivo/uniseg"

// Wrap breaks body into lines of at most width extended grapheme clusters
// each (§4.9's "Firmware requirement (wire contract): line-break on
// grapheme-cluster boundaries, never inside one"). This is the one place
// this simulator can demonstrate that requirement is satisfiable at all: the
// server ships a single text string and owns zero layout numbers (§4.9,
// A.10), so pagination and line breaking exist nowhere except in firmware —
// which this function stands in for.
//
// It is a demonstration algorithm, not a production one: real firmware may
// prefer breaking at whitespace, hyphenating, or accounting for proportional
// glyph widths. The one invariant this function is here to prove
// satisfiable, and the one every caller should test against, is narrower and
// non-negotiable: a cut NEVER lands inside a grapheme cluster, however wide
// that cluster's underlying bytes or code points are. An emoji ZWJ sequence,
// a flag made of two regional indicators, or a base character with a
// combining accent is treated as exactly one unit, the same as any plain
// ASCII letter.
//
// An explicit '\n' in body always starts a new line, independent of width,
// matching how a device would want to honour a letter-writer's own line
// breaks rather than silently collapsing them.
func Wrap(body string, width int) []string {
	if width <= 0 {
		width = 1
	}

	var lines []string
	var current []byte
	count := 0

	flush := func() {
		lines = append(lines, string(current))
		current = current[:0]
		count = 0
	}

	g := uniseg.NewGraphemes(body)
	for g.Next() {
		cluster := g.Str()
		if cluster == "\n" {
			flush()
			continue
		}
		if count >= width {
			flush()
		}
		current = append(current, cluster...)
		count++
	}
	lines = append(lines, string(current))

	return lines
}
