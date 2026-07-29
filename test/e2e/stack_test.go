//go:build e2e

// Package e2e is the end-to-end half of the verification table in
// wasi-server-plan §15: the same clauses the unit suites assert against fakes,
// re-asserted against the real stack that deploy/compose.dev.yml brings up —
// Wasi in its own container behind two TLS listeners, the Python strip
// service, and maddy standing in for Fastmail.
//
// It exists because the failure modes are different here. A unit test proving
// derivation flags `degraded` when a strip client returns an error cannot
// prove that SIGKILLing a Python container produces that error (V-10); a unit
// test proving state.json holds no bodies cannot prove that nothing else under
// /data does, nor that no line of JSON log output does (V-11); and a mocked
// write failure is not a crash (V-12). Where an e2e case would only restate
// what a unit test already pins down, it is deliberately absent and says so —
// see the "Deliberately not re-tested here" list below.
//
// # Running it
//
//	make e2e
//
// The suite brings the stack up itself and resets it between cases, so it
// never depends on a hand-prepared fixture. Two environment knobs:
//
//	WASI_E2E_DOCKER     the docker command, split on spaces. Default "docker";
//	                    set to "sudo docker" where the daemon socket needs it.
//	WASI_E2E_TEARDOWN   "1" runs `compose down -v` after the suite. Off by
//	                    default: tearing the volumes down costs a full rebuild
//	                    on the next run, and leaving them up is harmless
//	                    because every case resets what it depends on.
//
// # Deliberately not re-tested here
//
//	V-2   Window-resync cap. Seeding resync_window+1 (201) messages costs
//	      minutes of fixture time to re-assert an arithmetic bound that
//	      internal/syncsvc already covers against a fake mailbox. The real
//	      IMAP path it would add — Recent() — is exercised by every empty
//	      cursor sync in this file.
//	V-13  Pututu coalescing. The interesting parameter is a 15-minute window;
//	      an honest e2e version would have to sit through it, and internal/
//	      pututu's clock-injecting tests already pin the policy. The doorbell
//	      firing at all is asserted here (V-18) via the counter on the wire.
//	V-14  Vocabulary boundary. Re-asserted here rather than skipped, but only
//	      over the *rendered* surfaces — live HTML and real notice letters —
//	      since grepping template source is what the unit test already does.
//	V-22  `go test ./...` itself. That is `make check`, not a case this suite
//	      can meaningfully contain.
package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Fixture facts. Every one of these is fixed by deploy/compose.dev.yml,
// deploy/wasi.example.toml, or deploy/maddy/init.sh; none of them is a
// production value and none of them may become one.
const (
	deviceSyncURL   = "https://127.0.0.1:18443/sync"
	guardianBaseURL = "https://127.0.0.1:18444"
	stripHealthURL  = "http://127.0.0.1:18080/healthz"

	deviceToken = "dev-device-token"
	// pututuKey mirrors WASI_PUTUTU_KEY in compose.dev.yml. §10.2 requires it
	// to be a secret separate from the bearer token, and the suite needs it
	// for the same reason a device does: to verify a doorbell token's MAC.
	pututuKey = "dev-pututu-hmac-key-not-for-prod"

	childAddress = "kid@chaski.test"
	// relativeAddress is the far end of every outbound letter. It must be a
	// different mailbox from the child's: outbound resolves through the ayllu
	// to somebody else by construction, so the child's own account cannot
	// stand in for it (V-1).
	relativeAddress = "theo@chaski.test"
	// strangerAddress is nobody's contact. It is a *local* account only so
	// that "no bounce, ever" (I-3, V-5) is checkable rather than assumed: a
	// bounce would have to land somewhere, and this is the somewhere.
	strangerAddress = "stranger@chaski.test"

	inboxFolder = "INBOX"
	heldFolder  = "Held"
	spamFolder  = "Junk"

	guardianName     = "dad"
	guardianPassword = "e2e-guardian-password-not-for-prod"
)

