package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLogin(t *testing.T) {
	tests := []struct {
		name       string
		guardian   string
		password   string
		wantStatus int
		wantCookie bool
	}{
		{"correct", "dad", testPassword, http.StatusSeeOther, true},
		{"name is case-folded", "Dad", testPassword, http.StatusSeeOther, true},
		{"wrong password", "dad", "not the password", http.StatusUnauthorized, false},
		{"unknown guardian", "stranger", testPassword, http.StatusUnauthorized, false},
		{"empty password", "dad", "", http.StatusUnauthorized, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addGuardian("dad")

			rec := h.do(h.request(http.MethodPost, "/login", url.Values{
				"guardian": {tc.guardian},
				"password": {tc.password},
			}, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var got *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookieName && c.Value != "" {
					got = c
				}
			}
			if (got != nil) != tc.wantCookie {
				t.Fatalf("session cookie set = %v, want %v", got != nil, tc.wantCookie)
			}
			if got == nil {
				return
			}
			if !got.HttpOnly || !got.Secure || got.SameSite != http.SameSiteLaxMode {
				t.Errorf("cookie attributes = HttpOnly:%v Secure:%v SameSite:%v, want all set (§9.2)",
					got.HttpOnly, got.Secure, got.SameSite)
			}
		})
	}
}

func TestLogin_FailureRevealsNothingAboutWhichGuardiansExist(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")

	known := h.do(h.request(http.MethodPost, "/login", url.Values{
		"guardian": {"dad"}, "password": {"wrong"},
	}, nil))
	unknown := h.do(h.request(http.MethodPost, "/login", url.Values{
		"guardian": {"nobody"}, "password": {"wrong"},
	}, nil))

	if known.Code != unknown.Code {
		t.Fatalf("status differs by account existence: %d vs %d", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatal("the login page differs by account existence")
	}
}

func TestSession_RequiredOnEveryPage(t *testing.T) {
	h := newHarness(t)

	paths := []string{"/", "/contacts", "/held", "/held/list", "/changes", "/settings", "/account", "/status/device"}
	for _, p := range paths {
		rec := h.get(p, nil)
		mustRedirect(t, rec, "/login?m=signed-out")
	}
}

func TestSession_TamperedCookieIsRejected(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"not three parts", "v1.abc"},
		{"wrong version", "v2" + cookie.Value[2:]},
		{"flipped mac byte", cookie.Value[:len(cookie.Value)-1] + flipLast(cookie.Value)},
		{"payload swapped for another guardian", forgePayload(h, "mum")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.get("/contacts", &http.Cookie{Name: sessionCookieName, Value: tc.value})
			mustRedirect(t, rec, "/login?m=signed-out")
		})
	}
}

// flipLast returns a different final character, so the MAC no longer verifies.
func flipLast(s string) string {
	last := s[len(s)-1]
	if last == 'A' {
		return "B"
	}
	return "A"
}

// forgePayload re-encodes the payload for a different guardian while keeping
// the original MAC — the shape of attack the MAC exists to stop.
func forgePayload(h *harness, guardian string) string {
	cookie := h.login("dad", testPassword)
	parts := strings.Split(cookie.Value, ".")
	forged := sessionPayload(guardian, h.now.Add(time.Hour), 1)
	return parts[0] + "." + base64URL(forged) + "." + parts[2]
}

func TestSession_ExpiresOnTime(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	h.now = h.now.Add(sessionLifetime - time.Minute)
	if rec := h.get("/contacts", cookie); rec.Code != http.StatusOK {
		t.Fatalf("a session one minute short of expiry = %d, want 200", rec.Code)
	}

	h.now = h.now.Add(2 * time.Minute)
	mustRedirect(t, h.get("/contacts", cookie), "/login?m=signed-out")
}

// TestV19_PasswordChangeRejectsPreviouslyIssuedCookies is the first half of
// V-19: a cookie whose MAC still verifies is refused because the account's
// session epoch moved. A lock change that leaves old keys working is not a
// lock change (§9.2).
func TestV19_PasswordChangeRejectsPreviouslyIssuedCookies(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")

	// Two browsers signed in as the same guardian.
	laptop := h.login("dad", testPassword)
	phone := h.login("dad", testPassword)

	if rec := h.get("/account", laptop); rec.Code != http.StatusOK {
		t.Fatalf("laptop session before the change = %d, want 200", rec.Code)
	}

	const newPassword = "an entirely different passphrase"
	rec := h.post("/account/password", "/account", url.Values{
		"current_password": {testPassword},
		"new_password":     {newPassword},
		"confirm_password": {newPassword},
	}, phone)
	mustRedirect(t, rec, "/account?m=password-changed")

	// The browser that made the change is re-cookied so it stays signed in.
	var refreshed *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			refreshed = c
		}
	}
	if refreshed == nil {
		t.Fatal("the guardian who changed their password was not issued a new cookie")
	}
	if refreshed.Value == phone.Value {
		t.Fatal("the reissued cookie is byte-identical to the old one — the epoch did not move")
	}
	if got := h.get("/account", refreshed); got.Code != http.StatusOK {
		t.Fatalf("the reissued cookie = %d, want 200", got.Code)
	}

	// Every previously issued cookie, including the one that made the request,
	// is now dead.
	for name, dead := range map[string]*http.Cookie{"laptop": laptop, "phone (pre-change)": phone} {
		if got := h.get("/account", dead); got.Code != http.StatusSeeOther {
			t.Errorf("%s session after the password change = %d, want a redirect to /login", name, got.Code)
		}
	}

	// And the epoch really moved in the file, not just in memory.
	g, _ := h.guardians.Get("dad")
	if g.SessionEpoch != 2 {
		t.Errorf("SessionEpoch = %d, want 2", g.SessionEpoch)
	}
	if _, err := h.guardians.Verify("dad", newPassword); err != nil {
		t.Errorf("the new password does not verify: %v", err)
	}
}

