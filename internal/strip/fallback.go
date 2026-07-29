package strip

import "strings"

// signatureDelimiter is the conventional "-- " sign-off marker (dash, dash,
// exactly one space) that both mail clients and RFC 3676 §4.5 treat as
// never a soft-flow line, even though it ends in a space.
const signatureDelimiter = "-- "

// Fallback applies the minimal in-process rule set used when the strip
// service is unreachable (§5.3): drop leading '>'-quoted blocks, then cut at
// a "-- " signature delimiter. Degraded is always true.
//
// This is deliberately much narrower than talon.quotations (services/strip):
// it does not recognise "On ... wrote:" attribution lines or
// "-----Original Message-----" blocks, so a trailing quoted reply — the most
// common real-world shape — usually passes through untouched. See
// fallback_test.go's golden-corpus cases for exactly which patterns this
// does and doesn't catch; the gap is intentional scope, not a bug, and it's
// what Degraded exists to communicate downstream.
func Fallback(text string, formatFlowed bool) Result {
	working := text
	if formatFlowed {
		working = unwrapFlowed(working)
	}

	trimmedBody := cutAtSignature(dropLeadingQuoted(working))

	return Result{
		Body:     trimmedBody,
		Trimmed:  trimmedBody != working,
		Degraded: true,
	}
}

// dropLeadingQuoted removes a contiguous run of leading '>'-quoted lines,
// and any blank lines interleaved with them, from the start of text. A
// message that opens with quoted context before the real reply — some
// mobile and top-posting clients produce this — loses that opening block;
// a message that never had one is returned unchanged.
func dropLeadingQuoted(text string) string {
	lines := strings.Split(text, "\n")
	i := 0
	for i < len(lines) && (strings.HasPrefix(lines[i], ">") || strings.TrimSpace(lines[i]) == "") {
		i++
	}
	return strings.Join(lines[i:], "\n")
}

// cutAtSignature drops everything from the first line that is exactly the
// signature delimiter onward. Text with no such line is returned unchanged.
func cutAtSignature(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line == signatureDelimiter {
			return strings.Join(lines[:i], "\n")
		}
	}
	return text
}

// unwrapFlowed undoes RFC 3676 format=flowed soft-wrapping (DelSp=no, the
// default — this package's callers carry a bare formatFlowed bool with no
// DelSp, matching the wire contract, §11.1). See services/strip/striplib.py's
// unwrap_flowed for the identical rule, kept in sync deliberately since both
// implementations need to agree on what "flowed" text looks like once
// unwrapped even though everything downstream of that (talon vs. these two
// narrow rules) is allowed to diverge.
func unwrapFlowed(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, " ") {
			line = line[1:]
		}

		// The signature delimiter must never be merged into by the line
		// before it, or a soft-broken "...text " immediately followed by
		// "-- " would silently glue into "...text -- " — which cutAtSignature
		// no longer recognises as the delimiter at all.
		n := len(out)
		if n > 0 && line != signatureDelimiter && isSoftBreak(out[n-1]) {
			out[n-1] += line
		} else {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// isSoftBreak reports whether line ends in a space that RFC 3676 treats as a
// soft line-break marker to be joined with the next line, rather than a
// meaningful trailing space (the signature delimiter is the one line that
// legitimately ends in " " but must never be joined onward).
func isSoftBreak(line string) bool {
	return strings.HasSuffix(line, " ") && line != signatureDelimiter
}
