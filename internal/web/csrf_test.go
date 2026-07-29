package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// mutatingRoutes is every state-changing path. The tests below run the whole
// list rather than a sample, so a route added without protection fails here
// instead of in review.
var mutatingRoutes = []string{
	"/logout",
	"/contacts/add",
	"/contacts/deactivate",
	"/contacts/reactivate",
	"/contacts/readdress",
	"/held/release",
	"/account/password",
	"/account/add",
}

func TestMutatingRoutesRejectGET(t *testing.T) {
	// §9.2: every state-changing action is a POST. The routes are registered
	// method-qualified, so this is the router's guarantee and not a check a
	// future handler could forget.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	for _, path := range mutatingRoutes {
		if rec := h.get(path, cookie); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, rec.Code)
		}
	}
}

func TestMutatingRoutesRequireACSRFToken(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	valid := h.csrfFrom("/account", cookie)

	tests := []struct {
		name  string
		token string
	}{
		{"absent", ""},
		{"not a token", "garbage"},
		{"no nonce separator", strings.ReplaceAll(valid, ".", "")},
		{"nonce kept, mac replaced", strings.SplitN(valid, ".", 2)[0] + ".AAAA"},
		{"mac kept, nonce replaced", "AAAA." + strings.SplitN(valid, ".", 2)[1]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range mutatingRoutes {
				rec := h.do(h.request(http.MethodPost, path, url.Values{csrfField: {tc.token}}, cookie))
				if rec.Code != http.StatusForbidden {
					t.Errorf("POST %s with a %s token = %d, want 403", path, tc.name, rec.Code)
				}
			}
		})
	}
}

func TestCSRFTokenIsBoundToTheSession(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	h.addGuardian("mum")

	dad := h.login("dad", testPassword)
	mum := h.login("mum", testPassword)

	dadsToken := h.csrfFrom("/account", dad)

	// mum's cookie plus dad's token must not authorise a write: the token is a
	// MAC over the guardian name and epoch, not a free-floating secret.
	rec := h.do(h.request(http.MethodPost, "/contacts/add", url.Values{
		csrfField: {dadsToken},
		"name":    {"Rosa"},
		"address": {"rosa@example.test"},
	}, mum))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("another guardian's token = %d, want 403", rec.Code)
	}
}

func TestCSRFTokenIsInvalidatedByAPasswordChange(t *testing.T) {
	// The token binds the epoch, so a page rendered before a password change
	// cannot be submitted after one — the same revocation the cookie gets.
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	stale := h.csrfFrom("/contacts", cookie)

	const newPassword = "an entirely different passphrase"
	rec := h.post("/account/password", "/account", url.Values{
		"current_password": {testPassword},
		"new_password":     {newPassword},
		"confirm_password": {newPassword},
	}, cookie)
	var refreshed *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			refreshed = c
		}
	}

	got := h.do(h.request(http.MethodPost, "/contacts/add", url.Values{
		csrfField: {stale},
		"name":    {"Rosa"},
		"address": {"rosa@example.test"},
	}, refreshed))
	if got.Code != http.StatusForbidden {
		t.Fatalf("a token minted before the password change = %d, want 403", got.Code)
	}
}

func TestOriginIsCheckedOnEveryWrite(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)
	token := h.csrfFrom("/account", cookie)

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{"same-origin fetch metadata", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusSeeOther},
		{"direct navigation", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusSeeOther},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://" + testHost}, http.StatusForbidden},
		{"same-site but not same-origin", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"matching Origin only", map[string]string{"Origin": "https://" + testHost}, http.StatusSeeOther},
		{"foreign Origin", map[string]string{"Origin": "https://evil.test"}, http.StatusForbidden},
		{"no origin information at all", map[string]string{}, http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/logout",
				strings.NewReader(url.Values{csrfField: {token}}.Encode()))
			r.Host = testHost
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			r.AddCookie(cookie)

			if rec := h.do(r); rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestLoginIsOriginChecked(t *testing.T) {
	// Login has no session and therefore no token; the origin check is its
	// whole CSRF defense, and it must actually run.
	h := newHarness(t)
	h.addGuardian("dad")

	r := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(url.Values{"guardian": {"dad"}, "password": {testPassword}}.Encode()))
	r.Host = testHost
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.test")

	if rec := h.do(r); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login = %d, want 403", rec.Code)
	}
}
