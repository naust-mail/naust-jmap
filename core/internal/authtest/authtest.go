// Package authtest provides a trivial in-memory Authenticator for the
// core module's own tests. It is module-private (internal) and never
// shipped: passwords are held in plain text and compared in constant
// time but never hashed, which is acceptable only because this exists
// purely as a test fixture. Real embedders implement auth.Authenticator
// against their own credential store; the examples show a hashed
// reference (argon2id).
package authtest

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

// Static authenticates HTTP Basic credentials against a fixed in-memory
// user set.
type Static struct {
	users map[string]staticUser
}

type staticUser struct {
	password string
	identity auth.Identity
}

// NewStatic returns an empty Static authenticator.
func NewStatic() *Static {
	return &Static{users: make(map[string]staticUser)}
}

// AddUser registers username/password with a single personal account.
func (s *Static) AddUser(username, password string, accountID jmap.Id) {
	s.users[username] = staticUser{
		password: password,
		identity: auth.Identity{
			Username: username,
			Accounts: map[jmap.Id]auth.Access{accountID: {Name: username, Personal: true}},
			Primary:  accountID,
		},
	}
}

// AddAccess grants an already-added user access to a further account,
// e.g. a shared team account. Unknown usernames are ignored.
func (s *Static) AddAccess(username string, accountID jmap.Id, access auth.Access) {
	if u, ok := s.users[username]; ok {
		u.identity.Accounts[accountID] = access
	}
}

// Authenticate implements auth.Authenticator.
func (s *Static) Authenticate(r *http.Request) (*auth.Identity, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	u, found := s.users[username]
	// Compare even for unknown users so timing does not reveal existence.
	expected := u.password
	if !found {
		expected = ""
	}
	// Compare fixed-width digests, not the raw strings: subtle.ConstantTimeCompare
	// returns early when its inputs differ in length, so comparing the passwords
	// directly would leak the expected length through timing. Hashing both sides
	// to 32 octets first makes the comparison length-independent.
	got := sha256.Sum256([]byte(password))
	want := sha256.Sum256([]byte(expected))
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	if !found || !match {
		return nil, auth.ErrUnauthenticated
	}
	id := u.identity
	return &id, nil
}
