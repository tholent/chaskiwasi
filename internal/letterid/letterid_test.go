package letterid

import (
	"regexp"
	"strings"
	"testing"
)

var idShape = regexp.MustCompile(`^l-[0-9a-f]{10}$`)

func TestFromMessageID_Deterministic(t *testing.T) {
	raw := "<abc123.xyz@mail.example.com>"
	a := FromMessageID(raw)
	b := FromMessageID(raw)
	if a != b {
		t.Fatalf("FromMessageID not deterministic: %q vs %q", a, b)
	}
}

func TestFromMessageID_Shape(t *testing.T) {
	ids := []string{
		FromMessageID("<abc123.xyz@mail.example.com>"),
		FromMessageID(""),
		FromMessageID("<>"),
	}
	for _, id := range ids {
		if !idShape.MatchString(id) {
			t.Errorf("id %q does not match l-<10 lowercase hex>", id)
		}
	}
}

func TestFromMessageID_DifferentInputsDiffer(t *testing.T) {
	a := FromMessageID("<one@example.com>")
	b := FromMessageID("<two@example.com>")
	if a == b {
		t.Fatalf("distinct Message-IDs collided: %q", a)
	}
}

// The raw Message-ID header can leak the sender's mail-client hostname; it
// must never appear, in whole or in the recognisable part, inside the id.
func TestFromMessageID_NeverExposesRawHeader(t *testing.T) {
	raw := "<abc123.xyz@sendinghost.example.com>"
	id := FromMessageID(raw)
	if strings.Contains(id, "sendinghost") || strings.Contains(id, "abc123") {
		t.Fatalf("id %q leaks raw Message-ID content from %q", id, raw)
	}
}

func TestFromFallback_Deterministic(t *testing.T) {
	from := "Rosa <rosa@example.com>"
	date := "Tue, 29 Jul 2026 10:00:00 -0700"
	body := []byte("Dear kiddo, camping was great...")

	a := FromFallback(from, date, body)
	b := FromFallback(from, date, body)
	if a != b {
		t.Fatalf("FromFallback not deterministic: %q vs %q", a, b)
	}
	if !idShape.MatchString(a) {
		t.Errorf("id %q does not match l-<10 lowercase hex>", a)
	}
}

func TestFromFallback_DistinctInputsDiffer(t *testing.T) {
	from := "Rosa <rosa@example.com>"
	date := "Tue, 29 Jul 2026 10:00:00 -0700"

	a := FromFallback(from, date, []byte("body one"))
	b := FromFallback(from, date, []byte("body two"))
	if a == b {
		t.Fatalf("distinct bodies collided: %q", a)
	}

	c := FromFallback("Other <other@example.com>", date, []byte("body one"))
	if a == c {
		t.Fatalf("distinct senders collided: %q", a)
	}
}

// Concatenation without a separator would let a From/Date split ambiguously;
// this guards against that class of collision.
func TestFromFallback_FieldSeparatorPreventsCollision(t *testing.T) {
	a := FromFallback("ab", "c", []byte("body"))
	b := FromFallback("a", "bc", []byte("body"))
	if a == b {
		t.Fatalf("field-boundary collision: FromFallback(%q,%q) == FromFallback(%q,%q) == %q", "ab", "c", "a", "bc", a)
	}
}

func TestFromFallback_LongBodyCappedAt1KB(t *testing.T) {
	from := "Rosa <rosa@example.com>"
	date := "Tue, 29 Jul 2026 10:00:00 -0700"

	base := strings.Repeat("x", 1024)
	short := FromFallback(from, date, []byte(base))
	// Appending anything after the first 1 KB must not change the id.
	long := FromFallback(from, date, []byte(base+"this changes nothing"))
	if short != long {
		t.Fatalf("fallback id changed when body grew beyond the 1 KB cap: %q vs %q", short, long)
	}

	// But a change within the first 1 KB must change the id.
	changedEarly := FromFallback(from, date, []byte("y"+base[1:]))
	if changedEarly == short {
		t.Fatalf("fallback id did not change for a difference within the 1 KB window")
	}
}
