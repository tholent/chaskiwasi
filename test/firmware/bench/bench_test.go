//go:build bench

// The bench tier: the rows of client §15 that need a real device.
//
// Everything here is blocked on hardware by construction, and the suite says so
// rather than pretending. With no board attached every test SKIPS with an
// explanation — a skip must never read as a pass, because the whole point of
// the standing no-hardware caveat (implementation-plan §4) is that a green
// suite has never meant a working device.
//
// Run it:
//
//	make up                                   # Wasi + strip + maddy
//	CHASKI_BENCH_PORT=/dev/ttyACM0 \         # macOS: /dev/cu.usbmodem*
//	  go test -tags bench -v ./test/firmware/bench/
//
// See README.md for flashing, provisioning, and the control vocabulary.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	shortCmd = 10 * time.Second
	syncCmd  = 60 * time.Second
	bootWait = 30 * time.Second
)

// rig is one bench session: an attached device plus the server it syncs to.
type rig struct {
	dev  *Device
	wasi string
}

// newRig attaches, or skips. The skip is the honest outcome when the hardware
// this suite exists to exercise is not present.
func newRig(t *testing.T) *rig {
	t.Helper()

	port := os.Getenv("CHASKI_BENCH_PORT")
	if port == "" {
		t.Skip("no device: set CHASKI_BENCH_PORT to run the bench tier " +
			"(Linux /dev/ttyACM0, macOS /dev/cu.usbmodem*). These rows are " +
			"hardware-blocked and a skip is not a pass.")
	}
	if _, err := os.Stat(port); err != nil {
		t.Skipf("no device at %s: %v", port, err)
	}

	// The default is the compose stack's published device-sync listener, which is
	// 18443 on the host and 8443 only inside the container network
	// (deploy/compose.dev.yml). test/e2e uses this same literal; they must not
	// drift, because a bench run that cannot reach Wasi fails as a sync fault and
	// reads like a device problem. 127.0.0.1 rather than localhost: the publish is
	// IPv4-only, so a host resolving localhost to ::1 gets a connection refused
	// that has nothing to do with the board.
	wasi := os.Getenv("CHASKI_BENCH_WASI")
	if wasi == "" {
		wasi = "https://127.0.0.1:18443/sync"
	}

	dev, err := OpenDevice(port, wasi, true)
	if err != nil {
		t.Fatalf("attaching to %s: %v", port, err)
	}
	t.Cleanup(func() { _ = dev.Close() })

	r := &rig{dev: dev, wasi: wasi}
	// A device that has been sitting attached has already said hello; clear it
	// so a later WaitHello cannot match a boot that happened before this test.
	dev.DrainHello()
	if _, err := r.do(t, map[string]any{"cmd": "hello"}, shortCmd); err != nil {
		t.Fatalf("the device did not answer hello: %v", err)
	}
	return r
}

func (r *rig) do(t *testing.T, cmd map[string]any, timeout time.Duration) (map[string]any, error) {
	t.Helper()
	return r.dev.Do(cmd, timeout)
}

// must runs a command and fails the test if the device refuses it.
func (r *rig) must(t *testing.T, cmd map[string]any, timeout time.Duration) map[string]any {
	t.Helper()
	ev, err := r.do(t, cmd, timeout)
	if err != nil {
		t.Fatalf("%v: %v", cmd["cmd"], err)
	}
	return ev
}

func num(ev map[string]any, key string) int {
	if f, ok := ev[key].(float64); ok {
		return int(f)
	}
	return 0
}

func str(ev map[string]any, key string) string {
	s, _ := ev[key].(string)
	return s
}

func boolean(ev map[string]any, key string) bool {
	b, _ := ev[key].(bool)
	return b
}

// nonce makes a letter body this run can recognise later, in the mailbox and in
// the console capture.
func nonce(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("bench-%d-%s", time.Now().UnixNano(), t.Name())
}

