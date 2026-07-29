// Package guardians owns /data/guardians.toml: one row per guardian account,
// holding an argon2id password hash and a session epoch (wasi-server-plan
// §9.2, §12.4). It is written by `wasi useradd` and by self-service password
// change in the guardian UI, and by nothing else.
//
// # Why there is an epoch at all
//
// Sessions are stateless signed cookies (§9.2) — there is no session store to
// delete rows from, so "log everyone out of this account" has to be
// expressible in the data the cookie carries. The per-guardian SessionEpoch is
// that expression: it is stamped into every cookie at login and incremented on
// every password change, so a cookie issued before the change verifies its MAC
// perfectly and is still rejected for carrying a stale epoch.
//
// This is not a nicety. It is the hostile-household case the design spec
// contemplates: a lock change that leaves old keys working is not a lock
// change. (Test V-19.)
//
// # What this package deliberately does not do
//
// It does not know about cookies, HTTP, or throttling — internal/web owns all
// three. It hands out a Guardian (name + epoch) on a correct password and an
// error otherwise, and that is the whole contract.
package guardians

import (
	"errors"
	"time"
)

// Guardian is one row of guardians.toml.
//
// PasswordHash is a PHC-encoded argon2id string (see hash.go); the parameters
// travel inside it, so raising the cost later re-hashes on next password
// change without invalidating existing accounts.
type Guardian struct {
	Name         string `toml:"name"`
	PasswordHash string `toml:"password_hash"`

	// SessionEpoch is stamped into every session cookie issued for this
	// account and incremented on every password change (§9.2). It starts at 1
	// so that a zero value read from a hand-mangled file is never mistaken for
	// a valid epoch.
	SessionEpoch int `toml:"session_epoch"`

	CreatedAt         time.Time `toml:"created_at"`
	PasswordChangedAt time.Time `toml:"password_changed_at"`
}

// Store is the guardian account table (§9.2). Every method is safe for
// concurrent use: the web UI serves logins and password changes from
// independent request goroutines.
type Store interface {
	// List returns every guardian, name-sorted. Password hashes are included
	// in the struct but no caller should render them; the UI shows names,
	// creation dates, and nothing else.
	List() []Guardian

	// Get returns one guardian by name. The bool is false for an unknown name.
	Get(name string) (Guardian, bool)

	// Add creates a new account. It fails with ErrExists rather than
	// overwriting: silently resetting an existing guardian's password is
	// exactly the move a hostile household member would want from `useradd`.
	Add(name, password string) (Guardian, error)

	// SetPassword replaces a guardian's password and increments SessionEpoch,
	// which invalidates every session cookie previously issued for that
	// account (§9.2, V-19). It returns the guardian as it now stands.
	SetPassword(name, password string) (Guardian, error)

	// Verify checks a password in constant time and returns the matching
	// guardian. It reports ErrBadCredentials for both an unknown name and a
	// wrong password, and spends the same argon2id work in both cases, so a
	// login form cannot be used to enumerate account names.
	Verify(name, password string) (Guardian, error)
}

var (
	// ErrBadCredentials is returned for an unknown guardian and for a wrong
	// password alike. Callers must not distinguish the two in any response.
	ErrBadCredentials = errors.New("guardians: incorrect username or password")

	// ErrNoSuchGuardian is returned by SetPassword for a name that does not
	// exist. It is safe to surface: the caller is already authenticated as a
	// guardian by the time they can reach a password change.
	ErrNoSuchGuardian = errors.New("guardians: no such guardian")

	// ErrExists is returned by Add for a name already in the table.
	ErrExists = errors.New("guardians: that guardian already exists")

	// ErrInvalidName is returned for a name outside the accepted character set.
	ErrInvalidName = errors.New("guardians: name must be 1-32 characters of a-z, 0-9, dot, dash or underscore")

	// ErrWeakPassword is returned for a password shorter than MinPasswordLen.
	ErrWeakPassword = errors.New("guardians: password is too short")
)

// MinPasswordLen is the only password rule this server imposes. Composition
// rules ("one digit, one symbol") are well documented to produce shorter and
// more predictable passwords, so length is the single lever: argon2id prices
// the offline attack and login throttling prices the online one (§9.2), which
// leaves length as the thing neither of those can supply.
const MinPasswordLen = 12
