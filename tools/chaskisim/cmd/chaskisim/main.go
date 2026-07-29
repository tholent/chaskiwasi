// Command chaskisim is a CLI wrapper around package chaskisim: enough of a
// Chaski device's side of wasi-server-plan §4's wire contract to demonstrate
// and exercise a running Wasi server with zero hardware (§15). test/e2e
// drives the same behaviour by importing tools/chaskisim directly, for
// tighter in-process assertions than parsing this CLI's output would allow;
// this binary exists for interactive, human-driven demonstration and manual
// testing against a real (or compose-local) Wasi instance.
//
// Subcommands:
//
//	compose   add a child-authored letter to the outbox
//	sync      perform exactly one POST /sync round trip
//	drain     wake and sync until more: false, capped at 10 rounds (§4.6)
//	dump      print the simulated device's current state as JSON
//	reset     simulate a factory reset (wipes all local state)
//	pututu    verify or mint a CH1.<counter>.<mac> doorbell token (§10.2)
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tholent/chaskiwasi/internal/protocol"
	"github.com/tholent/chaskiwasi/tools/chaskisim"
)

// defaultStatePath is where a simulated device's flash lives when nothing
// else is specified — a single file in the working directory, so running
// this tool from a scratch directory per simulated device is the natural
// way to keep several devices apart.
const defaultStatePath = "chaskisim-state.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "compose":
		err = runCompose(os.Args[2:])
	case "sync":
		err = runSync(os.Args[2:])
	case "drain":
		err = runDrain(os.Args[2:])
	case "dump":
		err = runDump(os.Args[2:])
	case "reset":
		err = runReset(os.Args[2:])
	case "pututu":
		err = runPututu(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "chaskisim: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: chaskisim <command> [flags]

  compose   add a child-authored letter to the outbox
  sync      perform exactly one POST /sync round trip
  drain     wake and sync until more: false (capped at 10 rounds, §4.6)
  dump      print the simulated device's current state
  reset     simulate a factory reset (wipes all local state)
  pututu    verify or mint an SMS doorbell token (verify|mint)

Every command accepts -state (default "chaskisim-state.json", or
$CHASKISIM_STATE). Commands that talk to a server also accept -url/-token/
-insecure-skip-verify/-ca-cert, or $CHASKISIM_URL/$CHASKISIM_TOKEN.
`)
}

// envOr returns the environment variable key's value, or fallback if unset
// or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func statePathFlag(fs *flag.FlagSet) *string {
	return fs.String("state", envOr("CHASKISIM_STATE", defaultStatePath),
		"path to the simulated device's state file ($CHASKISIM_STATE)")
}

// netFlags are the flags shared by every command that actually talks to a
// device sync endpoint.
type netFlags struct {
	url      string
	token    string
	insecure bool
	caCert   string
}

func addNetFlags(fs *flag.FlagSet) *netFlags {
	nf := &netFlags{}
	fs.StringVar(&nf.url, "url", envOr("CHASKISIM_URL", ""),
		"device sync endpoint, e.g. https://localhost:8443/sync ($CHASKISIM_URL)")
	fs.StringVar(&nf.token, "token", envOr("CHASKISIM_TOKEN", ""),
		"device bearer token ($CHASKISIM_TOKEN)")
	fs.BoolVar(&nf.insecure, "insecure-skip-verify", false,
		"skip TLS certificate verification — ONLY correct against the local dev fixture's self-signed cert (deploy/README.md); never against a real deployment")
	fs.StringVar(&nf.caCert, "ca-cert", "",
		"PEM file containing the private CA to trust (§12.2)")
	return nf
}

// client builds a chaskisim.Client from the parsed flags, or a descriptive
// error if the minimum required flags are missing — failing here beats a
// deep, confusing dial error later.
func (nf *netFlags) client() (*chaskisim.Client, error) {
	if nf.url == "" {
		return nil, fmt.Errorf("-url (or $CHASKISIM_URL) is required")
	}
	if nf.token == "" {
		return nil, fmt.Errorf("-token (or $CHASKISIM_TOKEN) is required")
	}

	tlsConfig := &tls.Config{}
	switch {
	case nf.insecure:
		tlsConfig.InsecureSkipVerify = true
	case nf.caCert != "":
		pem, err := os.ReadFile(nf.caCert)
		if err != nil {
			return nil, fmt.Errorf("reading -ca-cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-ca-cert %s: no certificates found", nf.caCert)
		}
		tlsConfig.RootCAs = pool
	}

	return chaskisim.NewClient(chaskisim.ClientConfig{
		BaseURL:   nf.url,
		Token:     nf.token,
		TLSConfig: tlsConfig,
	}), nil
}

func runCompose(args []string) error {
	fs := flag.NewFlagSet("compose", flag.ContinueOnError)
	statePath := statePathFlag(fs)
	contactID := fs.String("contact", "", "contact_id to send to (required)")
	subject := fs.String("subject", "", "subject; empty means the server generates one (§6.2)")
	body := fs.String("body", "", "letter body (required)")
	localID := fs.String("local-id", "", "device-assigned local_id; auto-generated if empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *contactID == "" || *body == "" {
		return fmt.Errorf("compose: -contact and -body are required")
	}

	d, err := chaskisim.Open(*statePath)
	if err != nil {
		return err
	}
	letter := d.Compose(*contactID, *subject, *body, *localID)
	if err := d.Save(); err != nil {
		return err
	}

	fmt.Printf("queued %s to %s (%d bytes)\n", letter.LocalID, letter.ContactID, len(letter.Body))
	return nil
}

func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	statePath := statePathFlag(fs)
	nf := addNetFlags(fs)
	width := fs.Int("width", 24, "render width in graphemes, for displaying delivered letters (§4.9)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := nf.client()
	if err != nil {
		return err
	}
	d, err := chaskisim.Open(*statePath)
	if err != nil {
		return err
	}

	before := d.State()
	resp, syncErr := d.SyncOnce(context.Background(), client)
	if saveErr := d.Save(); saveErr != nil {
		return fmt.Errorf("sync: %v (state save also failed: %w)", syncErr, saveErr)
	}
	if syncErr != nil {
		return syncErr
	}

	printSyncResult(before, resp, *width)
	return nil
}

func runDrain(args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	statePath := statePathFlag(fs)
	nf := addNetFlags(fs)
	width := fs.Int("width", 24, "render width in graphemes, for displaying delivered letters (§4.9)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := nf.client()
	if err != nil {
		return err
	}
	d, err := chaskisim.Open(*statePath)
	if err != nil {
		return err
	}

	before := d.State()
	responses, syncErr := d.Wake(context.Background(), client)
	if saveErr := d.Save(); saveErr != nil {
		return fmt.Errorf("drain: %v (state save also failed: %w)", syncErr, saveErr)
	}
	if syncErr != nil {
		return syncErr
	}

	printDrainResult(before, responses, *width)
	return nil
}

func runDump(args []string) error {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	statePath := statePathFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := chaskisim.Open(*statePath)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(d.State(), "", "  ")
	if err != nil {
		return fmt.Errorf("dump: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

func runReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	statePath := statePathFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := chaskisim.Open(*statePath)
	if err != nil {
		return err
	}
	d.Reset()
	if err := d.Save(); err != nil {
		return err
	}

	fmt.Println("factory reset complete: cursor, ayllu cache, outbox, and dedup memory all cleared")
	return nil
}

func runPututu(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("pututu: expected a subcommand: verify <token> | mint <counter>")
	}
	switch args[0] {
	case "verify":
		return runPututuVerify(args[1:])
	case "mint":
		return runPututuMint(args[1:])
	default:
		return fmt.Errorf("pututu: unknown subcommand %q (want verify or mint)", args[0])
	}
}

func runPututuVerify(args []string) error {
	fs := flag.NewFlagSet("pututu verify", flag.ContinueOnError)
	statePath := statePathFlag(fs)
	keyHex := fs.String("key", os.Getenv("CHASKISIM_PUTUTU_KEY"), "hex-encoded pututu HMAC key ($CHASKISIM_PUTUTU_KEY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("pututu verify: expected exactly one token argument")
	}

	key, err := hex.DecodeString(*keyHex)
	if err != nil {
		return fmt.Errorf("pututu verify: -key must be hex: %w", err)
	}

	d, err := chaskisim.Open(*statePath)
	if err != nil {
		return err
	}
	// §10.2: a real device does none of this reporting — it verifies and
	// either silently accepts or silently ignores. This command exists to
	// make that internal, otherwise-invisible decision observable for
	// testing and demonstration; nothing about the wire contract itself
	// exposes it.
	result := d.AcceptPututu(fs.Arg(0), key)
	if err := d.Save(); err != nil {
		return err
	}

	fmt.Printf("valid=%v accepted=%v would_wake=%v counter=%d\n",
		result.Valid, result.Accepted, result.WouldWake, result.Counter)
	return nil
}

func runPututuMint(args []string) error {
	fs := flag.NewFlagSet("pututu mint", flag.ContinueOnError)
	keyHex := fs.String("key", os.Getenv("CHASKISIM_PUTUTU_KEY"), "hex-encoded pututu HMAC key ($CHASKISIM_PUTUTU_KEY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("pututu mint: expected exactly one counter argument")
	}
	counter, err := strconv.ParseUint(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("pututu mint: counter must be a non-negative integer: %w", err)
	}
	key, err := hex.DecodeString(*keyHex)
	if err != nil {
		return fmt.Errorf("pututu mint: -key must be hex: %w", err)
	}

	// This mints a token the way a real server would (§10.2), so this tool
	// can demonstrate and test the doorbell path without wave 3's
	// internal/pututu running anywhere — the CLI equivalent of an SMS
	// arriving.
	fmt.Println(chaskisim.MintPututuToken(counter, key))
	return nil
}

// seenSetOf builds a lookup set from a State snapshot's dedup ring, for the
// CLI's own "what's new this round" display — separate from, and simpler
// than, chaskisim.NewLetters, because here we want a running set updated as
// we print rather than a single before/after comparison.
func seenSetOf(s chaskisim.State) map[string]bool {
	out := make(map[string]bool, len(s.SeenLetterIDs))
	for _, id := range s.SeenLetterIDs {
		out[id] = true
	}
	return out
}

func printSyncResult(before chaskisim.State, resp *protocol.Response, width int) {
	seen := seenSetOf(before)
	newCount := 0
	for _, l := range resp.Letters {
		if seen[l.ID] {
			continue
		}
		printLetter(l, width)
		newCount++
	}
	for _, ack := range resp.Acks {
		fmt.Printf("ack %s: %s\n", ack.LocalID, ack.Status)
	}
	fmt.Printf("cursor=%q more=%v new_letters=%d pututu_counter=%d\n",
		resp.Cursor, resp.More, newCount, resp.PututuCounter)
}

func printDrainResult(before chaskisim.State, responses []*protocol.Response, width int) {
	seen := seenSetOf(before)
	total := 0
	for i, resp := range responses {
		roundNew := 0
		for _, l := range resp.Letters {
			if seen[l.ID] {
				continue
			}
			seen[l.ID] = true
			printLetter(l, width)
			total++
			roundNew++
		}
		for _, ack := range resp.Acks {
			fmt.Printf("ack %s: %s\n", ack.LocalID, ack.Status)
		}
		fmt.Printf("round %d/%d: more=%v new_letters=%d\n", i+1, len(responses), resp.More, roundNew)
	}
	fmt.Printf("drained %d round(s), %d new letter(s) total\n", len(responses), total)
}

// printLetter renders one delivered letter the way a Chaski panel would:
// the server ships a single body string and owns zero layout numbers
// (§4.9, A.10), so this is the demonstration of firmware-owned reflow —
// line-broken only on grapheme-cluster boundaries (chaskisim.Wrap).
func printLetter(l protocol.Letter, width int) {
	fmt.Printf("--- %s from %s ---\n", l.ID, l.ContactID)
	if l.Subject != "" {
		fmt.Printf("Subject: %s\n", l.Subject)
	}
	for _, line := range chaskisim.Wrap(l.Body, width) {
		fmt.Println(line)
	}
	var flags []string
	if l.Trimmed {
		flags = append(flags, "trimmed")
	}
	if l.Truncated {
		flags = append(flags, "truncated")
	}
	if l.Degraded {
		flags = append(flags, "degraded")
	}
	if len(flags) > 0 {
		fmt.Printf("[%s]\n", strings.Join(flags, ", "))
	}
}
