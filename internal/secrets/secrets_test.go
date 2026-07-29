package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv removes every secret env var this package knows about, so tests
// don't leak state from the ambient environment or from each other.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvIMAPPassword, EnvIMAPPassword + "_FILE",
		EnvCarrierAPIKey, EnvCarrierAPIKey + "_FILE",
		EnvCookieSigningKey, EnvCookieSigningKey + "_FILE",
		EnvPututuKey, EnvPututuKey + "_FILE",
		EnvServiceToken, EnvServiceToken + "_FILE",
	} {
		os.Unsetenv(name)
	}
}

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv(EnvIMAPPassword, "imap-pw")
	t.Setenv(EnvCookieSigningKey, "cookie-key")
	t.Setenv(EnvServiceToken, "service-token")
}

func TestLoad_AllRequiredFromEnv(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.IMAPPassword != "imap-pw" {
		t.Errorf("IMAPPassword = %q, want %q", s.IMAPPassword, "imap-pw")
	}
	if string(s.CookieSigningKey) != "cookie-key" {
		t.Errorf("CookieSigningKey = %q, want %q", s.CookieSigningKey, "cookie-key")
	}
	if s.ServiceToken != "service-token" {
		t.Errorf("ServiceToken = %q, want %q", s.ServiceToken, "service-token")
	}
	if s.CarrierAPIKey != "" {
		t.Errorf("CarrierAPIKey = %q, want empty (optional, unset)", s.CarrierAPIKey)
	}
	if len(s.PututuKey) != 0 {
		t.Errorf("PututuKey = %q, want empty (optional, unset)", s.PututuKey)
	}
}

func TestLoad_MissingRequiredIsAnError(t *testing.T) {
	clearEnv(t)
	// Deliberately leave EnvCookieSigningKey and EnvServiceToken unset.
	t.Setenv(EnvIMAPPassword, "imap-pw")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for missing required secrets")
	}
	for _, want := range []string{EnvCookieSigningKey, EnvServiceToken} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name missing var %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "imap-pw") {
		t.Fatalf("error string leaks a secret value: %q", err.Error())
	}
}

func TestLoad_FileFormTrimsTrailingNewline(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "carrier-key")
	if err := os.WriteFile(path, []byte("super-secret-key\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv(EnvCarrierAPIKey+"_FILE", path)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.CarrierAPIKey != "super-secret-key" {
		t.Errorf("CarrierAPIKey = %q, want %q (newline trimmed)", s.CarrierAPIKey, "super-secret-key")
	}
}

func TestLoad_DirectEnvWinsOverFile(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "pututu-key")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv(EnvPututuKey+"_FILE", path)
	t.Setenv(EnvPututuKey, "from-env")

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(s.PututuKey) != "from-env" {
		t.Errorf("PututuKey = %q, want %q (direct env should win)", s.PututuKey, "from-env")
	}
}

func TestLoad_MissingFileReferencedByEnvIsAnError(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv(EnvCarrierAPIKey+"_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error when the _FILE path does not exist")
	}
}

func TestLoad_OptionalSecretsStayEmptyWithoutError(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.CarrierAPIKey != "" || len(s.PututuKey) != 0 {
		t.Fatalf("optional secrets should be empty when unset, got CarrierAPIKey=%q PututuKey=%q",
			s.CarrierAPIKey, s.PututuKey)
	}
}
