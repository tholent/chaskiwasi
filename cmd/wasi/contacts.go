package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/config"
)

// runContacts implements `wasi contacts` (§14): contact-list maintenance
// with the server stopped.
//
// # Why the server must be stopped, not merely made safe to run alongside
//
// ayllu.toml is documented as hand-editable only while Wasi is stopped
// (§3). That is the same two-writers shape implementation-plan.md's F-8
// found as a real bug in guardians.toml: `wasi useradd` wrote the file
// while the running server held a stale in-memory copy, and the server's
// next write silently deleted the account the CLI had just added. F-8's fix
// was to make guardians.FileStore re-read the file when it changes — which
// works there because a guardian account carries no announcement
// obligation.
//
// A contact-list mutation does carry one: I-4 requires a notice letter for
// every add, deactivate, reactivate, and readdress. Re-reading ayllu.toml on
// the server side would close the staleness half of F-8's bug, but it does
// nothing about the other half — the running server's notice machinery has
// no way to learn a change happened at all if this CLI is the one that made
// it, re-read-on-change or not. So this package takes the option the task
// allows instead of the one F-8 used: detect a running server and refuse
// outright, via acquireDataLock (lock_unix.go). `wasi serve` holds that same
// lock for its entire run.
//
// # What this CLI can honestly promise about notices
//
// A stopped-server CLI cannot APPEND a notice letter itself — that needs a
// live IMAP connection, which is exactly what "stopped" rules out. Every
// mutation below instead goes through ayllu.Store.Mutate, the same entry
// point the guardian UI uses, which durably writes ayllu.toml and appends
// the change-log line before returning (§7.6) — the durable record I-4
// depends on. Nothing here is silent: the change-log line is written before
// this command exits successfully, and every mutation prints as much to the
// operator.
//
// Sending the letter is left to the next `wasi serve` start, which already
// reads that change log and announces anything with no matching notice yet
// in INBOX (notice.Service.Reconcile, built for F-5's crash-recovery case).
// From the server's point of view, a change this CLI made and a change the
// server itself crashed halfway through announcing are indistinguishable —
// both are a durable change-log line with no notice yet — so Reconcile,
// built to repair the second case, repairs the first one for free. That is
// a deliberate reuse, not an accident: it is what lets this command avoid
// ever touching internal/notice or state.json's pending_notices, both of
// which need machinery (a live mailbox, a specific crash-ordered write
// sequence) this CLI has no business reimplementing. What "no mutation
// sends a notice itself" means concretely is printed after every mutation
// below, so it is never mistaken for I-4's one forbidden outcome — a change
// with no announcement, ever.
func runContacts(args []string) error {
	if len(args) == 0 {
		contactsUsage()
		return errors.New("contacts: a subcommand is required")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runContactsList(rest)
	case "add":
		return runContactsAdd(rest)
	case "deactivate":
		return runContactsDeactivate(rest)
	case "reactivate":
		return runContactsReactivate(rest)
	case "readdress":
		return runContactsReaddress(rest)
	case "help", "-h", "-help", "--help":
		contactsUsage()
		return nil
	default:
		contactsUsage()
		return fmt.Errorf("contacts: unknown subcommand %q", sub)
	}
}

func contactsUsage() {
	fmt.Fprint(os.Stderr, `usage: wasi contacts <subcommand> [flags]

ayllu.toml is hand-editable, and therefore CLI-editable, only while Wasi is
not running (wasi-server-plan §3). This command refuses if it detects a
running "wasi serve" against the same data directory.

Subcommands:

  list                              list every contact, tombstones included
  add        -name -address         add a contact (reactivates a tombstone
                                     whose current or past address matches
                                     instead of creating a second row, §7.2)
  deactivate <contact-id>           remove a contact (never deletes: I-5)
  reactivate <contact-id>           undo an accidental deactivate
  readdress  <contact-id> -address  fix a mistyped or changed address

Every subcommand but "list" also takes -actor, recorded in ayllu-log.jsonl.
Run "wasi contacts <subcommand> -h" for a subcommand's full flag list.

No mutation sends a notice letter itself — the change is written durably to
ayllu.toml and ayllu-log.jsonl, and the next "wasi serve" start announces it
automatically. See the package doc in contacts.go for why that is the
honest thing a stopped-server command can promise.
`)
}

