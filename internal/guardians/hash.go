package guardians

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (§9.2, §12.4). Chosen deliberately, and the reasoning
// matters more than the numbers:
//
//   - Memory (64 MiB) is the only parameter that actually prices a GPU or ASIC
//     attack; time and parallelism can be bought cheaply by an attacker with
//     silicon, memory bandwidth cannot. It is therefore set as high as the
//     deployment can stand rather than as low as it can get away with.
//   - 64 MiB is what the deployment can stand: this is a home container with
//     two or three accounts, and §9.2's throttling caps how many logins can be
//     in flight at once, so the worst case is a handful of concurrent 64 MiB
//     allocations — nothing next to the mailbox client. Raising it to 256 MiB
//     would be better cryptography and worse operations on the small ARM boxes
//     this is meant to run on, which is a trade this scale does not need to make.
//   - t=3 with m=64 MiB is the OWASP argon2id baseline and lands around
//     50-100 ms per verification on that class of hardware: imperceptible to a
//     guardian logging in, ruinous to anyone grinding a stolen guardians.toml.
//   - p=4 matches the parallelism a small box can actually deliver; more
//     threads than cores buys nothing.
//
// These travel inside the encoded hash (PHC format), so raising them later
// applies to new and changed passwords without invalidating existing accounts.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// argonVersion is the argon2 version number the PHC string records. The
// x/crypto implementation is 0x13 (19) and has been since it landed.
const argonVersion = argon2.Version

// decoyHash is verified against when the supplied guardian name does not
// exist, so an unknown name costs exactly the same argon2id work as a wrong
// password. Without it, a login form is an account-name oracle measurable with
// a stopwatch — which in a hostile household is a genuinely useful signal to
// the wrong person.
//
// It is built lazily rather than at init: a hash costs 64 MiB and ~100 ms, and
// most processes that link this package (`wasi backup`, `wasi contacts`) never
// verify a password at all.
var decoyHash = sync.OnceValue(func() string {
	var pw [32]byte
	if _, err := rand.Read(pw[:]); err != nil {
		panic("guardians: no entropy for the timing decoy: " + err.Error())
	}
	h, err := hashPassword(string(pw[:]))
	if err != nil {
		panic("guardians: cannot build the timing decoy: " + err.Error())
	}
	return h
})

// hashPassword derives a PHC-encoded argon2id hash with a fresh random salt.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("guardians: reading salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches the PHC-encoded argon2id
// string in encoded, using the parameters recorded inside it rather than the
// current constants — that is what lets the cost be raised without a flag day.
//
// The comparison is constant-time. A malformed encoded string is a false, not
// an error the caller could branch on: a corrupted row must fail login, not
// change the shape of the response.
func verifyPassword(encoded, password string) bool {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

var errBadHashFormat = errors.New("guardians: unrecognised password hash format")

// decodeHash parses "$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>".
func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// A leading "$" yields an empty first field, hence six parts.
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, errBadHashFormat
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, errBadHashFormat
	}
	if version != argonVersion {
		return argonParams{}, nil, nil, fmt.Errorf("guardians: unsupported argon2 version %d", version)
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, errBadHashFormat
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return argonParams{}, nil, nil, errBadHashFormat
	}

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, errBadHashFormat
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return argonParams{}, nil, nil, errBadHashFormat
	}
	return p, salt, key, nil
}
