package subject

import (
	"strings"
	"testing"

	"github.com/tholent/chaskiwasi/internal/graphemes"
)

// TestV3_HeaderInjectionSanitisation exercises the three cases §15 names
// explicitly for V-3, end-to-end through Outbound rather than the individual
// helpers, since that's the code path a real outbound letter takes.
func TestV3_HeaderInjectionSanitisation(t *testing.T) {
	t.Run("CRLF header injection produces no injected header", func(t *testing.T) {
		got := Outbound("camping!\r\nBcc: attacker@evil.test", "irrelevant body", "Chris")
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("Outbound(%q) = %q retains a raw CR or LF", "camping!\r\nBcc: attacker@evil.test", got)
		}
	})

	t.Run("non-ASCII round-trips through RFC 2047", func(t *testing.T) {
		raw := "café \U0001F383"
		got := Outbound(raw, "irrelevant body", "Chris")
		if got == raw {
			t.Fatalf("Outbound(%q) = %q, want RFC 2047 encoding for non-ASCII", raw, got)
		}
		if decoded := NormalizeInbound(got); decoded != raw {
			t.Fatalf("round trip failed: %q -> %q -> %q", raw, got, decoded)
		}
	})

	t.Run("500-character subject capped at 100 graphemes", func(t *testing.T) {
		got := Outbound(strings.Repeat("a", 500), "irrelevant body", "Chris")
		if n := graphemes.Count(got); n > MaxGraphemes {
			t.Fatalf("Outbound of 500-char subject has %d graphemes, want <= %d", n, MaxGraphemes)
		}
	})
}

func TestNormalizeInbound(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "camping this weekend", "camping this weekend"},
		{"single Re", "Re: camping", "camping"},
		{"chained mixed-case prefixes", "Re: Re: Fwd: camping", "camping"},
		{"localised prefixes", "AW: SV: VS: RES: Antw: TR: WG: camping", "camping"},
		{"lowercase prefixes", "re: fwd: fw: camping", "camping"},
		{"RFC 2047 encoded plus prefix", "Re: =?utf-8?B?w6nDqcOp?=", "ééé"},
		{"collapses internal whitespace", "camping   this\tweekend\n", "camping this weekend"},
		{"prefix-like word inside subject is untouched", "Report: quarterly numbers", "Report: quarterly numbers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeInbound(c.in); got != c.want {
				t.Errorf("NormalizeInbound(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeInbound_CapsAt100Graphemes(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := NormalizeInbound(long)
	if n := graphemes.Count(got); n != MaxGraphemes {
		t.Fatalf("NormalizeInbound of 500-char subject has %d graphemes, want %d", n, MaxGraphemes)
	}
}

func TestNormalizeInbound_MalformedEncodedWordDoesNotFail(t *testing.T) {
	// A broken encoded-word must not make derivation drop the letter (§5.2):
	// it should fall through to the raw text rather than erroring out silently.
	got := NormalizeInbound("Re: =?utf-8?Q?broken")
	if got == "" {
		t.Fatalf("NormalizeInbound of malformed encoded-word returned empty string")
	}
}

// V-3: a subject containing a CRLF and a fake header must produce no injected
// header and a flattened, single-line subject.
func TestSanitize_HeaderInjection(t *testing.T) {
	raw := "camping!\r\nBcc: attacker@evil.test"
	got := Sanitize(raw)

	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Sanitize(%q) = %q still contains a raw CR or LF", raw, got)
	}
	if strings.Contains(got, "\nBcc:") || strings.Contains(got, "\rBcc:") {
		t.Fatalf("Sanitize(%q) = %q contains an injectable header line", raw, got)
	}

	// The encoded header value must also be a single line: EncodeHeader must
	// not reintroduce a raw control character.
	encoded := EncodeHeader(got)
	if strings.ContainsAny(encoded, "\r\n") {
		t.Fatalf("EncodeHeader(%q) = %q contains a raw CR or LF", got, encoded)
	}
}

