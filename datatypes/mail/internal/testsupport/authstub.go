// Package testsupport holds fixtures shared by more than one of this
// module's test packages. It exists only for _test.go files: nothing in the
// module's non-test build may import it (see internal/depsguard).
package testsupport

// StaticAuth is a trivial in-memory Authenticator for this module's tests
// only. It mirrors the core module's internal test fixture: core keeps its
// copy module-private (core/internal/authtest), which a separate module
// cannot import, so the mail tests carry their own. Passwords are held in
// plain text and compared in constant time but never hashed - acceptable
// only because this is test scaffolding, never shipped.

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

type staticAuthUser struct {
	password string
	identity auth.Identity
}

// StaticAuth is the shared fixture type; see the package-level doc.
type StaticAuth struct {
	users map[string]staticAuthUser
}

// NewStaticAuth returns an empty StaticAuth: no users, everything
// unauthenticated.
func NewStaticAuth() *StaticAuth {
	return &StaticAuth{users: make(map[string]staticAuthUser)}
}

// AddUser registers a user with one primary, personal account.
func (s *StaticAuth) AddUser(username, password string, accountID jmap.Id) {
	s.users[username] = staticAuthUser{
		password: password,
		identity: auth.Identity{
			Username: username,
			Accounts: map[jmap.Id]auth.Access{accountID: {Name: username, Personal: true}},
			Primary:  accountID,
		},
	}
}

// AddAccess grants an existing user access to another account.
func (s *StaticAuth) AddAccess(username string, accountID jmap.Id, access auth.Access) {
	if u, ok := s.users[username]; ok {
		u.identity.Accounts[accountID] = access
	}
}

// Authenticate implements the auth.Authenticator socket via HTTP Basic.
func (s *StaticAuth) Authenticate(r *http.Request) (*auth.Identity, error) {
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
	// Hash both sides to a fixed width first so the constant-time compare is
	// length-independent (see the core fixture for the full rationale).
	got := sha256.Sum256([]byte(password))
	want := sha256.Sum256([]byte(expected))
	if !found || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return nil, auth.ErrUnauthenticated
	}
	id := u.identity
	return &id, nil
}
