package carrier_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tholent/chaskiwasi/internal/carrier"
	"github.com/tholent/chaskiwasi/internal/carrier/carriertest"
)

// TestFake_Conformance runs the shared suite against the fake provider
// (§10.4: "a new provider is one package plus a passing carriertest
// conformance run").
func TestFake_Conformance(t *testing.T) {
	carriertest.Run(t, carriertest.Config{
		New: func(t *testing.T) carrier.Carrier {
			return carrier.NewFake()
		},
		Failing: func(t *testing.T) carrier.Carrier {
			f := carrier.NewFake()
			f.FailNext(1, nil)
			return f
		},
		BalanceUnsupported: func(t *testing.T) carrier.Carrier {
			return carrier.NewFake() // ErrUnsupported by default
		},
	})
}

// hologramTestServer builds an httptest server standing in for
// dashboard.hologram.io, and a *carrier.Hologram pointed at it. success
// controls whether /sms/incoming reports success; balance is the decimal
// string the /organizations/{id}/balance endpoint returns.
func hologramTestServer(t *testing.T, smsSuccess bool, balance string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/incoming", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "apikey" || pass != "test-hologram-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			DeviceID int64  `json:"deviceid"`
			Body     string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.DeviceID == 0 || body.Body == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if smsSuccess {
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "simulated hologram failure"})
		}
	})
	mux.HandleFunc("/organizations/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"balance": balance},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestHologram(t *testing.T, srv *httptest.Server, orgID string) *carrier.Hologram {
	t.Helper()
	h, err := carrier.NewHologram("test-hologram-key", carrier.HologramOptions{
		DeviceID: 4242,
		OrgID:    orgID,
		BaseURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("NewHologram: %v", err)
	}
	return h
}

// TestHologram_Conformance runs the shared suite against internal/carrier's
// hologram provider, using httptest servers instead of the real Hologram
// account M3's plan explicitly wants this offline (§15: "the only
// hardware-blocked check is one live Hologram SMS at bring-up").
func TestHologram_Conformance(t *testing.T) {
	carriertest.Run(t, carriertest.Config{
		New: func(t *testing.T) carrier.Carrier {
			srv := hologramTestServer(t, true, "96.05")
			return newTestHologram(t, srv, "org-1")
		},
		Failing: func(t *testing.T) carrier.Carrier {
			srv := hologramTestServer(t, false, "0")
			return newTestHologram(t, srv, "org-1")
		},
		BalanceUnsupported: func(t *testing.T) carrier.Carrier {
			srv := hologramTestServer(t, true, "96.05")
			return newTestHologram(t, srv, "") // no org id configured
		},
	})
}
