//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/tholent/chaskiwasi/tools/chaskisim"
)

// harness is one test's view of the stack: brought to a known state, with the
// three clients a case needs (a device, a guardian browser, and the mail
// store) already wired to it.
//
// Reset is per-test rather than per-run because these cases are not
// independent otherwise: V-21 rolls UIDVALIDITY, V-12 kills the server
// mid-write, and V-11 asserts over every byte under /data. Any of those
// leaking into the next case would produce a failure that has nothing to do
// with the clause under test — and, worse, a pass that does not.
type harness struct {
	stack *stack
	mail  *mailFixture
	ui    *guardianUI

	dataVolume   string
	backupVolume string
	tlsVolume    string
	caPEM        []byte
	resetAt      time.Time
	guardianPass string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	s := newStack()
	h := &harness{
		stack:        s,
		mail:         &mailFixture{s: s},
		guardianPass: guardianPassword,
	}
	h.dataVolume = s.volumeFor(t, "wasi", "/data")
	h.backupVolume = s.volumeFor(t, "wasi", "/backups")
	h.tlsVolume = s.volumeFor(t, "wasi", "/config/tls")

	h.reset(t)
	h.ui = newGuardianUI(t, s)
	h.ui.login(t, guardianName, h.guardianPass)
	return h
}

// reset returns the stack to a fresh install: no contacts, no state, no
// guardians, empty mailboxes, one guardian account.
//
// Wasi is SIGKILLed rather than stopped, and killed *before* anything is
// wiped. Both matter: /data is wiped while its only writer is dead, and the
// mail accounts are recreated while nothing holds an IMAP session against
// them. A graceful stop would also be wrong here for a subtler reason — it
// runs the shutdown path, so a bug that only appears after an ungraceful exit
// would be reset away before the next case could find it.
func (h *harness) reset(t *testing.T) {
	t.Helper()
	h.resetAt = time.Now().UTC().Add(-time.Second)

	h.stack.kill(t, "wasi")
	h.mail.resetAccounts(t)
	h.stack.wipeVolume(t, h.dataVolume)
	h.stack.wipeVolume(t, h.backupVolume)
	h.stack.start(t, "wasi")

	h.caPEM = h.stack.readVolumeFile(t, h.tlsVolume, "ca.crt")
	h.waitReady(t)
	h.useradd(t, guardianName, h.guardianPass)
}

// waitReady blocks until both listeners answer.
//
// Neither probe performs a sync, and that is deliberate: a sync runs §5.1's
// reconciliation pass, so a readiness check built on one would file the very
// mail V-15 is waiting to watch the *startup* pass file. The device listener
// is probed with a deliberately wrong bearer token instead, which proves it is
// listening and, incidentally, re-asserts §4.1's 401 on every reset.
func (h *harness) waitReady(t *testing.T) {
	t.Helper()

	client := newGuardianUI(t, h.stack).client
	waitFor(t, readyTimeout, "the guardian listener to report ready", func() error {
		resp, err := client.Get(guardianBaseURL + "/readyz")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("readyz: status %d", resp.StatusCode)
		}
		return nil
	})

	waitFor(t, readyTimeout, "the device listener to answer", func() error {
		return probeDeviceListener(h.caPEM)
	})
}

// restartWasi kills and restarts the server, waiting for it to come back. This
// is the crash every recovery clause is written against (§7.6, §5.1's
// "filing must not depend on uptime").
func (h *harness) restartWasi(t *testing.T) {
	t.Helper()
	h.stack.kill(t, "wasi")
	h.stack.start(t, "wasi")
	h.waitReady(t)
}

// useradd creates a guardian with `wasi useradd`, the documented path (§9.2).
// Running it against the live server is also the shape of F-8's bug: a
// separate process writing guardians.toml under a running one.
func (h *harness) useradd(t *testing.T, name, password string) {
	t.Helper()
	h.stack.exec(t, "wasi", []byte(password),
		"/usr/local/bin/wasi", "useradd", "-password-file", "-", name)
}

// contacts reads /data/ayllu.toml straight off the volume.
//
// Contact ids are server-minted and unpredictable, so a test that wants to
// deactivate "Theo" has to look the id up somewhere. This reads the file
// rather than scraping the contacts page because the id is an implementation
// detail of the fixture's bookkeeping, not part of what any V-case asserts —
// the *mutations* still go through the real UI, which is what §7.4 and §8 are
// about.
func (h *harness) contacts(t *testing.T) []fileContact {
	t.Helper()
	var ff struct {
		Version  int           `toml:"version"`
		Contacts []fileContact `toml:"contacts"`
	}
	data := h.stack.readVolumeFile(t, h.dataVolume, "ayllu.toml")
	if err := toml.Unmarshal(data, &ff); err != nil {
		t.Fatalf("parsing ayllu.toml: %v\n%s", err, data)
	}
	return ff.Contacts
}

type fileContact struct {
	ID            string   `toml:"id"`
	Name          string   `toml:"name"`
	Address       string   `toml:"address"`
	PastAddresses []string `toml:"past_addresses"`
	Active        bool     `toml:"active"`
}

