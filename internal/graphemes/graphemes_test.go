package graphemes

import "testing"

// Named test cases (V-22: grapheme counting must be correct at emoji ZWJ
// sequences, combining accents, and regional-indicator pairs, including
// exactly at the cap boundary). Built from explicit escapes rather than pasted
// literals so the exact code points are unambiguous in source.
var (
	// familyEmoji is man+ZWJ+woman+ZWJ+girl+ZWJ+boy: one grapheme cluster made
	// of seven code points joined by U+200D ZERO WIDTH JOINER.
	familyEmoji = "\U0001F468" + "‍" + "\U0001F469" + "‍" + "\U0001F467" + "‍" + "\U0001F466"
	// usFlag is a pair of regional indicator symbols ("U" "S"): one cluster.
	usFlag = "\U0001F1FA\U0001F1F8"
	// caFlag is a pair of regional indicator symbols ("C" "A"): one cluster.
	caFlag = "\U0001F1E8\U0001F1E6"
	// eAcute is "e" followed by U+0301 COMBINING ACUTE ACCENT: one cluster,
	// two code points — the case a naive rune count gets wrong.
	eAcute = "e" + "́"
)

// TestV22_BoundaryCases is the named regression for V-22: grapheme counting
// and truncation must agree with what the e-ink panel renders exactly at a
// cap boundary, for the cases most likely to diverge from a naive byte or
// rune count — emoji ZWJ sequences, regional-indicator flag pairs, and
// combining accents.
func TestV22_BoundaryCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"family emoji ZWJ sequence", familyEmoji},
		{"regional indicator flag pair", usFlag},
		{"combining accent", eAcute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := Count(c.in)
			if n != 1 {
				t.Fatalf("Count(%q) = %d, want 1 (one grapheme cluster)", c.in, n)
			}

			// Exactly at the cap: kept whole, not cut.
			got, cut := Truncate(c.in, 1)
			if got != c.in || cut {
				t.Errorf("Truncate at exact cap = (%q, %v), want (%q, false)", got, cut, c.in)
			}

			// One below the cap: the whole cluster must be dropped, never
			// split into invalid bytes.
			got, cut = Truncate(c.in, 0)
			if got != "" || !cut {
				t.Errorf("Truncate below cap = (%q, %v), want (\"\", true)", got, cut)
			}
		})
	}
}

func TestCount(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "hello", 5},
		{"family emoji ZWJ sequence", familyEmoji, 1},
		{"two flags", usFlag + caFlag, 2},
		{"combining accent", eAcute, 1},
		{"combining accent among ascii", "caf" + eAcute, 4},
		{"mixed", "hi " + familyEmoji + " " + usFlag, 6}, // h i space family space flag
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Count(c.in); got != c.want {
				t.Errorf("Count(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		n       int
		want    string
		wantCut bool
	}{
		{"empty string", "", 5, "", false},
		{"n<=0 on nonempty", "abc", 0, "", true},
		{"negative n", "abc", -1, "", true},
		{"under cap", "abc", 5, "abc", false},
		{"exact cap", "abc", 3, "abc", false},
		{"over cap ascii", "abcdef", 3, "abc", true},
		{
			// The family emoji is one cluster; a cap of 1 must keep it whole
			// rather than cutting inside the ZWJ sequence.
			"family emoji exactly at cap", familyEmoji, 1, familyEmoji, false,
		},
		{
			// A cap of 0 with a single-cluster string must not emit a partial
			// cluster: the whole cluster is dropped, not split.
			"family emoji cap zero", familyEmoji, 0, "", true,
		},
		{
			"two flags, cap keeps first flag whole", usFlag + caFlag, 1, usFlag, true,
		},
		{
			"two flags, cap covers both", usFlag + caFlag, 2, usFlag + caFlag, false,
		},
		{
			"combining accent kept whole at cap", "ab" + eAcute, 3, "ab" + eAcute, false,
		},
		{
			"combining accent cut before it forms", "ab" + eAcute + "cd", 2, "ab", true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, cut := Truncate(c.in, c.n)
			if got != c.want || cut != c.wantCut {
				t.Errorf("Truncate(%q, %d) = (%q, %v), want (%q, %v)", c.in, c.n, got, cut, c.want, c.wantCut)
			}
			if c.n > 0 && Count(got) > c.n {
				t.Errorf("Truncate(%q, %d) result %q has more than %d clusters", c.in, c.n, got, c.n)
			}
			// The result must never split a cluster: re-truncating at a huge
			// cap must be a no-op, i.e. got is itself grapheme-aligned.
			if reGot, reCut := Truncate(got, 1<<30); reGot != got || reCut {
				t.Errorf("Truncate result %q is not stable under re-truncation: got (%q, %v)", got, reGot, reCut)
			}
		})
	}
}
