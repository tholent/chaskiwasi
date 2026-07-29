//go:build unix

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// promptPassword reads one line from the terminal with echo disabled.
//
// Echo suppression is not decoration here: `wasi useradd` is run at a console
// that, in the household this server is designed for, may well be in a shared
// room. A password typed in the clear on a screen someone else can see defeats
// the point of hashing it at all.
//
// When stdin is not a terminal, the password is read plainly — that is the
// documented automation path (`wasi useradd -password-file -`), and there is
// no echo to suppress.
func promptPassword(prompt string) (string, error) {
	restore, err := disableEcho(int(os.Stdin.Fd()))
	if err == nil {
		defer restore()
	}
	// The prompt goes to stderr so stdout stays usable for anything scripted
	// around this command.
	fmt.Fprint(os.Stderr, prompt)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("useradd: reading the password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// disableEcho clears ECHO on fd and returns a function restoring the previous
// terminal state. A non-terminal fd returns an error, which the caller treats
// as "nothing to suppress" rather than as a failure.
func disableEcho(fd int) (restore func(), err error) {
	previous, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}

	quiet := *previous
	quiet.Lflag &^= unix.ECHO
	// ECHONL keeps the user's Return visible, so the cursor still moves off
	// the prompt line even though the password itself is hidden.
	quiet.Lflag |= unix.ECHONL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return nil, err
	}

	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, previous) }, nil
}
