// Package config loads /config/wasi.toml, the human-owned configuration file
// (wasi-server-plan §3, §13). Wasi never writes it: it is bind-mounted read-only
// and hot-reloaded on change. Two writers to one file is the failure mode the
// ownership split exists to prevent, which is why the guardian UI displays these
// settings read-only with the file path shown (§9.1).
//
// Secrets are never here (§3). The IMAP password, carrier API key, cookie
// signing key, pututu HMAC key, and shared-service token all come from the
// environment or mounted secret files via package secrets, so wasi.toml stays
// safe to keep in a family git repository. The device token *hash* is safe here.
package config

// Config is the parsed wasi.toml (§13).
type Config struct {
	Owner        Owner        `toml:"owner"`
	Mail         Mail         `toml:"mail"`
	Device       Device       `toml:"device"`
	Sync         Sync         `toml:"sync"`
	Ayllu        Ayllu        `toml:"ayllu"`
	Kipu         Kipu         `toml:"kipu"`
	Backup       Backup       `toml:"backup"`
	Pututu       Pututu       `toml:"pututu"`
	Guardian     Guardian     `toml:"guardian"`
	Carrier      Carrier      `toml:"carrier"`
	Services     Services     `toml:"services"`
	DeviceConfig DeviceConfig `toml:"device_config"`
}

// Owner names the child. Used only for generated subjects (§6.2).
type Owner struct {
	Name string `toml:"name"`
}

// Mail holds endpoints only; credentials come from secrets (§3).
type Mail struct {
	IMAP       string `toml:"imap"`
	SMTP       string `toml:"smtp"`
	Address    string `toml:"address"`
	HeldFolder string `toml:"held_folder"`
	SpamFolder string `toml:"spam_folder"`
}

// Device configures the device listener and its bearer token (§4.1, §12.1).
type Device struct {
	// TokenHash is the SHA-256 hex of the bearer token. Wasi stores only the
	// hash, which is why the pututu MAC needs its own key (§10.2).
	TokenHash string `toml:"token_hash"`
	Listen    string `toml:"listen"`
	TLSCert   string `toml:"tls_cert"`
	TLSKey    string `toml:"tls_key"`
}

// Sync holds the protocol knobs (§13).
type Sync struct {
	// MaxLetterChars is counted in graphemes and applies in both directions.
	MaxLetterChars int `toml:"max_letter_chars"`
	// BudgetBytes is a steady-state response target, not a transport ceiling:
	// assembly always includes at least one complete letter (§4.6).
	BudgetBytes int `toml:"budget_bytes"`
	// ResyncWindow bounds an empty-cursor resync (§4.4).
	ResyncWindow int `toml:"resync_window"`
	IntervalS    int `toml:"interval_s"`
}

// Ayllu caps the contact list. Tombstones count toward the cap (§7.2, A.3).
type Ayllu struct {
	MaxContacts int `toml:"max_contacts"`
}

// Kipu configures health-log retention by whole day-file deletion (§3).
type Kipu struct {
	RetentionDays int `toml:"retention_days"`
}

// Backup configures `wasi backup`, which excludes kipu/ so that retention means
// what it says (§3).
type Backup struct {
	Dir        string `toml:"dir"`
	RetainDays int    `toml:"retain_days"`
}

// Pututu configures doorbell coalescing (§10.1). The HMAC key is a secret.
type Pututu struct {
	CoalesceMin int `toml:"coalesce_min"`
}

// Guardian configures the human listener (§9, §12.1).
type Guardian struct {
	Listen  string `toml:"listen"`
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
	// CopyAddresses is the single documented exception to I-3: optional
	// operational copies of notices to fixed, human-configured addresses (§7.5).
	CopyAddresses []string `toml:"copy_addresses"`
}

// Carrier selects an SMS provider by name plus a provider-specific sub-table.
// The indirection exists because providers disagree on device identity, and
// that difference must not leak into core config or pututu code (§10.4).
type Carrier struct {
	Name    string         `toml:"name"`
	Options map[string]any `toml:"options"`
}

// Services points at the shared, private-network-only helpers (§2, §11).
type Services struct {
	StripURL string `toml:"strip_url"`
	CellURL  string `toml:"cell_url"`
}

// DeviceConfig is passed through to the device in the sync response (§4.3).
// It holds content knobs only — layout is device-owned (§4.9, A.10).
type DeviceConfig struct {
	RAT   string `toml:"rat"`
	Cover string `toml:"cover"`
}

// Defaults per §13. Applied to any field left unset.
const (
	DefaultMaxLetterChars = 500
	DefaultBudgetBytes    = 2048
	DefaultResyncWindow   = 200
	DefaultIntervalS      = 21600
	DefaultMaxContacts    = 24
	DefaultKipuRetention  = 14
	DefaultBackupDir      = "/backups"
	DefaultBackupRetain   = 7
	DefaultCoalesceMin    = 15
	DefaultHeldFolder     = "Held"
)