// V-3: every control character must be neutralised, not just CR/LF.
func TestSanitize_AllControlCharsStripped(t *testing.T) {
	raw := "a\x00b\x01c\x1bd\x7fe\tf"
	got := Sanitize(raw)
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("Sanitize(%q) = %q retains control byte %U", raw, got, r)
		}
	}
}

// V-3: non-ASCII must round-trip through RFC 2047.
func TestEncodeHeader_RoundTripsNonASCII(t *testing.T) {
	original := "camping avec Renée \U0001F3D5️"
	sanitized := Sanitize(original)
	encoded := EncodeHeader(sanitized)

	if encoded == sanitized {
		t.Fatalf("EncodeHeader(%q) did not encode non-ASCII input", sanitized)
	}
	if strings.ContainsAny(encoded, "\r\n") {
		t.Fatalf("EncodeHeader(%q) contains a raw CR or LF", encoded)
	}

	decoded := NormalizeInbound(encoded)
	if decoded != sanitized {
		t.Fatalf("round trip failed: original %q -> encoded %q -> decoded %q", sanitized, encoded, decoded)
	}
}

func TestEncodeHeader_ASCIIPassesThroughUnchanged(t *testing.T) {
	s := "camping this weekend"
	if got := EncodeHeader(s); got != s {
		t.Fatalf("EncodeHeader(%q) = %q, want unchanged", s, got)
	}
}

// V-3: a 500-character subject is capped at 100 graphemes.
func TestSanitize_CapsAt100Graphemes(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := Sanitize(long)
	if n := graphemes.Count(got); n != MaxGraphemes {
		t.Fatalf("Sanitize of 500-char subject has %d graphemes, want %d", n, MaxGraphemes)
	}
}

func TestGenerate_FirstWordsOfBody(t *testing.T) {
	body := "camping this weekend was so much fun we saw a bear and a raccoon and also a deer near the lake"
	got := Generate(body, "Chris")
	if !strings.HasPrefix(got, "camping this weekend") {
		t.Fatalf("Generate(%q, ...) = %q, want it to start with the body's first words", body, got)
	}
	if n := graphemes.Count(got); n > generatedMaxGraphemes {
		t.Fatalf("Generate result has %d graphemes, want <= %d", n, generatedMaxGraphemes)
	}
}

func TestGenerate_FallsBackToOwnerName(t *testing.T) {
	cases := []string{"", "   ", "\n\t\r", "\x00\x01"}
	for _, body := range cases {
		got := Generate(body, "Chris")
		want := "Letter from Chris"
		if got != want {
			t.Errorf("Generate(%q, %q) = %q, want %q", body, "Chris", got, want)
		}
	}
}

func TestGenerate_StripsControlCharsFromBody(t *testing.T) {
	got := Generate("hi\r\nBcc: attacker@evil.test", "Chris")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Generate result %q retains a raw CR or LF", got)
	}
}

func TestOutbound_UsesDeviceSubjectWhenPresent(t *testing.T) {
	got := Outbound("camping!", "irrelevant body", "Chris")
	if got != "camping!" {
		t.Fatalf("Outbound with a device subject = %q, want %q", got, "camping!")
	}
}

func TestOutbound_GeneratesWhenAbsent(t *testing.T) {
	got := Outbound("", "great news from camp", "Chris")
	if !strings.HasPrefix(got, "great news from camp") {
		t.Fatalf("Outbound with no device subject = %q, want it derived from the body", got)
	}
}

func TestOutbound_GeneratesWhenBodyEmptyToo(t *testing.T) {
	got := Outbound("", "", "Chris")
	if got != "Letter from Chris" {
		t.Fatalf("Outbound with no subject and no body = %q, want %q", got, "Letter from Chris")
	}
}

func TestOutbound_NeverContainsRawControlChars(t *testing.T) {
	got := Outbound("hi\r\nBcc: attacker@evil.test", "body\r\nBcc: attacker@evil.test", "Chris")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Outbound result %q contains a raw CR or LF", got)
	}
}
