package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// Load parses path as wasi.toml, applies the defaults documented in §13 to
// any field left unset, and validates the result. It never writes: this
// package has no Save, because Wasi never writes its own config (§3, §13).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	return &cfg, nil
}

// applyDefaults fills any field left at its Go zero value with the default
// from §13. Because the zero value of int is 0 and of string is "", a field a
// human explicitly set to 0 or "" is indistinguishable from one they left out
// — the same ambiguity §13's table itself lives with ("default" columns for
// fields no TOML syntax can mark "intentionally zero").
func applyDefaults(cfg *Config) {
	if cfg.Mail.HeldFolder == "" {
		cfg.Mail.HeldFolder = DefaultHeldFolder
	}
	if cfg.Sync.MaxLetterChars == 0 {
		cfg.Sync.MaxLetterChars = DefaultMaxLetterChars
	}
	if cfg.Sync.BudgetBytes == 0 {
		cfg.Sync.BudgetBytes = DefaultBudgetBytes
	}
	if cfg.Sync.ResyncWindow == 0 {
		cfg.Sync.ResyncWindow = DefaultResyncWindow
	}
	if cfg.Sync.IntervalS == 0 {
		cfg.Sync.IntervalS = DefaultIntervalS
	}
	if cfg.Ayllu.MaxContacts == 0 {
		cfg.Ayllu.MaxContacts = DefaultMaxContacts
	}
	if cfg.Kipu.RetentionDays == 0 {
		cfg.Kipu.RetentionDays = DefaultKipuRetention
	}
	if cfg.Backup.Dir == "" {
		cfg.Backup.Dir = DefaultBackupDir
	}
	if cfg.Backup.RetainDays == 0 {
		cfg.Backup.RetainDays = DefaultBackupRetain
	}
	if cfg.Pututu.CoalesceMin == 0 {
		cfg.Pututu.CoalesceMin = DefaultCoalesceMin
	}
}

// validate checks the keys that have no sane default and so must come from
// the human: mail endpoints, the owner name used in generated subjects
// (§6.2), the device token hash, and both listener addresses (§12.1). It also
// re-checks the content knobs that a human could set to a nonsensical value
// even after defaulting. Every problem is collected and reported together —
// a human fixing wasi.toml by hand wants the whole list on one failed
// restart, not one error per edit-and-retry cycle.
func validate(cfg *Config) error {
	var problems []error
	require := func(ok bool, key string) {
		if !ok {
			problems = append(problems, fmt.Errorf("%s is required", key))
		}
	}

	require(cfg.Owner.Name != "", "owner.name")
	require(cfg.Mail.IMAP != "", "mail.imap")
	require(cfg.Mail.SMTP != "", "mail.smtp")
	require(cfg.Mail.Address != "", "mail.address")
	require(cfg.Device.Listen != "", "device.listen")
	require(cfg.Guardian.Listen != "", "guardian.listen")

	if cfg.Device.TokenHash == "" {
		problems = append(problems, errors.New("device.token_hash is required"))
	} else if !isHex64(cfg.Device.TokenHash) {
		problems = append(problems, fmt.Errorf(
			"device.token_hash must be 64 hex characters (SHA-256 of the device token), got %d characters",
			len([]rune(cfg.Device.TokenHash)),
		))
	}

	if cfg.Sync.MaxLetterChars <= 0 {
		problems = append(problems, fmt.Errorf(
			"sync.max_letter_chars must be greater than 0, got %d", cfg.Sync.MaxLetterChars,
		))
	}

	return errors.Join(problems...)
}

// isHex64 reports whether s is exactly 64 hexadecimal characters, upper or
// lower case, as produced by fmt.Sprintf("%x", sha256.Sum256(...)).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