// Command timeouts. Generous, because a loaded CI box building an image is not
// a failing stack; short enough that a wedged container fails the run rather
// than hanging it.
const (
	composeTimeout = 3 * time.Minute
	buildTimeout   = 15 * time.Minute
	readyTimeout   = 90 * time.Second
	pollInterval   = 250 * time.Millisecond
)

// stack drives deploy/compose.dev.yml. It shells out rather than using a
// Docker API client on purpose: the commands below are the ones a human runs
// against this fixture, and a suite that drives the stack through a different
// mechanism than the README documents can pass while the documented path is
// broken.
type stack struct {
	docker  []string // e.g. {"docker"} or {"sudo", "docker"}
	compose []string // docker + {"compose", "-f", <compose file>}
	root    string   // repository root
}

func newStack() *stack {
	root := repoRoot()
	docker := strings.Fields(envOr("WASI_E2E_DOCKER", "docker"))
	compose := append(append([]string{}, docker...), "compose", "-f", filepath.Join(root, "deploy", "compose.dev.yml"))
	return &stack{docker: docker, compose: compose, root: root}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// repoRoot locates the repository from this file's own compiled-in path, so
// the suite does not depend on the working directory `go test` chose.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("e2e: cannot locate this file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// run executes argv with stdin attached and returns stdout. argv is always an
// argument list, never a shell string: nothing here interpolates a test-chosen
// value into something a shell will parse.
func (s *stack) run(timeout time.Duration, stdin []byte, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("e2e: %s: %w (stderr: %s)",
			strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// composeCmd runs one `docker compose` subcommand and fails the test on error.
func (s *stack) composeCmd(t testing.TB, timeout time.Duration, args ...string) []byte {
	t.Helper()
	out, err := s.run(timeout, nil, append(append([]string{}, s.compose...), args...)...)
	if err != nil {
		t.Fatalf("compose %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// up brings the whole stack to a running, healthy state. Idempotent: an
// already-running stack is left alone, which is what makes `make up && make
// e2e` and a bare `go test -tags e2e` behave identically.
//
// It returns an error rather than taking a testing.TB because TestMain calls
// it, and TestMain has no test to fail.
func (s *stack) up() error {
	_, err := s.run(buildTimeout, nil, append(append([]string{}, s.compose...), "up", "-d", "--build")...)
	return err
}

func (s *stack) down() error {
	_, err := s.run(composeTimeout, nil, append(append([]string{}, s.compose...), "down", "-v")...)
	return err
}

// kill stops a service the way a power cut does. Every crash-consistency case
// in this suite (V-10, V-12, V-15, V-17) uses this and never `stop`: SIGTERM
// gives Wasi its shutdown grace and its deferred cleanup, which is precisely
// the code path a crash does not run.
func (s *stack) kill(t testing.TB, service string) {
	t.Helper()
	s.composeCmd(t, composeTimeout, "kill", "-s", "KILL", service)
}

func (s *stack) start(t testing.TB, service string) {
	t.Helper()
	s.composeCmd(t, composeTimeout, "start", service)
}

// exec runs a command inside a running service container.
func (s *stack) exec(t testing.TB, service string, stdin []byte, args ...string) []byte {
	t.Helper()
	argv := append(append([]string{}, s.compose...), "exec", "-T", service)
	argv = append(argv, args...)
	out, err := s.run(composeTimeout, stdin, argv...)
	if err != nil {
		t.Fatalf("exec in %s: %v", service, err)
	}
	return out
}

// logs returns everything a service has written since `since`. This is the
// capture V-11's log grep runs over — the actual container output an operator
// would see, not a logger the test wired up itself.
func (s *stack) logs(t testing.TB, since time.Time, service string) string {
	t.Helper()
	out := s.composeCmd(t, composeTimeout,
		"logs", "--no-color", "--no-log-prefix", "--since", since.UTC().Format(time.RFC3339), service)
	return string(out)
}

// volumeFor reports the named volume backing dest inside service's container.
// Resolved rather than hardcoded because the volume name carries the compose
// project name, which is the directory the stack was brought up from.
func (s *stack) volumeFor(t testing.TB, service, dest string) string {
	t.Helper()
	format := fmt.Sprintf(`{{range .Mounts}}{{if eq .Destination %q}}{{.Name}}{{end}}{{end}}`, dest)
	argv := append(append([]string{}, s.compose...), "ps", "-q", service)
	idOut, err := s.run(composeTimeout, nil, argv...)
	if err != nil {
		t.Fatalf("locating container for %s: %v", service, err)
	}
	id := strings.TrimSpace(string(idOut))
	if id == "" {
		t.Fatalf("no container for service %s; is the stack up?", service)
	}

	argv = append(append([]string{}, s.docker...), "inspect", id, "--format", format)
	out, err := s.run(composeTimeout, nil, argv...)
	if err != nil {
		t.Fatalf("inspecting %s: %v", service, err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		t.Fatalf("service %s has no named volume at %s", service, dest)
	}
	return name
}

// helperImage is the throwaway image used to read and wipe volumes. It is
// already pulled by the stack itself (compose.dev.yml's cert jobs use it), so
// this costs no extra network.
const helperImage = "alpine:3.20"

// wipeVolume empties a named volume. Used only by reset, and only while the
// container that owns the volume is dead.
func (s *stack) wipeVolume(t testing.TB, volume string) {
	t.Helper()
	argv := append(append([]string{}, s.docker...),
		"run", "--rm", "-v", volume+":/v", helperImage,
		"sh", "-c", "rm -rf /v/..?* /v/.[!.]* /v/*")
	if _, err := s.run(composeTimeout, nil, argv...); err != nil {
		t.Fatalf("wiping volume %s: %v", volume, err)
	}
}

// volumeFiles returns every regular file in a named volume, keyed by path
// relative to the volume root.
//
// It reads the volume through a helper container rather than asking Wasi for
// anything, which is the whole point of V-11: the question is what is on disk,
// not what the process admits to having written. A tar stream is used because
// it is the only way to get *every* file, including ones no test knew to look
// for — a storage invariant checked against a list of expected filenames would
// pass the day a new file starts holding a body.
func (s *stack) volumeFiles(t testing.TB, volume string) map[string][]byte {
	t.Helper()
	argv := append(append([]string{}, s.docker...),
		"run", "--rm", "-v", volume+":/v", helperImage,
		"tar", "cf", "-", "-C", "/v", ".")
	out, err := s.run(composeTimeout, nil, argv...)
	if err != nil {
		t.Fatalf("reading volume %s: %v", volume, err)
	}

	files := make(map[string][]byte)
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading tar of volume %s: %v", volume, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s from volume %s: %v", hdr.Name, volume, err)
		}
		files[filepath.Clean(hdr.Name)] = data
	}
	return files
}

// readVolumeFile returns one file from a named volume, or fails.
func (s *stack) readVolumeFile(t testing.TB, volume, path string) []byte {
	t.Helper()
	argv := append(append([]string{}, s.docker...),
		"run", "--rm", "-v", volume+":/v", helperImage, "cat", filepath.Join("/v", path))
	out, err := s.run(composeTimeout, nil, argv...)
	if err != nil {
		t.Fatalf("reading %s from volume %s: %v", path, volume, err)
	}
	return out
}

// writeVolumeFile replaces one file in a named volume. Used by V-20 to put an
// older state.json back, which is the whole of §3's restore story.
func (s *stack) writeVolumeFile(t testing.TB, volume, path string, data []byte) {
	t.Helper()
	argv := append(append([]string{}, s.docker...),
		"run", "--rm", "-i", "-v", volume+":/v", helperImage,
		"sh", "-c", "cat > "+filepath.Join("/v", path)+" && chown 65532:65532 "+filepath.Join("/v", path))
	if _, err := s.run(composeTimeout, data, argv...); err != nil {
		t.Fatalf("writing %s to volume %s: %v", path, volume, err)
	}
}

// waitFor polls until fn returns nil or the deadline passes, reporting the
// last failure. Every "eventually" in this suite goes through here: the stack
// is asynchronous (IDLE, a backstop ticker, a container restart), and a fixed
// sleep is either flaky or slow.
func waitFor(t testing.TB, timeout time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = fn()
		if last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
		}
		time.Sleep(pollInterval)
	}
}
