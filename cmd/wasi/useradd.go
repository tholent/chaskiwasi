package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tholent/chaskiwasi/internal/guardians"
)

// runUserAdd implements `wasi useradd` (§9.2, §14): create a guardian account
// in /data/guardians.toml, with an argon2id hash and a fresh session epoch.
//
// This is also the recovery path. Nothing in the web UI can reset another
// guardian's password — a guardian who could would be able to lock a co-parent
// out of the record of their own child's contact list, which is the
// hostile-household case §9.2 is written against. `-reset` therefore requires
// access to the host, which is the boundary that makes it safe to have at all.
func runUserAdd(args []string) error {
	fs := flag.NewFlagSet("useradd", flag.ContinueOnError)
	dataDir := fs.String("data", defaultDataDir, "path to the server-owned data directory")
	passwordFile := fs.String("password-file", "",
		`read the password from this file ("-" for stdin) instead of prompting`)
	reset := fs.Bool("reset", false,
		"set the password of an existing guardian, ending all of their signed-in sessions")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: wasi useradd [flags] <name>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("useradd: exactly one guardian name is required")
	}
	name := fs.Arg(0)

	store, err := guardians.Open(*dataDir)
	if err != nil {
		return err
	}

	password, err := readPassword(*passwordFile, *reset)
	if err != nil {
		return err
	}

	if *reset {
		g, err := store.SetPassword(name, password)
		if err != nil {
			return fmt.Errorf("useradd: %w", err)
		}
		fmt.Printf("Password set for %q in %s.\n", g.Name, store.Path())
		fmt.Println("Every browser signed in as this guardian has been signed out.")
		return nil
	}

	g, err := store.Add(name, password)
	if err != nil {
		if errors.Is(err, guardians.ErrExists) {
			return fmt.Errorf("useradd: %w (use -reset to set a new password)", err)
		}
		return fmt.Errorf("useradd: %w", err)
	}
	fmt.Printf("Guardian %q created in %s.\n", g.Name, store.Path())
	fmt.Println("Sign in on the guardian listener — reachable from your home network or a VPN.")
	return nil
}

// readPassword gets the password from a file, from stdin, or from the
// terminal with echo disabled. Nothing here writes the password anywhere but
// the KDF: it is never logged, never echoed back, and never placed in a
// command-line argument, which is the one place a password would be readable
// by every other process on the machine.
func readPassword(passwordFile string, reset bool) (string, error) {
	if passwordFile != "" {
		return readPasswordFile(passwordFile)
	}

	prompt := "New password: "
	if reset {
		prompt = "New password for this guardian: "
	}
	first, err := promptPassword(prompt)
	if err != nil {
		return "", err
	}
	second, err := promptPassword("Repeat password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("useradd: the two passwords did not match")
	}
	return first, nil
}

func readPasswordFile(path string) (string, error) {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(bufio.NewReader(os.Stdin))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("useradd: reading the password: %w", err)
	}
	// One line, trailing newline trimmed: a file produced by `echo` or an
	// editor has one and it is not part of the password.
	password, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimRight(password, "\r"), nil
}
