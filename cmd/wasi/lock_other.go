//go:build !unix

package main

import "errors"

// acquireDataLock has no advisory-lock primitive outside unix. The
// deployment target is a Linux container (§14); this stub exists only to
// keep `go build ./...` honest on other platforms, the same reasoning as
// promptPassword's non-unix stub in password_other.go. Since there is no
// safe way to honor the two-writers protection this lock exists to provide
// on this platform, it refuses outright rather than silently running
// unprotected.
func acquireDataLock(dataDir string) (release func(), err error) {
	return nil, errors.New("wasi: the data-directory lock is unix-only; refusing to run without it")
}