func TestPasswordChange_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		form    url.Values
		wantTo  string
		wantEpo int
	}{
		{
			"wrong current password",
			url.Values{"current_password": {"nope"}, "new_password": {"a long enough one"}, "confirm_password": {"a long enough one"}},
			"/account?m=password-wrong", 1,
		},
		{
			"new passwords do not match",
			url.Values{"current_password": {testPassword}, "new_password": {"a long enough one"}, "confirm_password": {"a different long one"}},
			"/account?m=password-mismatch", 1,
		},
		{
			"new password too short",
			url.Values{"current_password": {testPassword}, "new_password": {"short"}, "confirm_password": {"short"}},
			"/account?m=password-weak", 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addGuardian("dad")
			cookie := h.login("dad", testPassword)

			mustRedirect(t, h.post("/account/password", "/account", tc.form, cookie), tc.wantTo)

			g, _ := h.guardians.Get("dad")
			if g.SessionEpoch != tc.wantEpo {
				t.Errorf("SessionEpoch = %d, want %d — a refused change must not log anyone out",
					g.SessionEpoch, tc.wantEpo)
			}
			if rec := h.get("/account", cookie); rec.Code != http.StatusOK {
				t.Errorf("the session was invalidated by a refused password change (status %d)", rec.Code)
			}
		})
	}
}

func TestLogout_ClearsTheCookieAndIsPostOnly(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)

	if rec := h.get("/logout", cookie); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /logout = %d, want 405 — a logout reachable by GET is a link an image tag can pull", rec.Code)
	}

	rec := h.post("/logout", "/account", nil, cookie)
	mustRedirect(t, rec, "/login?m=signed-out")
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge >= 0 {
			t.Errorf("logout left the cookie alive (MaxAge %d)", c.MaxAge)
		}
	}
}

func TestGuardianAdd(t *testing.T) {
	tests := []struct {
		name   string
		form   url.Values
		wantTo string
	}{
		{"created", url.Values{"guardian": {"mum"}, "password": {testPassword}, "confirm_password": {testPassword}}, "/account?m=guardian-added"},
		{"already exists", url.Values{"guardian": {"dad"}, "password": {testPassword}, "confirm_password": {testPassword}}, "/account?m=guardian-exists"},
		{"invalid name", url.Values{"guardian": {"aunt rosa"}, "password": {testPassword}, "confirm_password": {testPassword}}, "/account?m=guardian-invalid"},
		{"weak password", url.Values{"guardian": {"mum"}, "password": {"short"}, "confirm_password": {"short"}}, "/account?m=password-weak"},
		{"mismatch", url.Values{"guardian": {"mum"}, "password": {testPassword}, "confirm_password": {"other one entirely"}}, "/account?m=password-mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addGuardian("dad")
			cookie := h.login("dad", testPassword)
			mustRedirect(t, h.post("/account/add", "/account", tc.form, cookie), tc.wantTo)
		})
	}
}

func TestGuardianAdd_CannotResetAnotherGuardiansPassword(t *testing.T) {
	// A guardian who could reset a co-parent's password could lock them out of
	// the record of their own child's contact list (§9.2's hostile household).
	h := newHarness(t)
	h.addGuardian("dad")
	h.addGuardian("mum")
	cookie := h.login("dad", testPassword)

	mustRedirect(t, h.post("/account/add", "/account", url.Values{
		"guardian": {"mum"}, "password": {"a takeover passphrase"}, "confirm_password": {"a takeover passphrase"},
	}, cookie), "/account?m=guardian-exists")

	if _, err := h.guardians.Verify("mum", testPassword); err != nil {
		t.Fatal("another guardian's password was changed from the UI")
	}
	g, _ := h.guardians.Get("mum")
	if g.SessionEpoch != 1 {
		t.Errorf("mum's SessionEpoch = %d, want 1 — her sessions were ended by someone else", g.SessionEpoch)
	}
}
