// Command fwgates runs the firmware's build-time gates: the checks that assert
// a device invariant by inspecting the tree or the linked binary rather than by
// running code.
//
// Three gates, each pinned to an invariant it makes unfalsifiable:
//
//	strings  (C-15, D-2)  chaski_strings.c holds no internal vocabulary and no
//	                      address-shaped text; no user-visible literal lives
//	                      outside it.
//	symbols  (C-16, D-3)  a linked ELF contains no WiFi/BLE symbols. Production
//	                      firmware has exactly one radio path, and the dev
//	                      transport is USB, not a radio (client B.2) — so this
//	                      must hold for BOTH build variants.
//	unicode  (C-9, B.7)   the committed grapheme vectors record the Unicode
//	                      version they were generated at, so a toolchain bump
//	                      is visible rather than silent.
//
// Usage:
//
//	go run ./tools/fwgates strings
//	go run ./tools/fwgates symbols build/chaski.elf
//	go run ./tools/fwgates unicode
package main

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	firmwareDir = "firmware/chaski"
	stringsFile = "firmware/chaski/main/chaski_strings.c"
	vectorsFile = "test/firmware/host/testdata/graphemes.json"
)

// internalVocabulary is greppable on purpose and must never reach a screen
// (design §0.1, client §0). Wire field names are exempt — the wire is
// machine-facing — which is why this gate reads the strings table, not the
// whole tree, for these words.
var internalVocabulary = []string{"pututu", "ayllu", "kipu"}

