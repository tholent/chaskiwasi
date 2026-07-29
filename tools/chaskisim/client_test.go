package chaskisim

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

func TestClient_Sync_Success_RoundTripsRequestAndResponse(t *testing.T) {
	var gotAuth, gotContentType string
	var gotReq protocol.Request

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("server: decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(protocol.Response{
			ServerTime: 1234,
			Cursor:     "next-cursor",
		})
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "dev-token"})
	resp, err := c.Sync(context.Background(), protocol.Request{Cursor: "prev-cursor", AylluVersion: 3})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if gotAuth != "Bearer dev-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer dev-token")
	}
	if gotContentType != "application/json; charset=utf-8" {
		t.Errorf("Content-Type header = %q", gotContentType)
	}
	if gotReq.Cursor != "prev-cursor" {
		t.Errorf("server saw cursor %q, want %q", gotReq.Cursor, "prev-cursor")
	}
	if resp.Cursor != "next-cursor" {
		t.Errorf("resp.Cursor = %q, want %q", resp.Cursor, "next-cursor")
	}
}

func TestClient_Sync_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "wrong"})
	_, err := c.Sync(context.Background(), protocol.Request{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Sync error = %v, want ErrUnauthorized", err)
	}
}

func TestClient_Sync_ServiceUnavailable_CarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "mailbox unreachable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "dev-token"})
	_, err := c.Sync(context.Background(), protocol.Request{})

	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("Sync error = %v, want *RetryableError", err)
	}
	if retryable.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", retryable.StatusCode)
	}
	if retryable.RetryAfter != 300*time.Second {
		t.Errorf("RetryAfter = %s, want 300s", retryable.RetryAfter)
	}
}

func TestClient_Sync_UnexpectedStatus_IsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "dev-token"})
	_, err := c.Sync(context.Background(), protocol.Request{})

	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("Sync error = %v, want *RetryableError (§4.1: anything else is transient)", err)
	}
}