// commonFlags is what every mutating subcommand needs to open the same
// store `wasi serve` and the guardian UI use.
type commonFlags struct {
	dataDir    string
	configPath string
	actor      string
}

func addCommonFlags(fs *flag.FlagSet, includeActor bool) *commonFlags {
	cf := &commonFlags{}
	fs.StringVar(&cf.dataDir, "data", defaultDataDir, "path to the server-owned data directory")
	fs.StringVar(&cf.configPath, "config", defaultConfigPath, "path to wasi.toml, read for ayllu.max_contacts (§13)")
	if includeActor {
		fs.StringVar(&cf.actor, "actor", "",
			"name recorded as the actor in ayllu-log.jsonl (default: the OS user running this command)")
	}
	return cf
}

func runContactsList(args []string) error {
	fs := flag.NewFlagSet("contacts list", flag.ContinueOnError)
	cf := addCommonFlags(fs, false)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: wasi contacts list [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("contacts list: unexpected arguments")
	}

	store, release, err := openStore(cf.dataDir, cf.configPath)
	if err != nil {
		return err
	}
	defer release()

	_, contacts := store.List()
	printContacts(os.Stdout, contacts)
	return nil
}

func runContactsAdd(args []string) error {
	fs := flag.NewFlagSet("contacts add", flag.ContinueOnError)
	cf := addCommonFlags(fs, true)
	name := fs.String("name", "", "the person's name, as the child will see it (required)")
	address := fs.String("address", "", "their email address (required)")
	pinned := fs.Bool("pinned", false, "set the youth's pinned overlay flag")
	order := fs.Int("order", 0, "set the youth's display-order overlay value")
	portrait := fs.String("portrait", "", "set the youth's portrait glyph id")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: wasi contacts add -name NAME -address ADDRESS [flags]\n\n"+
			"An address matching a tombstone's current or past address reactivates\n"+
			"that contact instead of creating a second row (§7.2): a person who\n"+
			"leaves and returns stays one person in the archive.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("contacts add: unexpected arguments")
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*address) == "" {
		fs.Usage()
		return errors.New("contacts add: -name and -address are both required")
	}

	return mutateAndReport(cf, ayllu.Mutation{
		Action:   ayllu.ActionAdd,
		Name:     *name,
		Address:  *address,
		Pinned:   *pinned,
		Order:    *order,
		Portrait: *portrait,
	})
}

func runContactsDeactivate(args []string) error {
	return mutateByID("contacts deactivate", args, ayllu.ActionDeactivate,
		"usage: wasi contacts deactivate <contact-id> [flags]\n\n"+
			"Deactivation never deletes (I-5): the address is retained, the\n"+
			"person's history keeps rendering, and outbound to them is rejected.\n"+
			"Undo with \"wasi contacts reactivate\".\n\n")
}

func runContactsReactivate(args []string) error {
	return mutateByID("contacts reactivate", args, ayllu.ActionReactivate,
		"usage: wasi contacts reactivate <contact-id> [flags]\n\n"+
			"Reverses an accidental deactivate. Sending resumes immediately once\n"+
			"the next \"wasi serve\" start announces it.\n\n")
}

// mutateByID implements deactivate and reactivate, which take only a
// contact id plus the common flags.
func mutateByID(name string, args []string, action ayllu.Action, usageHeader string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cf := addCommonFlags(fs, true)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageHeader)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("%s: exactly one contact id is required", name)
	}

	return mutateAndReport(cf, ayllu.Mutation{Action: action, ContactID: fs.Arg(0)})
}

func runContactsReaddress(args []string) error {
	fs := flag.NewFlagSet("contacts readdress", flag.ContinueOnError)
	cf := addCommonFlags(fs, true)
	address := fs.String("address", "", "the new address (required)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: wasi contacts readdress <contact-id> -address ADDRESS [flags]\n\n"+
			"The old address is retained and keeps resolving for that person's\n"+
			"past letters; new mail arriving at it afterwards goes to Held, since\n"+
			"a readdress usually means the old account was lost (§7.2, F-1).\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("contacts readdress: exactly one contact id is required")
	}
	if strings.TrimSpace(*address) == "" {
		fs.Usage()
		return errors.New("contacts readdress: -address is required")
	}

	return mutateAndReport(cf, ayllu.Mutation{
		Action:    ayllu.ActionReaddress,
		ContactID: fs.Arg(0),
		Address:   *address,
	})
}

