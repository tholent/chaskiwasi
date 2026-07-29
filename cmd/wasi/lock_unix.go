//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquireDataLock takes an exclusive, non-blocking advisory lock on
// dataDir/.wasi.lock and returns a func that releases it.
//
// This is what turns wasi-server-plan §3's "ayllu.toml is hand-editable
// only while Wasi is stopped" from an instruction the operator has to
// remember into a fact `wasi contacts` can check. `serve` takes this same
// lock for its entire run (see runServe in serve.go) and holds it until the
// process exits; `contacts` takes it only for the duration of one
// invocation and refuses outright — rather than proceed — if it can't.
//
// Refusing, not racing, is the deliberate choice for contacts specifically.
// guardians.toml had the identical two-writers shape (F-8 in
// implementation-plan.md: `wasi useradd` wrote the file while the running
// server held a stale copy, and the server's next write deleted the account
// the CLI had just added) and was fixed the other way, by making
// guardians.FileStore re-read the file when it changes. That fix works
// there because a guardian account carries no announcement obligation.
// ayllu.toml's mutations do (I-4: every add/deactivate/reactivate/readdress
// gets a notice letter), and re-reading the file on the server side would
// still leave the running server's notice machinery with no way to learn a
// CLI-made change happened at all — re-reading fixes staleness, not
// silence. So contacts takes the option F-8 didn't: detect the running
// server and refuse, rather than build a second, harder concurrency story
// for a file whose own doc comment already says "stopped only."
//
// flock(2) rather than a PID file: the kernel releases the lock the instant
// the holding process exits for any reason, including SIGKILL, so there is
// no stale-lock cleanup path to get wrong — the problem a PID file always
// eventually has to solve badly. It works across two different containers
// sharing the same Docker volume because, underneath, they are the same
// host directory on the same kernel; that is this server's documented
// deployment (§14), not a coincidence relied on by accident. It is not
// guaranteed to work over a network filesystem without lockd support — not
// a concern for the documented deployment.
func acquireDataLock(dataDir string) (release func(), err error) {
	path := filepath.Join(dataDir, dataLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errDataDirBusy
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	return func() { f.Close() }, nil // closing the fd releases the flock
}
