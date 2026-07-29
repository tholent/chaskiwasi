package strip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Config configures a Client.
type Config struct {
	// BaseURL is the strip service's origin, e.g. "http://strip.internal:8080"
	// (wasi.toml's services.strip_url, §13). No trailing slash required.
	BaseURL string

	// Token authenticates to the shared service (§11, §12.4). Comes from
	// secrets (WASI_SERVICE_TOKEN), never wasi.toml.
	Token string

	// Timeout bounds one /strip call. Deliberately tight (default 2s): this
	// is a same-network call to a stateless service, and a slow strip must
	// not turn into a slow sync — falling back is cheap and safe (§5.3), so
	// there is little reason to wait long for the alternative.
	Timeout time.Duration

	// HTTPClient, if set, replaces the default *http.Client (tests only —
	// e.g. to point at an httptest.Server without touching the network).
	HTTPClient *http.Client

	Logger *slog.Logger
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Second
}

func (c Config) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Client is the strip service HTTP client, with the §5.3 fallback built in.
type Client struct {
	cfg Config
}

// NewClient builds a Client.
func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// requestBody and responseBody mirror the wire contract in §11.1 exactly:
// POST /strip {text, format_flowed} -> {body, trimmed, removed_bytes}.
type requestBody struct {
	Text         string `json:"text"`
	FormatFlowed bool   `json:"format_flowed"`
}

type responseBody struct {
	Body         string `json:"body"`
	Trimmed      bool   `json:"trimmed"`
	RemovedBytes int    `json:"removed_bytes"`
}

// Strip calls the live service and returns its result. If the service
// cannot be reached, times out, or returns something unusable, Strip falls
// back to the in-process rules (§5.3) instead of returning an error — see
// the package doc for why that's the whole point of this type existing.
//
// Neither the request text nor the response body is ever logged, at any
// level (I-1). On the fallback path, only the fact that fallback happened
// and the underlying error's type/message are logged — never text content.
func (c *Client) Strip(ctx context.Context, text string, formatFlowed bool) Result {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	defer cancel()

	result, err := c.call(ctx, text, formatFlowed)
	if err != nil {
		c.cfg.logger().Warn("strip: service unreachable, using fallback rules", "err", err)
		return Fallback(text, formatFlowed)
	}
	return Result{Body: result.Body, Trimmed: result.Trimmed, Degraded: false}
}

func (c *Client) call(ctx context.Context, text string, formatFlowed bool) (*responseBody, error) {
	reqBody, err := json.Marshal(requestBody{Text: text, FormatFlowed: formatFlowed})
	if err != nil {
		return nil, fmt.Errorf("strip: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/strip", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("strip: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.cfg.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("strip: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Deliberately not including the response body in the error: on a
		// well-behaved deployment it's a small {"error":"..."} JSON object
		// with no letter content, but nothing here should ever depend on
		// that being true (I-1).
		return nil, fmt.Errorf("strip: unexpected status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, 1<<20) // defensive cap; see services/strip's own MAX_TEXT_BYTES
	var out responseBody
	if err := json.NewDecoder(limited).Decode(&out); err != nil {
		return nil, fmt.Errorf("strip: decode response: %w", err)
	}
	return &out, nil
}
