package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tholent/chaskiwasi/internal/guardians"
)

// maxFormBytes bounds a posted form. The largest form here carries a name, an
// address, and a token; 64 KiB is orders of magnitude of headroom.
const maxFormBytes = 64 << 10

// requireSession is the one place authentication and CSRF are enforced.
// Wrapping every non-login route in it — rather than checking inside each
// handler — is what makes "every state-changing action is POST with CSRF
// protection" (§9.2) a property of the router instead of a habit.
func (s *Server) requireSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.readSession(r)
		if err != nil {
			// The four failure modes are logged apart and answered the same:
			// a login page that explains which one happened is an oracle.
			if !errors.Is(err, errNoSession) {
				s.log.Info("web: session rejected", "path", r.URL.Path, "reason", err)
			}
			s.clearSessionCookie(w)
			s.redirect(w, r, "/login", flashSignedOut)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Every form this UI posts is a few hundred bytes. Capping the body
			// before anything parses it means a request that is not one of
			// those costs nothing to refuse.
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)

			if !s.checkOrigin(r) {
				s.log.Warn("web: rejected a write with a bad or missing origin",
					"path", r.URL.Path, "guardian", sess.Guardian)
				http.Error(w, "request origin rejected", http.StatusForbidden)
				return
			}
			if !s.checkCSRF(r, sess) {
				s.log.Warn("web: rejected a write with a bad CSRF token",
					"path", r.URL.Path, "guardian", sess.Guardian)
				http.Error(w, "invalid form token — reload the page and try again", http.StatusForbidden)
				return
			}
		}

		h(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, sess)))
	}
}

// handleLoginForm renders the login page. An already-valid session is bounced
// to the dashboard rather than shown a second login box.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, err := s.readSession(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, "")
}

// handleLogin implements §9.2's login: constant-time verification (in
// internal/guardians), a ~1 s fixed delay on failure, and per-account
// exponential backoff after five consecutive failures.
//
// It has no CSRF token — there is no session to bind one to — so the origin
// check is its whole defense; see csrf.go.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkOrigin(r) {
		s.log.Warn("web: rejected a login with a bad or missing origin")
		http.Error(w, "request origin rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)

	name := r.PostFormValue("guardian")
	password := r.PostFormValue("password")

	if allowed, retryAfter := s.throttle.check(name); !allowed {
		// Deliberately answered before the password is looked at, so a
		// locked-out account costs an attacker nothing of ours and tells them
		// nothing about whether the password was right.
		s.log.Warn("web: login attempt refused by backoff", "guardian", name,
			"retry_after_s", int(retryAfter.Seconds()+0.5))
		w.Header().Set("Retry-After", formatSeconds(retryAfter))
		s.renderLoginStatus(w, r, http.StatusTooManyRequests,
			"Too many failed sign-ins for that account. Try again in "+humanDuration(retryAfter)+".")
		return
	}

	g, err := s.guardians.Verify(name, password)
	if err != nil {
		if !errors.Is(err, guardians.ErrBadCredentials) {
			s.log.Error("web: login verification failed unexpectedly", "error", err)
		}
		// The fixed delay runs before the response on every failure, so the
		// guess rate is capped from the very first attempt rather than only
		// once the counter trips.
		s.sleep(failureDelay)
		backoff := s.throttle.fail(name)
		s.log.Info("web: failed sign-in", "guardian", name, "locked_for_s", int(backoff.Seconds()))
		s.renderLoginStatus(w, r, http.StatusUnauthorized, "Incorrect username or password.")
		return
	}

	s.throttle.succeed(name)
	value, expiry := s.mintSession(g)
	s.setSessionCookie(w, value, expiry)
	s.log.Info("web: signed in", "guardian", g.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout clears the cookie. It is a POST — a logout reachable by GET is
// a link an image tag can pull, which is only annoying here but is the same
// mistake that matters elsewhere (§9.2).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	s.redirect(w, r, "/login", flashSignedOut)
}

// formatSeconds renders a Retry-After header value: whole seconds, rounded up,
// never below one.
func formatSeconds(d time.Duration) string {
	secs := int(d.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}
