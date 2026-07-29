package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/tholent/chaskiwasi/internal/guardians"
)

type guardianView struct {
	Name              string
	CreatedAt         time.Time
	PasswordChangedAt time.Time
	IsYou             bool
}

type accountPage struct {
	layout
	Guardians      []guardianView
	MinPasswordLen int
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	page := accountPage{
		layout:         s.newLayout(r, sess, "Guardians", "account"),
		MinPasswordLen: guardians.MinPasswordLen,
	}
	for _, g := range s.guardians.List() {
		page.Guardians = append(page.Guardians, guardianView{
			Name:              g.Name,
			CreatedAt:         g.CreatedAt,
			PasswordChangedAt: g.PasswordChangedAt,
			IsYou:             g.Name == sess.Guardian,
		})
	}
	s.page(w, http.StatusOK, "account.html", page)
}

// handlePasswordChange is the self-service half of §9.2. Changing a password
// increments the account's session epoch, which invalidates every session
// cookie previously issued for it — including, deliberately, the one that made
// this very request, so the guardian is signed out and back in with a fresh
// cookie. A lock change that leaves old keys working is not a lock change
// (V-19).
//
// Only the guardian's own password can be changed here. A guardian who could
// reset another guardian's password could lock a co-parent out of the record
// of their own child's contact list, which is the hostile-household scenario
// this whole surface exists to survive; the recovery path for a forgotten
// password is `wasi useradd` at the host, which requires access to the machine.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	if next != confirm {
		s.redirect(w, r, "/account", flashPasswordMismatch)
		return
	}
	if _, err := s.guardians.Verify(sess.Guardian, current); err != nil {
		// Re-proving the current password is what stops an unattended browser
		// from becoming a permanent takeover of the account.
		s.log.Warn("web: password change refused, current password wrong", "guardian", sess.Guardian)
		s.redirect(w, r, "/account", flashPasswordWrong)
		return
	}

	g, err := s.guardians.SetPassword(sess.Guardian, next)
	if err != nil {
		if errors.Is(err, guardians.ErrWeakPassword) {
			s.redirect(w, r, "/account", flashPasswordWeak)
			return
		}
		s.log.Error("web: password change failed", "guardian", sess.Guardian, "error", err)
		s.redirect(w, r, "/account", flashPasswordWrong)
		return
	}

	s.log.Info("web: password changed", "guardian", g.Name, "session_epoch", g.SessionEpoch)

	// Issue a cookie carrying the new epoch so the guardian who just changed
	// their password stays signed in on this browser and nowhere else. Skipping
	// this would sign them out too, which is defensible but reads as a bug and
	// invites people to avoid changing passwords.
	value, expiry := s.mintSession(g)
	s.setSessionCookie(w, value, expiry)

	s.redirect(w, r, "/account", flashPasswordChanged)
}

// handleGuardianAdd creates another guardian account from the UI. `wasi
// useradd` does the same thing from a shell (§9.2, §14) and is the path that
// works when nobody can sign in.
func (s *Server) handleGuardianAdd(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	name := r.PostFormValue("guardian")
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm_password")

	if password != confirm {
		s.redirect(w, r, "/account", flashPasswordMismatch)
		return
	}

	g, err := s.guardians.Add(name, password)
	switch {
	case errors.Is(err, guardians.ErrExists):
		s.redirect(w, r, "/account", flashGuardianExists)
		return
	case errors.Is(err, guardians.ErrInvalidName):
		s.redirect(w, r, "/account", flashGuardianInvalid)
		return
	case errors.Is(err, guardians.ErrWeakPassword):
		s.redirect(w, r, "/account", flashPasswordWeak)
		return
	case err != nil:
		s.log.Error("web: creating a guardian failed", "error", err)
		s.redirect(w, r, "/account", flashGuardianInvalid)
		return
	}

	s.log.Info("web: guardian account created", "guardian", g.Name, "created_by", sess.Guardian)
	s.redirect(w, r, "/account", flashGuardianAdded)
}

// renderLogin renders the sign-in page.
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, msg string) {
	s.renderLoginStatus(w, r, http.StatusOK, msg)
}

// renderLoginStatus renders the sign-in page with an explicit status code, so
// a throttled attempt answers 429 and a bad password answers 401 rather than
// both quietly answering 200.
func (s *Server) renderLoginStatus(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.page(w, status, "login.html", loginPage{
		layout: layout{Title: "Sign in", Flash: flashFor(r)},
		Error:  msg,
	})
}

type loginPage struct {
	layout
	Error string
}
