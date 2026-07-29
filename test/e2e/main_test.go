//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
)

// TestMain brings the stack up before anything runs and, optionally, tears it
// down afterwards.
//
// Bringing it up here rather than trusting `make e2e` to have done it is what
// makes this suite a gate: a run that only passes against a stack somebody
// prepared by hand proves nothing about a clean machine, which is the machine
// the next contributor has. `compose up -d --build` is idempotent, so this
// costs nothing when the stack is already running.
//
// Teardown is opt-in (WASI_E2E_TEARDOWN=1) because `down -v` discards the
// volumes and makes the next run pay for a full rebuild, while leaving them up
// is harmless: every case resets the state it depends on.
//
// Nothing in this package may call t.Parallel(). There is one stack, one mail
// store and one /data; two cases resetting them at once would produce failures
// belonging to neither.
func TestMain(m *testing.M) {
	s := newStack()

	if err := s.up(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: bringing the stack up failed: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if os.Getenv("WASI_E2E_TEARDOWN") == "1" {
		if err := s.down(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: tearing the stack down failed: %v\n", err)
		}
	}
	os.Exit(code)
}
