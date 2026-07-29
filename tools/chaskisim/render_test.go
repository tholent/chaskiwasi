package chaskisim

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

// family is a ZWJ (U+200D) emoji sequence — man, woman, girl, boy joined
// into one rendered glyph but four+ code points on the wire. accented is a
// base letter followed by a combining acute accent: two code points, one
// grapheme. Both are exactly the boundary cases §4.9 and V-22 care about.
const (
	family   = "\U0001F468‍\U0001F469‍\U0001F467‍\U0001F466"
	accented = "é"
)

func TestWrap_NeverCutsInsideAGraphemeCluster(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		width int
	}{
		{"family emoji at width 1", strings.Repeat(family, 5), 1},
		{"family emoji at width 2", strings.Repeat(family, 5), 2},
		{"combining accent at width 1", strings.Repeat(accented, 5), 1},
		{"combining accent at width 3", strings.Repeat(accented, 5), 3},
		{"mixed ascii and emoji", "hi " + family + " bye " + family, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := Wrap(tt.body, tt.width)
			rejoined := strings.Join(lines, "")

			// Every grapheme cluster that went in must come out whole and in
			// order: rejoining the lines must reproduce the same cluster
			// sequence as the input (none of these fixtures contain '\n',
			// the one character Wrap consumes rather than re-emits).
			wantClusters := clusters(tt.body)
			gotClusters := clusters(rejoined)
			if len(wantClusters) != len(gotClusters) {
				t.Fatalf("cluster count = %d, want %d (rejoined = %q)", len(gotClusters), len(wantClusters), rejoined)
			}
			for i := range wantClusters {
				if wantClusters[i] != gotClusters[i] {
					t.Fatalf("cluster %d = %q, want %q — a cut landed inside a grapheme cluster", i, gotClusters[i], wantClusters[i])
				}
			}

			// No line may itself contain more than width clusters.
			for _, line := range lines {
				if n := uniseg.GraphemeClusterCount(line); n > tt.width {
					t.Errorf("line %q has %d clusters, want <= %d", line, n, tt.width)
				}
			}
		})
	}
}

func TestWrap_HonoursExplicitNewlines(t *testing.T) {
	lines := Wrap("ab\ncd", 10)
	want := []string{"ab", "cd"}
	if len(lines) != len(want) {
		t.Fatalf("Wrap = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestWrap_EmptyBody(t *testing.T) {
	lines := Wrap("", 10)
	if len(lines) != 1 || lines[0] != "" {
		t.Errorf(`Wrap("", 10) = %q, want one empty line`, lines)
	}
}

func TestWrap_NonPositiveWidthTreatedAsOne(t *testing.T) {
	lines := Wrap("abc", 0)
	if len(lines) != 3 {
		t.Fatalf("Wrap width 0 = %q, want one cluster per line", lines)
	}
}

func clusters(s string) []string {
	var out []string
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		out = append(out, g.Str())
	}
	return out
}
