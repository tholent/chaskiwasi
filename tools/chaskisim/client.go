package chaskisim

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

// ErrUnauthorized reports a 401 from the device listener (§4.1): bad or
// missing bearer token. The device's behaviour rule is "show a
// provisioning-fault state; do not retry hot" — this simulator surfaces that
// as a distinct error so a caller (the CLI, or an e2e test) can tell it
// apart from a transient failure instead of retrying into a tight loop.
var ErrUnauthorized = errors.New("chaskisim: device token rejected (401)")

// RetryableError reports any sync outcome the wire contract says the device
// should retry: the identical request, later (§4.1's coarse status table —
// 503 with Retry-After, or anything else that isn't 200 or 401).
type RetryableError struct {
	// StatusCode is the HTTP status that produced this error.
	StatusCode int
	// RetryAfter is the server's hint (§4.1's 503 case), zero if absent.
	RetryAfter time.Duration
	Err        error
}

func (e *RetryableError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("chaskisim: sync retryable (status %d, retry after %s): %v", e.StatusCode, e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("chaskisim: sync retryable (status %d): %v", e.StatusCode, e.Err)
}

func (e *RetryableError) Unwrap() error { return e.Err }

// Client speaks POST /sync (§4) over HTTPS to one Wasi device listener.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// ClientConfig configures a Client.
type ClientConfig struct {
	// BaseURL is the full sync endpoint, e.g. "https://localhost:8443/sync".
	BaseURL string
	// Token is the device bearer token (§4.1), sent as "Authorization:
	// Bearer <token>".
	Token string

	// TLSConfig, if set, is used as-is for the underlying transport. This is
	// how a caller pins the private CA (§12.2) in production use, or sets
	// InsecureSkipVerify for the local dev fixture's self-signed
	// certificate (deploy/README.md explains why that's only ever correct
	// there). Nil means Go's default trust store, which is right for
	// neither case a Chaski device is actually deployed in — callers are
	// expected to set this deliberately rather than rely on a default.
	TLSConfig *tls.Config

	// Timeout bounds one whole POST /sync round trip. Defaults to 30s.
	Timeout time.Duration
}

// NewClient builds a Client. It does not connect; the first Sync call does.
func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: cfg.BaseURL,
		token:   cfg.Token,
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: cfg.TLSConfig},
		},
	}
}

// Sync performs one POST /sync round trip (§4.1-§4.3): exactly one HTTP
// request, exactly one response. Looping on More is Wake's job, not this
// method's — Sync never retries and never loops, so a caller that wants to
// observe a single round (e.g. to inspect More directly) can.
func (c *Client) Sync(ctx context.Context, req protocol.Request) (*protocol.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("chaskisim: marshalling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("chaskisim: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("chaskisim: request failed: %w", err)}
	}
	defer resp.Body.Close()

	// §4.1's status table is deliberately coarse: 200 apply, 401 don't
	// retry hot, 503 retry after the hint, anything else is still just
	// "transient, retry later" — there are no other special cases.
	switch resp.StatusCode {
	case http.StatusOK:
		var out protocol.Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("chaskisim: decoding response: %w", err)
		}
		return &out, nil

	case http.StatusUnauthorized:
		return nil, ErrUnauthorized

	case http.StatusServiceUnavailable:
		return nil, &RetryableError{
			StatusCode: resp.StatusCode,
			RetryAfter: retryAfterDuration(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("service unavailable: %s", drainErrorBody(resp.Body)),
		}

	default:
		return nil, &RetryableError{
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("unexpected status: %s", drainErrorBody(resp.Body)),
		}
	}
}

// retryAfterDuration parses a Retry-After header value in seconds (§4.1
// always sends it as such); an unparseable or absent value is zero, which
// callers treat as "no hint given."
func retryAfterDuration(header string) time.Duration {
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// drainErrorBody reads a short error body for diagnostics. It is capped and
// best-effort: a device error path has no reason to ever contain a letter
// body, but capping it costs nothing and keeps a misbehaving server from
// handing this simulator an unbounded read.
func drainErrorBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 4096))
	return string(data)
}
