package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tholent/chaskiwasi/internal/guardians"
)

func writePasswordFile(t *testing.T, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pw")
	// A trailing newline is what `echo` and every editor produce; useradd must
	// not silently make it part of the password.
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	return path
}

func TestUserAdd(t *testing.T) {
	const password = "a perfectly good passphrase"

	dataDir := t.TempDir()
	pwFile := writePasswordFile(t, password)

	if err := runUserAdd([]string{"-data", dataDir, "-password-file", pwFile, "Dad"}); err != nil {
		t.Fatalf("useradd: %v", err)
	}

	store, err := guardians.Open(dataDir)
	if err != nil {
		t.Fatalf("guardians.Open: %v", err)
	}
	g, ok := store.Get("dad")
	if !ok {
		t.Fatal("the guardian was not written")
	}
	if g.SessionEpoch != 1 {
		t.Errorf("SessionEpoch = %d, want 1", g.SessionEpoch)
	}
	if _, err := store.Verify("dad", password); err != nil {
		t.Fatalf("the written password does not verify (trailing newline eaten?): %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dataDir, "guardians.toml"))
	if err != nil {
		t.Fatalf("read guardians.toml: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatal("the password is stored in cleartext")
	}
}

func TestUserAdd_Errors(t *testing.T) {
	dataDir := t.TempDir()
	good := writePasswordFile(t, "a perfectly good passphrase")

	tests := []struct {
		name string
		args []string
	}{
		{"no guardian name", []string{"-data", dataDir, "-password-file", good}},
		{"two guardian names", []string{"-data", dataDir, "-password-file", good, "dad", "mum"}},
		{"invalid name", []string{"-data", dataDir, "-password-file", good, "aunt rosa"}},
		{"short password", []string{"-data", dataDir, "-password-file", writePasswordFile(t, "short"), "dad"}},
		{"reset of an unknown guardian", []string{"-data", dataDir, "-password-file", good, "-reset", "nobody"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := runUserAdd(tc.args); err == nil {
				t.Fatal("runUserAdd accepted an invalid invocation")
			}
		})
	}
}

func TestUserAdd_RefusesToOverwriteWithoutReset(t *testing.T) {
	// Silently resetting an existing guardian's password from a shell is
	// exactly what a hostile household member would reach for.
	dataDir := t.TempDir()
	first := writePasswordFile(t, "a perfectly good passphrase")
	second := writePasswordFile(t, "a completely different one")

	if err := runUserAdd([]string{"-data", dataDir, "-password-file", first, "dad"}); err != nil {
		t.Fatalf("first useradd: %v", err)
	}
	err := runUserAdd([]string{"-data", dataDir, "-password-file", second, "dad"})
	if err == nil {
		t.Fatal("useradd overwrote an existing guardian")
	}
	if !strings.Contains(err.Error(), "-reset") {
		t.Errorf("error %q does not point at the flag that does this on purpose", err)
	}

	store, _ := guardians.Open(dataDir)
	if _, err := store.Verify("dad", "a perfectly good passphrase"); err != nil {
		t.Error("the original password stopped working")
	}
}

func TestUserAdd_ResetBumpsTheEpoch(t *testing.T) {
	dataDir := t.TempDir()
	first := writePasswordFile(t, "a perfectly good passphrase")
	second := writePasswordFile(t, "a completely different one")

	if err := runUserAdd([]string{"-data", dataDir, "-password-file", first, "dad"}); err != nil {
		t.Fatalf("useradd: %v", err)
	}
	if err := runUserAdd([]string{"-data", dataDir, "-password-file", second, "-reset", "dad"}); err != nil {
		t.Fatalf("useradd -reset: %v", err)
	}

	store, err := guardians.Open(dataDir)
	if err != nil {
		t.Fatalf("guardians.Open: %v", err)
	}
	g, _ := store.Get("dad")
	if g.SessionEpoch != 2 {
		t.Errorf("SessionEpoch after reset = %d, want 2 (every session must be ended)", g.SessionEpoch)
	}
	if _, err := store.Verify("dad", "a completely different one"); err != nil {
		t.Errorf("the reset password does not verify: %v", err)
	}
}
