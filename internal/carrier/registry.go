package carrier

import (
	"fmt"
	"strconv"

	"github.com/tholent/chaskiwasi/internal/config"
)

// New builds the Carrier selected by cfg (wasi.toml's [carrier] block,
// §10.4). apiKey is the dedicated carrier secret (internal/secrets, never
// TOML); it is ignored by providers that don't need one (fake).
//
// An empty cfg.Name is a valid, supported configuration meaning "no carrier
// configured" — a deployment that hasn't set up an SMS account yet. New
// returns (nil, nil) in that case; callers wire filing.NopDoorbell instead
// of internal/pututu when they get it, exactly as they already do when
// filing.Config.Doorbell is left nil.
//
// This is the one place a provider name string turns into a concrete type;
// everything above it (internal/pututu, internal/filing) only ever sees the
// Carrier interface.
func New(cfg config.Carrier, apiKey string) (Carrier, error) {
	switch cfg.Name {
	case "":
		return nil, nil
	case "fake":
		return NewFake(), nil
	case "hologram":
		return newHologramFromOptions(apiKey, cfg.Options)
	case "soracom":
		return newSoracom(apiKey, cfg.Options)
	default:
		return nil, fmt.Errorf("carrier: unknown provider %q", cfg.Name)
	}
}

// newHologramFromOptions extracts Hologram's provider-specific identity —
// device_id required, org_id optional — from the [carrier.options] sub-table
// so that indirection never leaks into core config (§10.4).
func newHologramFromOptions(apiKey string, options map[string]any) (Carrier, error) {
	deviceID, ok, err := optionInt(options, "device_id")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("carrier: hologram: options.device_id is required")
	}

	orgID := ""
	if v, ok := options["org_id"]; ok {
		s, err := optionString(v)
		if err != nil {
			return nil, fmt.Errorf("carrier: hologram: options.org_id: %w", err)
		}
		orgID = s
	}

	return NewHologram(apiKey, HologramOptions{DeviceID: deviceID, OrgID: orgID})
}

// optionInt reads an integer-shaped provider option. TOML integers unmarshal
// into map[string]any as int64 via go-toml/v2, but a decimal string is
// accepted too, since a human hand-editing wasi.toml may reasonably quote a
// long id.
func optionInt(options map[string]any, key string) (value int64, ok bool, err error) {
	v, present := options[key]
	if !present {
		return 0, false, nil
	}
	switch t := v.(type) {
	case int64:
		return t, true, nil
	case int:
		return int64(t), true, nil
	case float64:
		return int64(t), true, nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("carrier: options.%s must be an integer, got %q", key, t)
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("carrier: options.%s has unsupported type %T", key, v)
	}
}

// optionString coerces a provider option value to string, accepting the
// integer shapes go-toml/v2 might produce for an id a human wrote unquoted.
func optionString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case int:
		return strconv.FormatInt(int64(t), 10), nil
	case float64:
		return strconv.FormatInt(int64(t), 10), nil
	default:
		return "", fmt.Errorf("unsupported type %T", v)
	}
}
