// Package secrets loads the five values wasi-server-plan §3 says must never
// live in wasi.toml: the IMAP password, carrier API key, cookie-signing key,
// pututu HMAC key, and the shared-service bearer token. Each is read from
// either a plain environment variable or a mounted secret file (the `_FILE`
// convention Docker/Kubernetes secrets use), so an operator can choose
// whichever their deployment supports without any code change.
//
// Nothing in this package ever logs a secret value. Errors name the missing
// environment variable, never its (absent) content.
package secrets

import (
	"fmt"
	"os"
	"strings"
)

// Environment variable names. Each has a matching "<name>_FILE" form that
// points at a mounted file instead (§3).
const (
	EnvIMAPPassword     = "WASI_IMAP_PASSWORD"
	EnvCarrierAPIKey    = "WASI_CARRIER_API_KEY"
	EnvCookieSigningKey = "WASI_COOKIE_SIGNING_KEY"
	EnvPututuKey        = "WASI_PUTUTU_KEY"
	EnvServiceToken     = "WASI_SERVICE_TOKEN"
)

// Secrets holds every value this server needs that must not live in
// wasi.toml. Fields marked optional are legitimately empty in deployments
// that don't yet use the feature they gate (e.g. no carrier configured
// before M3); Load only errors on the ones marked required.
type Secrets struct {
	// IMAPPassword authenticates to the mailbox. Required: nothing in this
	// server works without mail.
	IMAPPassword string

	// CookieSigningKey is the HMAC key for guardian session cookies (§9.2).
	// Required: the guardian UI cannot issue sessions without it.
	CookieSigningKey []byte

	// ServiceToken is the shared bearer token for strip and cell (§11).
	// Required: derivation depends on strip from M1.
	ServiceToken string

	// CarrierAPIKey authenticates to the configured SMS provider (§10.4).
	// Optional: a deployment with no [carrier] configured needs none.
	CarrierAPIKey string

	// PututuKey is the dedicated HMAC key for doorbell tokens (§10.2) — it
	// must be a separate secret from the device bearer token, because Wasi
	// stores only that token's hash and so cannot MAC with it. Optional at
	// the secrets layer for the same reason as CarrierAPIKey; the caller
	// (pututu/carrier wiring) is what knows whether a carrier is configured
	// and therefore whether this is actually required.
	PututuKey []byte
}

// definition describes one secret's environment variable and whether Load
// must fail if it's absent.
type definition struct {
	name     string
	required bool
}

// Load reads every secret from the environment or a mounted file, per the
// package doc. It returns one combined error naming every missing required
// secret by its environment variable, never by value, so a misconfigured
// deployment fails loudly and legibly at startup rather than well into a
// sync or a login attempt.
func Load() (*Secrets, error) {
	values := map[string]string{}
	var missing []string

	for _, d := range []definition{
		{EnvIMAPPassword, true},
		{EnvCookieSigningKey, true},
		{EnvServiceToken, true},
		{EnvCarrierAPIKey, false},
		{EnvPututuKey, false},
	} {
		v, ok, err := resolve(d.name)
		if err != nil {
			return nil, err
		}
		if !ok {
			if d.required {
				missing = append(missing, d.name)
			}
			continue
		}
		values[d.name] = v
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets: missing required environment variable(s) (set <name> or <name>_FILE): %s",
			strings.Join(missing, ", "))
	}

	return &Secrets{
		IMAPPassword:     values[EnvIMAPPassword],
		CookieSigningKey: []byte(values[EnvCookieSigningKey]),
		ServiceToken:     values[EnvServiceToken],
		CarrierAPIKey:    values[EnvCarrierAPIKey],
		PututuKey:        []byte(values[EnvPututuKey]),
	}, nil
}

// resolve reads name from the environment, falling back to reading the file
// named by "<name>_FILE" if name itself is unset or empty. It reports
// ok=false if neither is set. Trailing newlines are trimmed, since secret
// files are routinely produced by `echo` or an editor that adds one.
func resolve(name string) (value string, ok bool, err error) {
	if v, present := os.LookupEnv(name); present && v != "" {
		return v, true, nil
	}

	filePath, present := os.LookupEnv(name + "_FILE")
	if !present || filePath == "" {
		return "", false, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", false, fmt.Errorf("secrets: read %s (from %s_FILE): %w", filePath, name, err)
	}
	return strings.TrimRight(string(data), "\n\r"), true, nil
}