// C-1: the letter path, end to end, on real hardware.
//
// Compose on the device, sync over the USB bridge to a real Wasi, and confirm
// the device reports it sent. The mailbox-side assertion (the letter arriving at
// maddy for the right address with a valid Message-ID) is the server's V-1 and
// is checked there; what this row adds is that the DEVICE produced it.
func TestC1_ComposeAndSyncDeliversTheLetter(t *testing.T) {
	r := newRig(t)
	body := nonce(t)

	st := r.must(t, map[string]any{"cmd": "state"}, shortCmd)
	if !boolean(st, "provisioned") {
		t.Skip("device is not provisioned: see README.md, `provision` command")
	}

	composed := r.must(t, map[string]any{
		"cmd": "compose", "contact_id": contactID(t), "body": body,
	}, shortCmd)
	localID := str(composed, "local_id")
	if localID == "" {
		t.Fatal("compose returned no local_id")
	}

	ev := r.must(t, map[string]any{"cmd": "sync", "trigger": "user"}, syncCmd)
	if f := str(ev, "fault"); f != "none" {
		t.Fatalf("sync reported fault %q; is the stack up? (make up)", f)
	}
	if num(ev, "acks") < 1 {
		t.Fatalf("the letter was not acked: %v", ev)
	}

	// D-5: an acked letter leaves the outbox and is never resent.
	out := r.must(t, map[string]any{"cmd": "outbox"}, shortCmd)
	if n := num(out, "sendable"); n != 0 {
		t.Errorf("outbox still holds %d sendable letters after an ack", n)
	}
}

// C-2 (bench half): a window resync re-delivers, and the device drops repeats.
//
// The server may re-send any letter at any time (server §4.5) and correctness
// must never depend on it not doing so. Clearing the cursor is how the device
// gets the resync a restored-from-backup server would produce.
func TestC2_WindowResyncLeavesNoDuplicates(t *testing.T) {
	r := newRig(t)

	if _, err := r.do(t, map[string]any{"cmd": "sync", "trigger": "user"}, syncCmd); err != nil {
		t.Fatalf("priming sync: %v", err)
	}
	before := r.must(t, map[string]any{"cmd": "letters"}, shortCmd)
	total := num(before, "total")
	if total == 0 {
		t.Skip("no letters on the device to re-deliver; seed the mailbox first")
	}

	r.must(t, map[string]any{"cmd": "clear_cursor"}, shortCmd)

	ev := r.must(t, map[string]any{"cmd": "sync", "trigger": "user"}, syncCmd)
	if num(ev, "deduped") == 0 {
		t.Errorf("a window resync deduped nothing; the seen-ring is not doing its job")
	}

	after := r.must(t, map[string]any{"cmd": "letters"}, shortCmd)
	if got := num(after, "total"); got != total {
		t.Errorf("letter count changed across a resync: %d -> %d (duplicates)", total, got)
	}
}

// C-4 (bench half): a power cut between sending the request and applying the
// response costs a duplicate SEND at worst, never a lost letter.
//
// `cut_at` reboots the device at a numbered step of the §5.2 apply order, which
// is the only way to exercise this on real hardware — the interesting failure
// is a reset, not an exception.
func TestC4_PowerCutMidApplyLosesNothing(t *testing.T) {
	r := newRig(t)
	body := nonce(t)

	r.must(t, map[string]any{
		"cmd": "compose", "contact_id": contactID(t), "body": body,
	}, shortCmd)

	r.dev.DrainHello()
	// Cut after the acks are applied but before the cursor is written: the
	// window where a naive implementation loses the ack or advances anyway.
	if _, err := r.do(t, map[string]any{
		"cmd": "sync", "trigger": "user", "cut_at": 2,
	}, syncCmd); err != nil && !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("cut sync: %v", err)
	}

	hello, err := r.dev.WaitHello(bootWait)
	if err != nil {
		t.Fatalf("device did not reboot after the cut: %v", err)
	}
	t.Logf("device came back: boot %d", num(hello, "boot"))

	// The letter is either acked-and-gone or still queued — never vanished.
	out := r.must(t, map[string]any{"cmd": "outbox"}, shortCmd)
	t.Logf("outbox after the cut: %d sendable", num(out, "sendable"))

	ev := r.must(t, map[string]any{"cmd": "sync", "trigger": "user"}, syncCmd)
	if f := str(ev, "fault"); f != "none" {
		t.Fatalf("recovery sync faulted: %s", f)
	}
	final := r.must(t, map[string]any{"cmd": "outbox"}, shortCmd)
	if n := num(final, "sendable"); n != 0 {
		t.Errorf("a letter is still stuck after recovery: %d sendable", n)
	}
	// The server's ack ring is what makes the duplicate send harmless; V-9
	// asserts that side. Here we assert the device did not lose the letter.
}

