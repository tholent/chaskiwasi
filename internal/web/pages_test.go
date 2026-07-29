package web

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDashboard_DeviceStatusFromTheNewestHealthLine(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	h.seedDeliveries()
	h.seedHealth(
		`{"at":"2026-07-28T09:00:00Z","`+healthBlockKey+`":{"battery_pct":11,"rssi":-120}}`,
		`{"at":"2026-07-29T11:30:00Z","`+healthBlockKey+`":{"battery_pct":84,"rat":"ltem","rssi":-97,"fw":"0.3.1"}}`,
	)

	body := h.get("/", cookie).Body.String()
	for _, want := range []string{"84%", "fair", "-97 dBm", "ltem", "0.3.1"} {
		if !strings.Contains(body, want) {
			t.Errorf("the device panel is missing %q", want)
		}
	}
	if strings.Contains(body, "11%") {
		t.Error("the device panel shows an older health line than the newest one")
	}

	// The fragment htmx swaps in must render the same thing standalone.
	fragment := h.get("/status/device", cookie).Body.String()
	if !strings.Contains(fragment, "84%") {
		t.Error("the device status fragment does not render the battery")
	}
}

func TestDashboard_DeviceStatusSurvivesAnUnreadableLine(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	h.seedHealth(
		`{"at":"2026-07-29T11:30:00Z","`+healthBlockKey+`":{"battery_pct":84}}`,
		`{ this line is not json`,
	)
	if !strings.Contains(h.get("/", cookie).Body.String(), "84%") {
		t.Fatal("one unreadable line took the whole panel down")
	}
}

func TestDashboard_NoHealthHistoryIsNormal(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	body := h.get("/", cookie).Body.String()
	if !strings.Contains(body, "has not reported its condition yet") {
		t.Fatal("a device that has never synced renders as an error rather than as a fact")
	}
}

// TestDashboard_DeliveriesCarryNoContent covers I-1: the panel shows ids,
// outcomes, and timestamps, and nothing else.
func TestDashboard_DeliveriesCarryNoContent(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)
	h.seedDeliveries()

	body := h.get("/", cookie).Body.String()
	for _, want := range []string{"o-000123", "sent", "o-000124", "rejected inactive"} {
		if !strings.Contains(body, want) {
			t.Errorf("the deliveries panel is missing %q", want)
		}
	}
	// There is nowhere for content to come from — the ack ring holds none —
	// so the assertion that matters is that nothing else is offered either.
	if strings.Contains(body, "maya@chaski.test") {
		t.Error("the deliveries panel renders an address")
	}
}

func TestDashboard_BalancePanelDegrades(t *testing.T) {
	tests := []struct {
		name     string
		reporter BalanceReporter
		wantShow bool
		wantText string
	}{
		{"no carrier wired", nil, false, ""},
		{"unsupported by the provider", fixedBalance{err: ErrBalanceUnsupported}, false, ""},
		{"supported", fixedBalance{value: Balance{Amount: 4.25, Currency: "USD"}}, true, "4.25 USD"},
		{"provider unreachable", fixedBalance{err: errors.New("timeout")}, true, "could not be reached"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addGuardian("dad")
			cookie := h.login("dad", testPassword)
			h.server.balance = tc.reporter

			body := h.get("/", cookie).Body.String()
			shown := strings.Contains(body, "Text-message credit")
			if shown != tc.wantShow {
				t.Fatalf("panel shown = %v, want %v", shown, tc.wantShow)
			}
			if tc.wantText != "" && !strings.Contains(body, tc.wantText) {
				t.Errorf("the panel does not show %q", tc.wantText)
			}
		})
	}
}

func TestSettings_AreReadOnlyAndNameTheirFile(t *testing.T) {
	// §9.1: displayed read-only with the file path shown. Two writers to one
	// file is the failure mode the ownership split exists to prevent.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	body := h.get("/settings", cookie).Body.String()

	if !strings.Contains(body, filepath.Join(h.dataDir, "wasi.toml")) {
		t.Error("the settings page does not show the path of the file that owns these settings")
	}
	for _, want := range []string{"Maya", "500 characters", "4", "15 minutes", "dad@example.test", "fake"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page is missing the value %q", want)
		}
	}
	if strings.Contains(body, `action="/settings`) {
		t.Error("the settings page posts back to itself — these settings are read-only here")
	}
	if !strings.Contains(body, "at your own risk") {
		t.Error("the settings page does not say that public exposure is at the operator's own risk (§9.2)")
	}
}

