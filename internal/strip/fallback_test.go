package strip

import (
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// corpusDir is the golden corpus shared with services/strip (task: "Golden
// corpus at services/strip/testdata/replies/*.eml"). One fixture set, two
// implementations — see the package doc and the per-case comments below for
// why their outputs are allowed, and expected, to diverge.
const corpusDir = "../../services/strip/testdata/replies"

// loadCorpusCase reads one .eml fixture and reports its raw text/plain body
// plus whether its Content-Type declared format=flowed — the same two
// things services/strip/test_striplib.py extracts from the same files.
func loadCorpusCase(t *testing.T, name string) (text string, formatFlowed bool) {
	t.Helper()
	f, err := os.Open(filepath.Join(corpusDir, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()

	msg, err := mail.ReadMessage(f)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", name, err)
	}
	ct := msg.Header.Get("Content-Type")
	return string(body), strings.Contains(ct, "format=flowed")
}

func TestFallback_GoldenCorpus(t *testing.T) {
	// Expected output under the FALLBACK rules specifically — not talon's
	// output (see services/strip/testdata/replies/*.expected.txt for that).
	// The fallback only recognises two patterns (dropLeadingQuoted,
	// cutAtSignature); every case here that has neither at the very start
	// or as an exact "-- " line passes through unchanged. That's not a bug
	// in the test: it's the documented gap between "the real service" and
	// "minimal rules so a down Python container never blocks a letter"
	// (§5.3), and it's exactly what Degraded exists to signal downstream.
	tests := []struct {
		name         string
		file         string
		wantTrimmed  bool
		wantContains []string // substrings that must survive in Body
	}{
		{
			// Trailing "On ... wrote:" + '>' block: not a *leading* quote,
			// so the fallback's dropLeadingQuoted rule never sees it — this
			// is the case talon.quotations handles and the fallback does
			// not.
			name:         "gmail attribution passes through untouched",
			file:         "gmail_attribution.eml",
			wantTrimmed:  false,
			wantContains: []string{"Tell me everything when you get back.", "We're going camping"},
		},
		{
			// "-----Original Message-----" isn't the "-- " signature
			// delimiter and isn't '>'-quoted, so neither fallback rule
			// matches; also untouched by design.
			name:         "outlook original-message block passes through untouched",
			file:         "outlook_original_message.eml",
			wantTrimmed:  false,
			wantContains: []string{"I wish I could have been there.", "-----Original Message-----"},
		},
		{
			// This IS what the fallback is built for: an exact "-- " line
			// near the end.
			name:         "signature delimiter is cut",
			file:         "signature_delimiter.eml",
			wantTrimmed:  true,
			wantContains: []string{"We'll be cheering for you from"},
		},
		{
			// Same shape as gmail_attribution: the nested '>'/'>>' quoting
			// is all trailing (reply text comes first), so
			// dropLeadingQuoted — which only looks at the start of the
			// message — never engages.
			name:         "nested quoting is trailing, not leading, so it passes through",
			file:         "nested_quoting.eml",
			wantTrimmed:  false,
			wantContains: []string{"Yes, pizza night is still on!", "Is pizza night still happening"},
		},
		{
			// format=flowed soft breaks must be rejoined with a space
			// before either rule runs, or dropLeadingQuoted would see
			// space-stuffed lines with a stray leading space intact.
			name:        "format=flowed is unwrapped even without a matching quote pattern",
			file:        "format_flowed.eml",
			wantTrimmed: false,
			wantContains: []string{
				"This is such a long letter about your week at camp, I loved reading every word of it.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, formatFlowed := loadCorpusCase(t, tt.file)
			result := Fallback(text, formatFlowed)

			if !result.Degraded {
				t.Errorf("Degraded = false, want true (Fallback always sets it)")
			}
			if result.Trimmed != tt.wantTrimmed {
				t.Errorf("Trimmed = %v, want %v (body: %q)", result.Trimmed, tt.wantTrimmed, result.Body)
			}
			for _, substr := range tt.wantContains {
				if !strings.Contains(result.Body, substr) {
					t.Errorf("Body missing %q\ngot: %q", substr, result.Body)
				}
			}
		})
	}
}

func TestFallback_SignatureCutRemovesTrailingContent(t *testing.T) {
	text, formatFlowed := loadCorpusCase(t, "signature_delimiter.eml")
	result := Fallback(text, formatFlowed)

	if strings.Contains(result.Body, "Uncle Theo") {
		t.Errorf("fallback did not cut the signature: %q", result.Body)
	}
	if strings.Contains(result.Body, "Sent from my phone") {
		t.Errorf("fallback did not cut the signature: %q", result.Body)
	}
}

func TestDropLeadingQuoted(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no quote, unchanged", "hello\nworld", "hello\nworld"},
		{
			"single leading quote block dropped",
			"> quoted line one\n> quoted line two\n\nreal reply",
			"real reply",
		},
		{
			"blank lines within the leading block are also dropped",
			"> one\n\n> two\nreal reply",
			"real reply",
		},
		{
			"trailing quote is not touched, only leading",
			"real reply\n> quoted",
			"real reply\n> quoted",
		},
		{"all quoted, nothing left", "> only quote here", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dropLeadingQuoted(tt.in); got != tt.want {
				t.Errorf("dropLeadingQuoted(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCutAtSignature(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no signature, unchanged", "hello\nworld", "hello\nworld"},
		{"cuts at exact delimiter", "hello\n-- \nsig line", "hello"},
		{"double dash without trailing space is not a match", "hello\n--\nnot a sig", "hello\n--\nnot a sig"},
		{"a line merely containing dashes is not a match", "hello -- world\nmore text", "hello -- world\nmore text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cutAtSignature(tt.in); got != tt.want {
				t.Errorf("cutAtSignature(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestUnwrapFlowed(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"soft break keeps the word-separating space",
			"one two three \nfour five six\n",
			"one two three four five six\n",
		},
		{
			"signature delimiter is never joined onward",
			"body line \n-- \nsig\n",
			"body line \n-- \nsig\n",
		},
		{
			"space-stuffed line loses its leading space",
			" >not a real quote marker, just stuffed\n",
			">not a real quote marker, just stuffed\n",
		},
		{
			"blank lines are preserved as paragraph breaks",
			"para one \nstill one\n\npara two\n",
			"para one still one\n\npara two\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unwrapFlowed(tt.in); got != tt.want {
				t.Errorf("unwrapFlowed(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFallback_TableAgainstAllCorpusCases is a compact sanity check that
// Fallback runs cleanly (no panics, always Degraded) across every fixture in
// the shared corpus, independent of the more detailed per-case assertions
// above. New fixtures added to services/strip's corpus are picked up here
// automatically.
func TestFallback_TableAgainstAllCorpusCases(t *testing.T) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".eml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no .eml fixtures found in %s", corpusDir)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			text, formatFlowed := loadCorpusCase(t, name)
			result := Fallback(text, formatFlowed)
			if !result.Degraded {
				t.Errorf("Degraded = false, want true")
			}
			_ = result.Body // no panics, that's the whole assertion here
		})
	}
}