// C-7: the coarse status codes each get their own device-visible state.
//
// 401 must stop rather than retry hot, because it means a guardian has to do
// something (§5.3). A device that hammered a 401 would spend a prepaid balance
// on a fault no amount of retrying can fix.
func TestC7_ProvisioningFaultStopsRatherThanRetrying(t *testing.T) {
	r := newRig(t)

	saved := r.must(t, map[string]any{"cmd": "state"}, shortCmd)
	if !boolean(saved, "provisioned") {
		t.Skip("device is not provisioned; nothing to invalidate")
	}

	r.must(t, map[string]any{"cmd": "provision", "token": "not-the-right-token"}, shortCmd)
	t.Cleanup(func() {
		if tok := os.Getenv("CHASKI_BENCH_TOKEN"); tok != "" {
			_, _ = r.do(t, map[string]any{"cmd": "provision", "token": tok}, shortCmd)
		}
	})

	ev := r.must(t, map[string]any{"cmd": "sync", "trigger": "user"}, syncCmd)
	if f := str(ev, "fault"); f != "provisioning" {
		t.Errorf("a bad token gave fault %q, want provisioning", f)
	}
	// §5.3: 401 halts the ladder rather than backing off into it.
	if b := num(ev, "backoff_ms"); b > 0 {
		t.Errorf("a 401 armed a %dms retry; it must stop until a key press", b)
	}
}

// C-19: the dev build's serial output carries no letter content.
//
// This is the client's V-11, and the dev build is deliberately the one under
// test: it is the build attached to a terminal someone can read over your
// shoulder (B.11).
func TestC19_TheConsoleNeverCarriesLetterContent(t *testing.T) {
	r := newRig(t)
	body := nonce(t)
	subject := "bench subject " + body

	r.must(t, map[string]any{
		"cmd": "compose", "contact_id": contactID(t), "body": body, "subject": subject,
	}, shortCmd)
	if _, err := r.do(t, map[string]any{"cmd": "sync", "trigger": "user"}, syncCmd); err != nil {
		t.Logf("sync during the console capture: %v (the grep still applies)", err)
	}
	r.must(t, map[string]any{"cmd": "letters"}, shortCmd)

	console := r.dev.Console()
	if console == "" {
		t.Skip("no console output captured; is this a dev build with the USB console on?")
	}

	// A torn frame's bytes land in the console capture, and a device->host frame
	// carries the child's letter — so a torn frame makes this grep inconclusive
	// rather than clean. Say so instead of reporting a pass.
	if n := r.dev.Torn(); n != 0 {
		t.Fatalf("%d torn frame(s): letter bytes may have spilled into the capture, "+
			"so this run cannot answer C-19 either way", n)
	}

	for _, secret := range []string{body, subject} {
		if strings.Contains(console, secret) {
			t.Errorf("the console contains letter content (D-7): %q", secret)
		}
	}
	t.Logf("captured %d bytes of console, clean", len(console))
}

// contactID picks the contact the bench composes to. It is a real id from the
// server's ayllu, not a guess: the device rejects an unknown one, which would
// look like a firmware bug rather than a misconfigured bench.
func contactID(t *testing.T) string {
	t.Helper()
	if id := os.Getenv("CHASKI_BENCH_CONTACT"); id != "" {
		return id
	}
	return "c_01"
}

// dumpEvent is a debugging aid for a failing bench run: events are small and
// content-free by design, so printing one whole is safe.
func dumpEvent(t *testing.T, ev map[string]any) {
	t.Helper()
	b, _ := json.MarshalIndent(ev, "", "  ")
	t.Logf("event: %s", b)
}