// mutateAndReport opens the store, applies m, prints the resulting contact
// list, and — every time, unconditionally — states plainly that no notice
// was sent yet. I-4 forbids a silent change; the point of this line is that
// a CLI mutation never reads as one, even to an operator who didn't read
// the package doc first.
func mutateAndReport(cf *commonFlags, m ayllu.Mutation) error {
	actor := cf.actor
	if actor == "" {
		actor = defaultActor()
	}

	store, release, err := openStore(cf.dataDir, cf.configPath)
	if err != nil {
		return err
	}
	defer release()

	ev, err := store.Mutate(actor, m)
	if err != nil {
		return fmt.Errorf("contacts: %w", err)
	}

	_, contacts := store.List()
	printContacts(os.Stdout, contacts)
	fmt.Printf("\n%s: %s (%s), ayllu version %d, recorded in ayllu-log.jsonl as actor %q.\n",
		ev.Action, ev.Name, ev.ContactID, ev.Version, ev.Actor)
	fmt.Println("No notice was sent — the server is stopped. The next \"wasi serve\" start " +
		"announces it automatically (it reconciles ayllu-log.jsonl against INBOX at startup, §7.6).")
	return nil
}

// openStore acquires the data-directory lock and opens the ayllu store
// against it, refusing with an operator-facing message if `wasi serve`
// already holds the lock. Callers must invoke release (defer is fine) once
// done; FileStore itself has nothing else to close.
func openStore(dataDir, configPath string) (ayllu.Store, func(), error) {
	release, err := acquireDataLock(dataDir)
	if err != nil {
		if errors.Is(err, errDataDirBusy) {
			return nil, nil, fmt.Errorf(
				"contacts: %s is in use by a running wasi server; stop it first (wasi-server-plan §3): %w",
				dataDir, err)
		}
		return nil, nil, fmt.Errorf("contacts: %w", err)
	}

	maxContacts, err := maxContactsFrom(configPath)
	if err != nil {
		release()
		return nil, nil, err
	}

	store, err := ayllu.Open(dataDir, maxContacts)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("contacts: %w", err)
	}
	return store, release, nil
}

// maxContactsFrom reads ayllu.max_contacts from wasi.toml so this command's
// cap agrees with the running server's (§13, A.3). A missing config file
// falls back to the documented default rather than failing outright: a
// fresh data directory can legitimately be seeded with contacts before
// wasi.toml exists or before the first `wasi serve` run.
func maxContactsFrom(configPath string) (int, error) {
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return config.DefaultMaxContacts, nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return 0, fmt.Errorf("contacts: %w", err)
	}
	return cfg.Ayllu.MaxContacts, nil
}

// defaultActor names the operator in ayllu-log.jsonl and, for a readdress,
// in the notice text itself ("Rosa's address was updated by Dad."). Falling
// back to the OS user beats an empty string, which readdress's wording
// treats as "no actor recorded" (internal/notice/text.go) — a detail this
// command shouldn't reproduce just because nobody passed -actor. Inside the
// distroless production image there is no /etc/passwd for user.Current to
// resolve, so this falls further to $USER and finally to a fixed label
// rather than erroring out over a cosmetic field.
func defaultActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "cli"
}

// printContacts renders the table every subcommand below prints after it
// acts, so an operator always sees the state a change landed in.
// Tombstones are marked plainly rather than left to be inferred from a
// blank "active" column, per the task's "including tombstones, clearly
// marked" requirement.
func printContacts(w io.Writer, contacts []ayllu.Contact) {
	sorted := append([]ayllu.Contact(nil), contacts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	if len(sorted) == 0 {
		fmt.Fprintln(w, "(no contacts)")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tADDRESS\tSTATUS\tPINNED\tORDER\tPAST ADDRESSES")
	for _, c := range sorted {
		status := "active"
		if !c.Active {
			status = "TOMBSTONE"
		}
		pinned := ""
		if c.Pinned {
			pinned = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			c.ID, c.Name, c.Address, status, pinned, c.Order, strings.Join(c.PastAddresses, ", "))
	}
	tw.Flush()
}
