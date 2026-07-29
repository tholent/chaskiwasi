package guardians

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPassword = "correct horse battery staple"

func newStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestOpen_MissingFileIsAnEmptyTable(t *testing.T) {
	// A fresh deployment has no guardians.toml until `wasi useradd` runs;
	// that must not be a startup failure.
	s := newStore(t)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("List() on a fresh store = %v, want empty", got)
	}
}

func TestAdd(t *testing.T) {
	tests := []struct {
		name     string
		guardian string
		password string
		wantErr  error
	}{
		{"plain name", "dad", testPassword, nil},
		{"name is case-folded", "DAD", testPassword, nil},
		{"dots dashes underscores", "aunt.rosa_2-b", testPassword, nil},
		{"empty name", "", testPassword, ErrInvalidName},
		{"space in name", "aunt rosa", testPassword, ErrInvalidName},
		{"name too long", strings.Repeat("a", 33), testPassword, ErrInvalidName},
		{"short password", "dad", "hunter2", ErrWeakPassword},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			g, err := s.Add(tc.guardian, tc.password)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Add(%q) error = %v, want %v", tc.guardian, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if g.SessionEpoch != 1 {
				t.Errorf("new guardian SessionEpoch = %d, want 1", g.SessionEpoch)
			}
			if g.Name != strings.ToLower(tc.guardian) {
				t.Errorf("stored name = %q, want %q", g.Name, strings.ToLower(tc.guardian))
			}
			if strings.Contains(g.PasswordHash, tc.password) {
				t.Fatal("the password appears verbatim in the stored hash")
			}
			if !strings.HasPrefix(g.PasswordHash, "$argon2id$v=19$") {
				t.Errorf("hash = %q, want a PHC-encoded argon2id string", g.PasswordHash)
			}
		})
	}
}

func TestAdd_RefusesToOverwriteAnExistingGuardian(t *testing.T) {
	// Silently resetting an existing guardian's password is exactly the move a
	// hostile household member would want from `useradd`.
	s := newStore(t)
	if _, err := s.Add("dad", testPassword); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Add("dad", "a completely different password"); !errors.Is(err, ErrExists) {
		t.Fatalf("second Add error = %v, want ErrExists", err)
	}
	if _, err := s.Verify("dad", testPassword); err != nil {
		t.Fatalf("the original password stopped working: %v", err)
	}
}

func TestVerify(t *testing.T) {
	s := newStore(t)
	if _, err := s.Add("dad", testPassword); err != nil {
		t.Fatalf("Add: %v", err)
	}

	tests := []struct {
		name     string
		guardian string
		password string
		wantErr  error
	}{
		{"correct", "dad", testPassword, nil},
		{"correct, name case-folded", "DAD", testPassword, nil},
		{"wrong password", "dad", testPassword + "!", ErrBadCredentials},
		{"unknown guardian", "stranger", testPassword, ErrBadCredentials},
		{"invalid name shape", "not a name", testPassword, ErrBadCredentials},
		{"empty password", "dad", "", ErrBadCredentials},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Verify(tc.guardian, tc.password)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify(%q) error = %v, want %v", tc.guardian, err, tc.wantErr)
			}
		})
	}
}

func TestVerify_UnknownNameIsIndistinguishableFromAWrongPassword(t *testing.T) {
	// Not a timing assertion — those are flaky in CI — but the property that
	// makes the timing right: both paths return the identical error value, so
	// no caller can branch on which one happened.
	s := newStore(t)
	if _, err := s.Add("dad", testPassword); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, unknown := s.Verify("nobody", testPassword)
	_, wrong := s.Verify("dad", "wrong password entirely")
	if unknown.Error() != wrong.Error() {
		t.Fatalf("unknown-name error %q differs from wrong-password error %q", unknown, wrong)
	}
}

// TestV19_PasswordChangeBumpsTheEpoch is the guardians half of V-19; the
// cookie-rejection half lives in internal/web.
func TestV19_PasswordChangeBumpsTheEpoch(t *testing.T) {
	s := newStore(t)
	before, err := s.Add("dad", testPassword)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	after, err := s.SetPassword("dad", "a brand new passphrase")
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if after.SessionEpoch != before.SessionEpoch+1 {
		t.Fatalf("SessionEpoch after change = %d, want %d", after.SessionEpoch, before.SessionEpoch+1)
	}
	if !after.PasswordChangedAt.After(before.PasswordChangedAt) &&
		!after.PasswordChangedAt.Equal(before.PasswordChangedAt) {
		t.Errorf("PasswordChangedAt went backwards")
	}
	if _, err := s.Verify("dad", testPassword); !errors.Is(err, ErrBadCredentials) {
		t.Error("the old password still verifies after a change")
	}
	if _, err := s.Verify("dad", "a brand new passphrase"); err != nil {
		t.Errorf("the new password does not verify: %v", err)
	}
}

