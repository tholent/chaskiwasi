package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tholent/chaskiwasi/internal/guardians"
)

// §9.2: stateless signed cookies. HMAC over {guardian, expiry,
// session_epoch}, and no session store — a server restart logs everyone out,
// which at two or three accounts is a non-event and is what makes a password
// change able to revoke sessions with nothing to delete.
const (
	// sessionCookieName is deliberately unprefixed. The __Host- prefix would
	// be a small improvement but forbids a Domain attribute and requires
	// Secure, which is fine here — the reason not to use it is that this UI is
	// commonly reached over a VPN by IP or a .internal name where operators
	// end up experimenting with reverse proxies, and a cookie that silently
	// stops being set is the worst possible failure mode for a login page.
	sessionCookieName = "wasi_session"

	// sessionLifetime is how long a login lasts. Twelve hours is one waking
	// day: long enough that a guardian checking the device morning and evening
	// does not log in twice, short enough that a session left open on a shared
	// laptop does not survive the week. §9.2 asks only for "sensible expiry";
	// this is the hostile-household reading of sensible.
	sessionLifetime = 12 * time.Hour

	// sessionVersion prefixes every cookie value. It exists so a future change
	// to the payload layout can be rejected outright instead of misparsed.
	sessionVersion = "v1"
)

// session is what a valid cookie proves. It carries no privileges of its own:
// every guardian who can log in can do everything this UI offers, because at
// this scale a role system would be ceremony without a threat model behind it.
type session struct {
	Guardian string
	Epoch    int
	Expiry   time.Time
}

var (
	errNoSession      = errors.New("web: no session cookie")
	errBadSession     = errors.New("web: session cookie failed verification")
	errSessionExpired = errors.New("web: session expired")
	// errStaleEpoch is the V-19 rejection: the MAC verifies, the expiry is in
	// the future, and the cookie is still refused because the account's
	// password changed after it was issued.
	errStaleEpoch = errors.New("web: session predates a password change")
)

// mintSession builds the signed cookie value for g.
func (s *Server) mintSession(g guardians.Guardian) (value string, expiry time.Time) {
	expiry = s.now().Add(sessionLifetime).UTC()
	payload := sessionPayload(g.Name, expiry, g.SessionEpoch)
	mac := s.signSession(payload)
	enc := base64.RawURLEncoding
	return sessionVersion + "." + enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(mac), expiry
}

// sessionPayload is the exact byte string the MAC covers. The NUL separators
// are safe because guardians.normalizeName restricts names to [a-z0-9._-], so
// no field can contain a separator and no two distinct sessions can produce
// the same payload.
func sessionPayload(guardian string, expiry time.Time, epoch int) string {
	return guardian + "\x00" + strconv.FormatInt(expiry.Unix(), 10) + "\x00" + strconv.Itoa(epoch)
}

func (s *Server) signSession(payload string) []byte {
	mac := hmac.New(sha256.New, s.cookieKey)
	// Domain separation: the same key also signs CSRF tokens, and a value
	// signed for one purpose must never verify for the other.
	mac.Write([]byte("wasi-session-v1\x00"))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// readSession verifies the cookie on r and resolves it against the current
// guardians table. Every failure mode is distinguished here for logging, and
// collapsed into one redirect by the caller: a login page must not explain
// which of the four things went wrong.
func (s *Server) readSession(r *http.Request) (session, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return session{}, errNoSession
	}

	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 || parts[0] != sessionVersion {
		return session{}, errBadSession
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[1])
	if err != nil {
		return session{}, errBadSession
	}
	mac, err := enc.DecodeString(parts[2])
	if err != nil {
		return session{}, errBadSession
	}
	if !hmac.Equal(mac, s.signSession(string(payload))) {
		return session{}, errBadSession
	}

	fields := strings.Split(string(payload), "\x00")
	if len(fields) != 3 {
		return session{}, errBadSession
	}
	unix, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return session{}, errBadSession
	}
	epoch, err := strconv.Atoi(fields[2])
	if err != nil {
		return session{}, errBadSession
	}

	sess := session{Guardian: fields[0], Epoch: epoch, Expiry: time.Unix(unix, 0).UTC()}
	if !s.now().Before(sess.Expiry) {
		return session{}, errSessionExpired
	}

	// The epoch check is the whole point of §9.2's second bullet. A cookie
	// whose MAC verifies perfectly is still refused if the account's password
	// changed after it was issued: a lock change that leaves old keys working
	// is not a lock change (V-19).
	g, ok := s.guardians.Get(sess.Guardian)
	if !ok {
		return session{}, errBadSession
	}
	if g.SessionEpoch != sess.Epoch {
		return session{}, errStaleEpoch
	}
	return sess, nil
}

// setSessionCookie writes the session cookie. HttpOnly because no script in
// this UI has any business reading it; Secure because the guardian listener is
// TLS-only (§12.1) and there is no configuration in which sending this over
// plaintext is correct; SameSite=Lax so a cross-site form POST cannot carry
// it, which is the first of the two CSRF defenses (see csrf.go).
func (s *Server) setSessionCookie(w http.ResponseWriter, value string, expiry time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiry,
		MaxAge:   int(sessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// sessionFrom reads the session a requireSession-wrapped handler is running
// under. It panics if there is none, because that can only mean a route was
// registered without the wrapper — a programming error that must not become a
// silently unauthenticated handler.
func sessionFrom(r *http.Request) session {
	sess, ok := r.Context().Value(sessionContextKey{}).(session)
	if !ok {
		panic(fmt.Sprintf("web: handler for %s ran without requireSession", r.URL.Path))
	}
	return sess
}

type sessionContextKey struct{}
