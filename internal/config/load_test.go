package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_ExampleFileCoversEveryDefault(t *testing.T) {
	cfg, err := Load("testdata/wasi.example.toml")
	if err != nil {
		t.Fatalf("Load(example): %v", err)
	}

	if cfg.Owner.Name != "Maya" {
		t.Errorf("Owner.Name = %q, want %q", cfg.Owner.Name, "Maya")
	}
	if cfg.Mail.IMAP == "" || cfg.Mail.SMTP == "" || cfg.Mail.Address == "" {
		t.Errorf("Mail block incomplete: %+v", cfg.Mail)
	}
	if cfg.Device.TokenHash == "" || !isHex64(cfg.Device.TokenHash) {
		t.Errorf("Device.TokenHash invalid: %q", cfg.Device.TokenHash)
	}
	if cfg.Sync.MaxLetterChars != 500 {
		t.Errorf("Sync.MaxLetterChars = %d, want 500", cfg.Sync.MaxLetterChars)
	}
	if cfg.Ayllu.MaxContacts != 24 {
		t.Errorf("Ayllu.MaxContacts = %d, want 24", cfg.Ayllu.MaxContacts)
	}
	if cfg.Kipu.RetentionDays != 14 {
		t.Errorf("Kipu.RetentionDays = %d, want 14", cfg.Kipu.RetentionDays)
	}
	if cfg.Backup.Dir != "/backups" || cfg.Backup.RetainDays != 7 {
		t.Errorf("Backup = %+v, want dir=/backups retain_days=7", cfg.Backup)
	}
	if cfg.Pututu.CoalesceMin != 15 {
		t.Errorf("Pututu.CoalesceMin = %d, want 15", cfg.Pututu.CoalesceMin)
	}
	if cfg.Guardian.Listen == "" {
		t.Errorf("Guardian.Listen is empty")
	}
	if len(cfg.Guardian.CopyAddresses) != 0 {
		t.Errorf("Guardian.CopyAddresses = %v, want empty by default", cfg.Guardian.CopyAddresses)
	}
	if cfg.Carrier.Name != "hologram" {
		t.Errorf("Carrier.Name = %q, want %q", cfg.Carrier.Name, "hologram")
	}
	if cfg.Carrier.Options["device_id"] != "123456" {
		t.Errorf("Carrier.Options[device_id] = %v, want 123456", cfg.Carrier.Options["device_id"])
	}
	if cfg.Services.StripURL == "" || cfg.Services.CellURL == "" {
		t.Errorf("Services block incomplete: %+v", cfg.Services)
	}
	if cfg.DeviceConfig.RAT == "" || cfg.DeviceConfig.Cover == "" {
		t.Errorf("DeviceConfig block incomplete: %+v", cfg.DeviceConfig)
	}
}

func TestLoad_AppliesDefaultsWhenOmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wasi.toml")
	minimal := `
[owner]
name = "Maya"
[mail]
imap = "imap.example.com:993"
smtp = "smtp.example.com:465"
address = "maya@example.com"
[device]
token_hash = "` + strings.Repeat("a", 64) + `"
listen = "0.0.0.0:8443"
[guardian]
listen = "0.0.0.0:8444"
`
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		name string
		got  int
		want int
	}{
		{"Sync.MaxLetterChars", cfg.Sync.MaxLetterChars, DefaultMaxLetterChars},
		{"Sync.BudgetBytes", cfg.Sync.BudgetBytes, DefaultBudgetBytes},
		{"Sync.ResyncWindow", cfg.Sync.ResyncWindow, DefaultResyncWindow},
		{"Sync.IntervalS", cfg.Sync.IntervalS, DefaultIntervalS},
		{"Ayllu.MaxContacts", cfg.Ayllu.MaxContacts, DefaultMaxContacts},
		{"Kipu.RetentionDays", cfg.Kipu.RetentionDays, DefaultKipuRetention},
		{"Backup.RetainDays", cfg.Backup.RetainDays, DefaultBackupRetain},
		{"Pututu.CoalesceMin", cfg.Pututu.CoalesceMin, DefaultCoalesceMin},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want default %d", c.name, c.got, c.want)
		}
	}
	if cfg.Mail.HeldFolder != DefaultHeldFolder {
		t.Errorf("Mail.HeldFolder = %q, want default %q", cfg.Mail.HeldFolder, DefaultHeldFolder)
	}
	if cfg.Backup.Dir != DefaultBackupDir {
		t.Errorf("Backup.Dir = %q, want default %q", cfg.Backup.Dir, DefaultBackupDir)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	base := func() string {
		return `
[owner]
name = "Maya"
[mail]
imap = "imap.example.com:993"
smtp = "smtp.example.com:465"
address = "maya@example.com"
[device]
token_hash = "` + strings.Repeat("a", 64) + `"
listen = "0.0.0.0:8443"
[guardian]
listen = "0.0.0.0:8444"
`
	}

	tests := []struct {
		name    string
		toml    string
		wantKey string // substring that must appear in the error, naming the offending key
	}{
		{
			name:    "missing owner name",
			toml:    strings.Replace(base(), `name = "Maya"`, `name = ""`, 1),
			wantKey: "owner.name",
		},
		{
			name:    "missing token_hash",
			toml:    strings.Replace(base(), `token_hash = "`+strings.Repeat("a", 64)+`"`, `token_hash = ""`, 1),
			wantKey: "device.token_hash",
		},
		{
			name:    "short token_hash",
			toml:    strings.Replace(base(), strings.Repeat("a", 64), strings.Repeat("a", 10), 1),
			wantKey: "device.token_hash",
		},
		{
			name:    "non-hex token_hash",
			toml:    strings.Replace(base(), strings.Repeat("a", 64), strings.Repeat("z", 64), 1),
			wantKey: "device.token_hash",
		},
		{
			name:    "missing device listen",
			toml:    strings.Replace(base(), `listen = "0.0.0.0:8443"`, `listen = ""`, 1),
			wantKey: "device.listen",
		},
		{
			name:    "missing guardian listen",
			toml:    strings.Replace(base(), `listen = "0.0.0.0:8444"`, `listen = ""`, 1),
			wantKey: "guardian.listen",
		},
		{
			name:    "negative max_letter_chars",
			toml:    base() + "\n[sync]\nmax_letter_chars = -5\n",
			wantKey: "sync.max_letter_chars",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wasi.toml")
			if err := os.WriteFile(path, []byte(tt.toml), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected a validation error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error %q does not name the offending key %q", err.Error(), tt.wantKey)
			}
		})
	}
}

func TestLoad_ReportsAllProblemsTogether(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wasi.toml")
	// Only owner.name is set; everything else required is missing.
	if err := os.WriteFile(path, []byte(`[owner]
name = "Maya"
`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"mail.imap", "mail.smtp", "mail.address", "device.token_hash", "device.listen", "guardian.listen"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("combined error missing %q: %s", want, err.Error())
		}
	}
}
