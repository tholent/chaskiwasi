package carrier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// hologramBaseURL is Hologram's Cloud REST API (dashboard.hologram.io),
// documented at https://docs.hologram.io/guides/rest-api/beginners-guide-to-the-hologram-rest-api
// and https://support.hologram.io/hc/en-us/articles/360035697173.
const hologramBaseURL = "https://dashboard.hologram.io/api/1"

// Hologram implements Carrier against Hologram's Cloud API. It keys device
// identity on a Hologram device id, per §10.4's note that Hologram and
// Soracom disagree on this and the disagreement must not leak past this
// file.
type Hologram struct {
	baseURL  string
	apiKey   string
	deviceID int64
	// orgID is only needed for Balance; Hologram's SMS-send endpoint doesn't
	// take one. An unset orgID makes Balance report ErrUnsupported rather
	// than guess at a value (§10.4).
	orgID  string
	client *http.Client
}

var _ Carrier = (*Hologram)(nil)

// HologramOptions configures NewHologram. DeviceID and OrgID come from
// wasi.toml's [carrier] sub-table (§10.4's provider-specific indirection);
// BaseURL and HTTPClient exist for tests, which point BaseURL at an
// httptest.Server instead of the real dashboard.
type HologramOptions struct {
	// DeviceID is the Hologram device id to ring. Required.
	DeviceID int64
	// OrgID is the Hologram organization id, needed only for Balance.
	// Leaving it empty is a valid configuration: it just means Balance
	// degrades to ErrUnsupported.
	OrgID string

	BaseURL    string
	HTTPClient *http.Client
}

// NewHologram builds a Hologram carrier. apiKey comes from
// internal/secrets, never from wasi.toml (§3, §10.4).
func NewHologram(apiKey string, opts HologramOptions) (*Hologram, error) {
	if apiKey == "" {
		return nil, errors.New("carrier: hologram: API key is required")
	}
	if opts.DeviceID == 0 {
		return nil, errors.New("carrier: hologram: device id is required (§10.4: Hologram keys device identity on a device id)")
	}

	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = hologramBaseURL
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	return &Hologram{
		baseURL:  baseURL,
		apiKey:   apiKey,
		deviceID: opts.DeviceID,
		orgID:    opts.OrgID,
		client:   client,
	}, nil
}

// Name implements Carrier.
func (h *Hologram) Name() string { return "hologram" }

// hologramSMSRequest is the /sms/incoming request body. fromnumber is
// deliberately never set: §10.2 forbids any sender identity in the payload,
// and this field exists only to spoof a display number, which is exactly
// the kind of "content" the doorbell contract rules out.
type hologramSMSRequest struct {
	DeviceID int64  `json:"deviceid"`
	Body     string `json:"body"`
}

type hologramResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// Pututu implements Carrier by POSTing to Hologram's /sms/incoming endpoint
// — Hologram's name for "message inbound to the device," i.e. cloud-to-SIM
// (§10.4). payload is the opaque doorbell token (§10.2); it is the entire
// message body, and nothing else is added.
func (h *Hologram) Pututu(ctx context.Context, payload string) error {
	body, err := json.Marshal(hologramSMSRequest{DeviceID: h.deviceID, Body: payload})
	if err != nil {
		return fmt.Errorf("carrier: hologram: encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/sms/incoming", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("carrier: hologram: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("apikey", h.apiKey)

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("carrier: hologram: sms send: %w", err)
	}
	defer resp.Body.Close()

	var parsed hologramResponse
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr == nil {
		_ = json.Unmarshal(data, &parsed) // best-effort; status code is the primary signal below
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("carrier: hologram: sms send: status %d", resp.StatusCode)
	}
	if !parsed.Success {
		msg := parsed.Error
		if msg == "" {
			msg = "response did not report success"
		}
		return fmt.Errorf("carrier: hologram: sms send: %s", msg)
	}
	return nil
}

type hologramBalanceResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Balance string `json:"balance"`
	} `json:"data"`
}

// Balance implements Carrier by reading the organization balance endpoint.
// Hologram's documented response reports balance as a decimal string in the
// account's billing currency; the API does not carry a currency code, so
// this reports "USD" — Hologram's own billing currency for every account at
// the time this was written. ErrUnsupported when no OrgID is configured,
// per §10.4's "optional capabilities degrade, they do not panic."
func (h *Hologram) Balance(ctx context.Context) (Balance, error) {
	if h.orgID == "" {
		return Balance{}, ErrUnsupported
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.baseURL+"/organizations/"+h.orgID+"/balance", nil)
	if err != nil {
		return Balance{}, fmt.Errorf("carrier: hologram: building balance request: %w", err)
	}
	req.SetBasicAuth("apikey", h.apiKey)

	resp, err := h.client.Do(req)
	if err != nil {
		return Balance{}, fmt.Errorf("carrier: hologram: balance: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Balance{}, fmt.Errorf("carrier: hologram: balance: status %d", resp.StatusCode)
	}

	var parsed hologramBalanceResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&parsed); err != nil {
		return Balance{}, fmt.Errorf("carrier: hologram: decoding balance response: %w", err)
	}
	if !parsed.Success {
		return Balance{}, errors.New("carrier: hologram: balance: response did not report success")
	}

	amount, err := strconv.ParseFloat(parsed.Data.Balance, 64)
	if err != nil {
		return Balance{}, fmt.Errorf("carrier: hologram: parsing balance %q: %w", parsed.Data.Balance, err)
	}
	return Balance{Amount: amount, Currency: "USD"}, nil
}
