// Package demoauth is a small reference auth.Authenticator for the
// examples: it verifies HTTP Basic credentials against argon2id password
// hashes, so copying it does not teach a footgun.
//
// Two things a real deployment does differently, and that this omits on
// purpose to stay small:
//
//  1. Users come from a real credential store, not AddUser calls.
//  2. The runtime authenticates EVERY request. Verifying a password (and
//     thus running argon2id) per request is far too expensive at scale,
//     so production verifies a cheap bearer TOKEN here and runs the KDF
//     only at a separate login endpoint that mints the token. This demo
//     hashes per request, which is fine only because it is a demo.
package demoauth

import (
	"crypto/rand"
	"crypto/subtle"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"golang.org/x/crypto/argon2"
)

// Params are the argon2id cost parameters. Higher is more resistant to
// offline cracking but slower per verification.
type Params struct {
	Time    uint32 // number of passes
	Memory  uint32 // memory in KiB
	Threads uint8  // parallelism
}

// Default follows the OWASP argon2id guidance (~19 MiB, 2 passes). Use it
// for anything holding real credentials.
func Default() Params { return Params{Time: 2, Memory: 19 * 1024, Threads: 1} }

// Fast is a deliberately cheap cost for benchmarks and tests, where the
// KDF is not what is being measured. Never use it for real credentials.
func Fast() Params { return Params{Time: 1, Memory: 8 * 1024, Threads: 1} }

// dummySalt lets an unknown user cost the same argon2id work as a known
// one, so user existence does not leak through response timing.
var dummySalt = []byte("naust-demo-salt.")

type user struct {
	salt     []byte
	hash     []byte
	identity auth.Identity
}

// Authenticator verifies HTTP Basic credentials against argon2id hashes.
type Authenticator struct {
	params Params
	users  map[string]user
}

// New returns an empty Authenticator using the given cost parameters.
func New(p Params) *Authenticator {
	return &Authenticator{params: p, users: make(map[string]user)}
}

// AddUser registers username/password with a single personal account. The
// password is hashed now; the plain text is not retained.
func (a *Authenticator) AddUser(username, password string, accountID jmap.Id) {
	salt := make([]byte, 16)
	rand.Read(salt) // crypto/rand.Read never returns an error
	a.users[username] = user{
		salt: salt,
		hash: a.hash(password, salt),
		identity: auth.Identity{
			Username: username,
			Accounts: map[jmap.Id]auth.Access{accountID: {Name: username, Personal: true}},
			Primary:  accountID,
		},
	}
}

func (a *Authenticator) hash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, a.params.Time, a.params.Memory, a.params.Threads, 32)
}

// Authenticate implements auth.Authenticator.
func (a *Authenticator) Authenticate(r *http.Request) (*auth.Identity, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	u, found := a.users[username]
	// Always run the KDF, against a throwaway salt for unknown users, so a
	// miss costs the same as a hit and existence stays hidden.
	salt := u.salt
	if !found {
		salt = dummySalt
	}
	got := a.hash(password, salt)
	if !found || subtle.ConstantTimeCompare(got, u.hash) != 1 {
		return nil, auth.ErrUnauthenticated
	}
	id := u.identity
	return &id, nil
}
