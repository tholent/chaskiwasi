package strip

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestClient_Strip_Success(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody requestBody

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		json.NewEncoder(w).Encode(responseBody{Body: "trimmed text", Trimmed: true, RemovedBytes: 42})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Token: "shared-secret", Logger: silentLogger()})
	result := c.Strip(context.Background(), "original text\n> quoted\n", true)

	if gotAuth != "Bearer shared-secret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer shared-secret")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/strip" {
		t.Errorf("path = %q, want /strip", gotPath)
	}
	if gotBody.Text != "original text\n> quoted\n" || !gotBody.FormatFlowed {
		t.Errorf("request body = %+v, want the original text and format_flowed=true", gotBody)
	}

	if result.Body != "trimmed text" || !result.Trimmed || result.Degraded {
		t.Errorf("result = %+v, want body from the service and Degraded=false", result)
	}
}

func TestClient_Strip_FallsBackOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Token: "t", Logger: silentLogger()})
	result := c.Strip(context.Background(), "hi\n-- \nsig", false)

	if !result.Degraded {
		t.Errorf("Degraded = false, want true after a 500 from the service")
	}
	if result.Body != "hi" {
		t.Errorf("Body = %q, want the fallback-cut body %q", result.Body, "hi")
	}
}

func TestClient_Strip_FallsBackOnMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Token: "t", Logger: silentLogger()})
	result := c.Strip(context.Background(), "plain text", false)

	if !result.Degraded {
		t.Errorf("Degraded = false, want true after a malformed response")
	}
	if result.Body != "plain text" {
		t.Errorf("Body = %q, want the unmodified fallback output", result.Body)
	}
}

func TestClient_Strip_FallsBackWhenUnreachable(t *testing.T) {
	// A closed port: nothing is listening, so the request fails to connect
	// at all — this is the case §5.3 exists for.
	addr := mustClosedAddr(t)

	c := NewClient(Config{BaseURL: "http://" + addr, Token: "t", Timeout: 300 * time.Millisecond, Logger: silentLogger()})
	start := time.Now()
	result := c.Strip(context.Background(), "letter text\n> old quote", false)
	elapsed := time.Since(start)

	if !result.Degraded {
		t.Errorf("Degraded = false, want true when the service is unreachable")
	}
	// The whole point of a "tight timeout": a down Python container must
	// not turn into a slow sync.
	if elapsed > 2*time.Second {
		t.Errorf("Strip took %v with an unreachable service and a 300ms timeout", elapsed)
	}
}

func TestClient_Strip_FallsBackOnContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		json.NewEncoder(w).Encode(responseBody{Body: "too slow", Trimmed: true})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Token: "t", Timeout: 50 * time.Millisecond, Logger: silentLogger()})
	result := c.Strip(context.Background(), "hello", false)

	if !result.Degraded {
		t.Errorf("Degraded = false, want true when the service is slower than the client timeout")
	}
	if result.Body != "hello" {
		t.Errorf("Body = %q, want the fallback's unmodified output, not the slow response", result.Body)
	}
}

func TestClient_Strip_NeverLogsRequestOrResponseText(t *testing.T) {
	// I-1: request/response bodies must never reach a log line, including
	// on the fallback path where the only thing logged is the fact that
	// fallback happened. This is necessarily a light-touch check (it can't
	// prove a negative over all possible logger output), but it pins down
	// that Strip's own log call carries no text fields.
	const secretText = "SUPER-SECRET-LETTER-CONTENT-1234"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	c := NewClient(Config{BaseURL: srv.URL, Token: "t", Logger: logger})
	c.Strip(context.Background(), secretText, false)

	if got := buf.String(); strings.Contains(got, secretText) {
		t.Fatalf("log output contains the letter text: %q", got)
	}
}

// mustClosedAddr returns an address guaranteed to have nothing listening on
// it (bind, then immediately release).
func mustClosedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}