func TestChangeLog_ShowsAddressesAndActors(t *testing.T) {
	// §7.4 points guardians here for the addresses behind a change; I-2
	// permits it because the file is never device-visible.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, cookie), "/contacts?m=contact-added")
	_, contacts := h.ayllu.List()
	mustRedirect(t, h.post("/contacts/readdress", "/contacts", url.Values{
		"contact_id": {contacts[0].ID}, "address": {"rosa2@example.test"},
	}, cookie), "/contacts?m=contact-address")

	body := h.get("/changes", cookie).Body.String()
	for _, want := range []string{"rosa@example.test", "rosa2@example.test", "readdress", "dad", "Rosa"} {
		if !strings.Contains(body, want) {
			t.Errorf("the change log is missing %q", want)
		}
	}
	// Newest first.
	if strings.Index(body, "readdress") > strings.Index(body, ">add<") && strings.Contains(body, ">add<") {
		t.Error("the change log is not newest-first")
	}
}

func TestChangeLog_MissingFileIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	// ayllu.Open creates the log file; removing it puts the page in the state
	// a brand-new deployment is in.
	if err := os.Remove(filepath.Join(h.dataDir, "ayllu-log.jsonl")); err != nil {
		t.Fatalf("removing the change log: %v", err)
	}
	body := h.get("/changes", cookie).Body.String()
	if !strings.Contains(body, "No changes recorded yet") {
		t.Fatalf("a missing change log renders as an error:\n%s", body)
	}
}

func TestChangeLog_SkipsUnreadableLines(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, cookie), "/contacts?m=contact-added")

	path := filepath.Join(h.dataDir, "ayllu-log.jsonl")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, append([]byte("{ not json\n"), existing...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if !strings.Contains(h.get("/changes", cookie).Body.String(), "rosa@example.test") {
		t.Fatal("one unreadable line hid the whole change log")
	}
}

func TestCertificateBanner(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		notAfter time.Time
		want     string
	}{
		{"well inside validity", now.Add(200 * 24 * time.Hour), ""},
		{"just outside the window", now.Add(46 * 24 * time.Hour), ""},
		{"inside the window", now.Add(30 * 24 * time.Hour), "expires in 30 days"},
		{"expired", now.Add(-24 * time.Hour), "expired on"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestCert(t, tc.notAfter)
			got := certWarningFor(path, now)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("warning = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestCertificateBanner_UnreadableFileStillWarns(t *testing.T) {
	// Staying silent would turn a misconfigured path into an invisible
	// problem that surfaces as a device that has quietly stopped reaching home.
	got := certWarningFor(filepath.Join(t.TempDir(), "nope.pem"), time.Now())
	if got == "" {
		t.Fatal("an unreadable certificate produced no banner")
	}
}

func TestCertificateBanner_AppearsOnEveryPage(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)
	h.pinCertWarning("the certificate is about to expire")

	for _, p := range []string{"/", "/contacts", "/held", "/changes", "/settings", "/account"} {
		if !strings.Contains(h.get(p, cookie).Body.String(), "the certificate is about to expire") {
			t.Errorf("%s does not carry the certificate banner (§12.3: persistent)", p)
		}
	}
}

func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	// §9.1: no build step, no CDN. The vendored htmx must come out of the
	// embedded FS, on a page that has to work with the WAN unplugged.
	h := newHarness(t)

	rec := h.get("/static/htmx.min.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/htmx.min.js = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), "var htmx=") {
		t.Error("the served file does not look like htmx")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("static assets are served without nosniff")
	}

	if h.get("/static/app.css", nil).Code != http.StatusOK {
		t.Error("the stylesheet is not served")
	}

	// Every page references both, and nothing else from off-host.
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)
	body := h.get("/", cookie).Body.String()
	if !strings.Contains(body, `src="/static/htmx.min.js"`) {
		t.Error("the layout does not load the vendored htmx")
	}
	if strings.Contains(body, "//unpkg.com") || strings.Contains(body, "cdn.") {
		t.Error("a page references an off-host asset")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	rec := h.get("/contacts", cookie)
	for header, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
}