func TestSetPassword_Errors(t *testing.T) {
	s := newStore(t)
	if _, err := s.Add("dad", testPassword); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := s.SetPassword("mum", testPassword); !errors.Is(err, ErrNoSuchGuardian) {
		t.Errorf("SetPassword on an unknown guardian = %v, want ErrNoSuchGuardian", err)
	}
	if _, err := s.SetPassword("dad", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("SetPassword with a short password = %v, want ErrWeakPassword", err)
	}
	// The failed attempts must not have moved the epoch.
	g, _ := s.Get("dad")
	if g.SessionEpoch != 1 {
		t.Errorf("SessionEpoch = %d after failed changes, want 1", g.SessionEpoch)
	}
}

func TestStore_RoundTripsThroughTheFile(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := first.Add("dad", testPassword); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := first.SetPassword("dad", "a brand new passphrase"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	second, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	g, ok := second.Get("dad")
	if !ok {
		t.Fatal("guardian missing after reopen")
	}
	if g.SessionEpoch != 2 {
		t.Errorf("SessionEpoch after reopen = %d, want 2", g.SessionEpoch)
	}
	if _, err := second.Verify("dad", "a brand new passphrase"); err != nil {
		t.Errorf("Verify after reopen: %v", err)
	}
}

func TestOpen_RejectsAnEpochBelowOne(t *testing.T) {
	// Lowering session_epoch by hand would hand old cookies back their access,
	// so a file claiming epoch 0 is a startup failure, not a default.
	dir := t.TempDir()
	body := "[[guardians]]\nname = \"dad\"\npassword_hash = \"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA\"\nsession_epoch = 0\n"
	if err := os.WriteFile(filepath.Join(dir, "guardians.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted session_epoch = 0")
	}
}

func TestVerify_MalformedHashFailsClosed(t *testing.T) {
	tests := []string{
		"",
		"not a hash",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",  // wrong variant
		"$argon2id$v=16$m=65536,t=3,p=4$c2FsdA$aGFzaA", // wrong version
		"$argon2id$v=19$m=0,t=3,p=4$c2FsdA$aGFzaA",     // zero memory
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA",    // bad salt base64
	}
	for _, encoded := range tests {
		if verifyPassword(encoded, testPassword) {
			t.Errorf("verifyPassword(%q) = true, want false", encoded)
		}
	}
}

// TestF8_AccountCreatedByAnotherProcessIsVisibleWithoutRestart pins the bug
// that `wasi useradd` on a running server used to hit: the CLI writes
// guardians.toml as a separate process (§9.2, §14), so a store that parsed the
// file once at Open reported "incorrect username or password" for an account
// that demonstrably existed on disk — while the CLI printed "sign in now".
func TestF8_AccountCreatedByAnotherProcessIsVisibleWithoutRestart(t *testing.T) {
	dir := t.TempDir()

	server, err := Open(dir) // the long-running process
	if err != nil {
		t.Fatalf("open server store: %v", err)
	}
	if _, err := server.Verify("dad", testPassword); err == nil {
		t.Fatal("verified against an empty table")
	}

	cli, err := Open(dir) // `wasi useradd`, a second process
	if err != nil {
		t.Fatalf("open cli store: %v", err)
	}
	if _, err := cli.Add("dad", testPassword); err != nil {
		t.Fatalf("useradd: %v", err)
	}

	if _, err := server.Verify("dad", testPassword); err != nil {
		t.Fatalf("the running server cannot see an account another process just created: %v", err)
	}
}

// TestF8_ForeignWriteIsNotClobberedByALaterLocalWrite is the same bug's second
// face: a server holding a stale table that then persists it would delete the
// account the CLI added.
func TestF8_ForeignWriteIsNotClobberedByALaterLocalWrite(t *testing.T) {
	dir := t.TempDir()

	server, err := Open(dir)
	if err != nil {
		t.Fatalf("open server store: %v", err)
	}
	if _, err := server.Add("dad", testPassword); err != nil {
		t.Fatalf("add dad: %v", err)
	}

	cli, err := Open(dir)
	if err != nil {
		t.Fatalf("open cli store: %v", err)
	}
	if _, err := cli.Add("mum", testPassword); err != nil {
		t.Fatalf("add mum: %v", err)
	}

	// The server now writes, for an unrelated reason.
	if _, err := server.SetPassword("dad", testPassword+"-new"); err != nil {
		t.Fatalf("set password: %v", err)
	}

	if _, err := server.Verify("mum", testPassword); err != nil {
		t.Fatalf("the account added by the other process was lost on the next write: %v", err)
	}
}