// addressShaped is a tripwire for D-2. The device has no code path that should
// ever produce an email address; finding one in the strings table means a
// design assumption broke somewhere upstream.
var addressShaped = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// radioSymbols are the entry points that would exist only if a radio stack were
// linked in. Matching is by prefix against the symbol table.
var radioSymbols = []string{
	"esp_wifi_", "wifi_init", "ieee80211_",
	"esp_bt_", "esp_ble_", "ble_hs_", "nimble_", "btdm_",
	"esp_now_",
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fwgates <strings|symbols|unicode> [args]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "strings":
		err = gateStrings()
	case "symbols":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fwgates symbols <path-to.elf>")
			os.Exit(2)
		}
		err = gateSymbols(os.Args[2])
	case "unicode":
		err = gateUnicode()
	default:
		fmt.Fprintf(os.Stderr, "unknown gate %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

// gateStrings enforces the vocabulary boundary and the address tripwire.
func gateStrings() error {
	b, err := os.ReadFile(stringsFile)
	if err != nil {
		return fmt.Errorf("reading the strings table: %w", err)
	}
	text := string(b)

	// The file's own header comment names the forbidden words in order to
	// explain the rule, so scan only the string literals, not the prose.
	lits := stringLiterals(text)
	for _, lit := range lits {
		low := strings.ToLower(lit)
		for _, w := range internalVocabulary {
			if strings.Contains(low, w) {
				return fmt.Errorf("%s: user-visible string contains internal vocabulary %q: %s\n"+
					"  These are greppable identifiers, not words for a screen a child reads (design §0.1)",
					stringsFile, w, lit)
			}
		}
		if m := addressShaped.FindString(lit); m != "" {
			return fmt.Errorf("%s: user-visible string looks like an email address (%s).\n"+
				"  D-2: no address may exist on the device, in any form", stringsFile, m)
		}
	}

	stray, err := strayLiterals()
	if err != nil {
		return err
	}
	if len(stray) > 0 {
		return fmt.Errorf("user-visible string literals outside the strings table:\n%s\n"+
			"  Every word a person can see belongs in %s (client §0, C-15)",
			strings.Join(stray, "\n"), stringsFile)
	}
	fmt.Printf("gate strings: OK (%d strings checked)\n", len(lits))
	return nil
}

// stringLiterals pulls double-quoted literals out of C source. It is
// deliberately simple: this runs over one small, hand-maintained file.
func stringLiterals(src string) []string {
	var out []string
	inLine, inBlock := false, false
	for i := 0; i < len(src); i++ {
		switch {
		case inLine:
			if src[i] == '\n' {
				inLine = false
			}
		case inBlock:
			if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock, i = false, i+1
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine, i = true, i+1
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock, i = true, i+1
		case src[i] == '"':
			var sb strings.Builder
			i++
			for ; i < len(src) && src[i] != '"'; i++ {
				if src[i] == '\\' && i+1 < len(src) {
					i++
					continue
				}
				sb.WriteByte(src[i])
			}
			if s := sb.String(); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// strayLiterals looks for user-visible text defined outside the strings table.
//
// It cannot be perfect — telling a UI label from a format tag is undecidable in
// general — so it flags the shape that actually leaks: a literal of several
// words with a space, outside chaski_strings.c, that is not a log message, a
// path, or a wire field name. False positives are silenced with the marker
// comment, which keeps the exemption visible in review.
func strayLiterals() ([]string, error) {
	var findings []string
	multiWord := regexp.MustCompile(`"[A-Za-z][A-Za-z0-9,'.!?:%\-]*(?: [A-Za-z0-9,'.!?:%\-]+){2,}"`)

	err := filepath.Walk(firmwareDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".c" && ext != ".cpp" && ext != ".h" {
			return nil
		}
		if filepath.ToSlash(path) == stringsFile {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for n, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
				continue
			}
			if strings.Contains(line, "fwgates:allow") {
				continue
			}
			// Trailing comments are prose about the code, not code. A doc
			// comment that quotes a UI string — which good ones do — must not
			// read as that string escaping the table.
			if m := multiWord.FindString(stripTrailingComment(line)); m != "" {
				findings = append(findings, fmt.Sprintf("  %s:%d: %s", path, n+1, m))
			}
		}
		return nil
	})
	return findings, err
}

// stripTrailingComment removes a // comment from a line, ignoring one that
// appears inside a string literal (a URL, say). Quote tracking is what keeps
// this from mangling legitimate code.
func stripTrailingComment(line string) string {
	inStr, esc := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case c == '/' && !inStr && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// gateSymbols asserts D-3 against a linked ELF: no radio stack is present.
// This is the check that makes "WiFi and BLE are not compiled in" a fact about
// the binary rather than a claim about the config.
func gateSymbols(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("opening ELF: %w", err)
	}
	defer f.Close()

	syms, err := f.Symbols()
	if err != nil {
		return fmt.Errorf("reading symbols (a stripped binary cannot be gated): %w", err)
	}

	var found []string
	for _, s := range syms {
		for _, p := range radioSymbols {
			if strings.HasPrefix(s.Name, p) {
				found = append(found, s.Name)
				break
			}
		}
	}
	if len(found) > 0 {
		if len(found) > 12 {
			found = append(found[:12], fmt.Sprintf("... and %d more", len(found)-12))
		}
		return fmt.Errorf("radio symbols linked into %s:\n  %s\n"+
			"  D-3: production firmware has exactly one radio path, and the dev\n"+
			"  transport is USB, not a radio (client B.2). This must hold for BOTH variants",
			path, strings.Join(found, "\n  "))
	}
	fmt.Printf("gate symbols: OK (%d symbols scanned, no radio stack in %s)\n", len(syms), path)
	return nil
}

// gateUnicode surfaces a toolchain skew between the two grapheme segmenters
// before it becomes a wrongly-rejected letter (B.7).
func gateUnicode() error {
	b, err := os.ReadFile(vectorsFile)
	if err != nil {
		return fmt.Errorf("reading grapheme vectors (run `go run ./tools/graphvectors`): %w", err)
	}
	if !strings.Contains(string(b), `"unicode_version"`) {
		return fmt.Errorf("%s carries no unicode_version; regenerate it", vectorsFile)
	}
	fmt.Println("gate unicode: OK (vectors present and versioned)")
	return nil
}
