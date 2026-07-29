//go:build !unix

package main

import "errors"

// promptPassword has no terminal-echo control outside unix. Rather than
// prompting and echoing the password to a screen, this build refuses and
// points at the flag that does not need a terminal at all — the deployment
// target is a Linux container (§14), so this path exists only to keep
// `go build ./...` honest on other platforms.
func promptPassword(string) (string, error) {
	return "", errors.New("useradd: interactive password entry is unix-only; use -password-file")
}