// contactID returns the id of the contact with the given name, or fails.
func (h *harness) contactID(t *testing.T, name string) string {
	t.Helper()
	for _, c := range h.contacts(t) {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("no contact named %q in ayllu.toml: %+v", name, h.contacts(t))
	return ""
}

// addContact adds a contact through the UI and returns its id.
func (h *harness) addContact(t *testing.T, name, address string) string {
	t.Helper()
	h.ui.addContact(t, name, address)
	return h.contactID(t, name)
}

// serverState decodes /data/state.json, or reports nil if it does not exist
// yet. A fresh install has no state.json until something first writes one, so
// "absent" is a normal reading and not a fixture problem.
func (h *harness) serverState(t *testing.T) map[string]any {
	t.Helper()
	data, ok := h.stack.volumeFiles(t, h.dataVolume)["state.json"]
	if !ok {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parsing state.json: %v\n%s", err, data)
	}
	return out
}

// logsSinceReset returns everything wasi and strip have written during this
// test. maddy's own logs are deliberately absent: it is the mail store,
// standing in for Fastmail, and I-1 has never claimed the provider does not
// hold the letters — it claims Wasi and the shared services do not.
func (h *harness) logsSinceReset(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"wasi":  h.stack.logs(t, h.resetAt, "wasi"),
		"strip": h.stack.logs(t, h.resetAt, "strip"),
	}
}

// nonce mints a distinctive token to build letter text out of.
//
// V-11's grep is only as good as the strings it looks for. Real prose is a bad
// probe: searching /data for "tent" out of "we slept in a tent" matches
// "contents" and "non-existent" in a config file and reports a leak that is
// not there, while a short common word that *is* leaked can hide in plain
// sight. A 16-character random token appears in the mailbox and nowhere else
// unless something wrote it down, which is exactly the question.
func nonce(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("no entropy for a letter nonce: %v", err)
	}
	return "zq" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
}

// waitForHeld blocks until a message carrying mark is in the Held folder,
// syncing between polls.
//
// Two different mechanisms put a message in Held, and which one applies depends
// on the sender — a distinction that decides how this poll behaves:
//
//   - A stranger (resolves against nothing, tombstones included) is quarantined
//     by the reconciliation pass that runs at the top of every sync (§5.1). The
//     windowResync nudge below drives exactly that pass, so a stranger's mail
//     lands in Held on the first poll after the nudge — fast and deterministic,
//     no notification involved.
//   - A *deactivated* contact's new mail is a different case. Reconciliation
//     resolves the full table and quarantines strangers only (finding F-2: an
//     active-only reconcile would sweep that contact's already-delivered
//     history into Held, which V-6 forbids). So the sync nudge does nothing for
//     it — the only path that holds a deactivated contact's arrival is the IDLE
//     goroutine (§5.1, §7.2: "the decision is made once, at arrival"), which
//     examines each message while the sender's active-status is current.
//
// The nudge cannot accelerate the second case, so this waits on IDLE latency
// there. Against the maddy fixture that latency is variable and occasionally
// several tens of seconds: `maddy imap-msgs add` writes to the store directly
// and does not push the IMAP notification a real SMTP delivery would, so IDLE
// only sees the message on its next reconnect. The floor below keeps that from
// flaking. It is a fixture artifact, not a product SLA — in production IDLE
// against Fastmail is prompt. See finding F-9 for the residual gap this
// exposes: reconciliation genuinely cannot cover a deactivated arrival, so if
// IDLE never runs (Wasi down at arrival) that mail is delivered, not held.
//
// V-15 deliberately does *not* use this: its whole subject is the startup
// pass, and syncing would hide which pass did the work.
func (h *harness) waitForHeld(t *testing.T, mark string, timeout time.Duration) {
	t.Helper()
	// A deactivated arrival can only be caught by IDLE, whose fixture latency is
	// the thing being tolerated; give it room regardless of the caller's guess.
	if timeout < 90*time.Second {
		timeout = 90 * time.Second
	}
	nudge := h.newDevice(t)
	waitFor(t, timeout, "the letter marked "+mark+" to be quarantined", func() error {
		if h.mail.holds(t, childAddress, heldFolder, mark) {
			return nil
		}
		nudge.windowResync(t)
		return fmt.Errorf("not in %s yet", heldFolder)
	})
}

// mustJSON re-encodes a value for assertions that ask what the whole wire
// response contained — the I-2 leak checks, where the question is whether an
// address appears *anywhere* in it, not in a field somebody thought to look at.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encoding a response: %v", err)
	}
	return b
}

// newDevice returns a simulated Chaski with its own fresh flash.
func (h *harness) newDevice(t *testing.T) *simDevice {
	t.Helper()
	dev, err := chaskisim.Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatalf("opening a simulated device: %v", err)
	}
	return &simDevice{Device: dev, client: newDeviceClient(t, h.caPEM)}
}

// backupFiles returns everything under the backup volume. `wasi backup` is
// Wave 4B's; until it lands the volume is empty, and V-11 says so rather than
// quietly asserting over nothing.
func (h *harness) backupFiles(t *testing.T) map[string][]byte {
	t.Helper()
	return h.stack.volumeFiles(t, h.backupVolume)
}
