package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// CSRF protection appropriate to a stateless-cookie design (§9.2). There is
// no session store, so a synchroniser token — the textbook answer — has
// nowhere to live. Two independent defenses are used instead, and the choice
// of both rather than either is deliberate: each one has a documented failure
// mode the other covers.
//
//  1. An origin check on every unsafe request. Sec-Fetch-Site is authoritative
//     where the browser sends it; Origin is the fallback. A request that
//     carries neither is refused, which also means this UI is browser-only by
//     construction — a curl POST with a stolen cookie is rejected before it
//     reaches a handler. This is the defense that does not depend on the page
//     the form came from, and therefore the one that still works if a template
//     ever forgets its hidden field.
//
//  2. A signed token bound to the session and to a fresh random nonce, carried
//     in a hidden form field. The nonce is why the token is not simply a
//     deterministic function of the cookie: a per-render value keeps a token
//     that leaks out of one cached page from being reusable, and costs 16
//     bytes of entropy. Verification is a MAC recomputation, so it needs no
//     server state — which is the only reason a stateless design can have a
//     real token at all.
//
// SameSite=Lax on the session cookie (session.go) is a third layer and is
// treated as defense in depth rather than as the answer: it is a browser
// policy this server cannot verify was applied.
const csrfField = "csrf_token"

// issueCSRF mints a token for sess. Called once per rendered page; every form
// on that page carries the same value, which is fine — the token authenticates
// the origin of the submission, not the individual form.
func (s *Server) issueCSRF(sess session) string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		// crypto/rand failing is not a condition this server can serve
		// through: every remaining request would get an unverifiable token.
		panic("web: no entropy for a CSRF token: " + err.Error())
	}
	enc := base64.RawURLEncoding
	n := enc.EncodeToString(nonce)
	return n + "." + enc.EncodeToString(s.signCSRF(sess, n))
}

func (s *Server) signCSRF(sess session, nonce string) []byte {
	mac := hmac.New(sha256.New, s.cookieKey)
	// Domain separation from the session MAC: a session cookie value must
	// never verify as a CSRF token, or vice versa.
	mac.Write([]byte("wasi-csrf-v1\x00"))
	mac.Write([]byte(sess.Guardian))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.Itoa(sess.Epoch)))
	mac.Write([]byte{0})
	mac.Write([]byte(nonce))
	return mac.Sum(nil)
}

// checkCSRF verifies the hidden field against the session. Binding the token
// to the guardian name and epoch means a token minted for one account, or
// before a password change, does not authorise a write afterwards.
func (s *Server) checkCSRF(r *http.Request, sess session) bool {
	token := r.PostFormValue(csrfField)
	nonce, mac, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		return false
	}
	return hmac.Equal(got, s.signCSRF(sess, nonce))
}

// checkOrigin implements defense (1). It runs on every unsafe request,
// including the login POST — which has no session and therefore no token, so
// this is the only CSRF defense login has. Login CSRF (an attacker logging a
// victim into the attacker's account) is a real if minor attack and the origin
// check is a complete answer to it.
func (s *Server) checkOrigin(r *http.Request) bool {
	// Sec-Fetch-Site is set by the browser and cannot be forged from script.
	// "none" means a direct user action such as a bookmark; "same-origin" is
	// the normal case for a form on our own page.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		// Every browser sends Origin on a form POST. Absence means either a
		// non-browser client or a browser old enough that neither header
		// exists, and refusing both is the correct default for a UI whose only
		// intended client is a browser on a home LAN.
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
