// Package auth defines the authentication socket. The runtime forces
// authentication to exist (RFC 8620 requires it) but not its nature:
// embedders implement Authenticator however they like; the runtime only
// ever asks "whose request is this, and which accounts may it touch?".
package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// Access describes one account an identity can use.
type Access struct {
	// Name is the user-facing account name (e.g. an email address).
	Name string
	// Personal is true if the account belongs to the identity itself.
	Personal bool
	// ReadOnly is true if the identity may not modify the account.
	ReadOnly bool
}

// Identity is an authenticated caller and its account access. Identity
// and account are deliberately distinct concepts: an identity may reach
// several accounts, and an account may be reachable by several
// identities.
type Identity struct {
	// Username is reported in the session resource; may be empty.
	Username string
	// Credential identifies the specific credential that authenticated,
	// when that is finer-grained than the identity (e.g. one API token
	// among several). Push subscriptions are tied to it (RFC 8620
	// section 7.2): only the creating credential sees or updates them.
	// Empty means the credential IS the identity and Username is used.
	Credential string
	// Accounts maps each reachable account to the identity's access.
	Accounts map[jmap.Id]Access
	// Primary is the identity's default account; it should be a key of
	// Accounts. Capability specs use it for primaryAccounts entries.
	Primary jmap.Id
}

// CredentialKey returns the key push subscriptions are tied to:
// Credential when set, Username otherwise.
func (i *Identity) CredentialKey() string {
	if i.Credential != "" {
		return i.Credential
	}
	return i.Username
}

// ErrUnauthenticated is returned when the request carries no valid
// credentials; the server answers 401.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Authenticator is the socket: given an HTTP request, identify the
// caller or reject with ErrUnauthenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (*Identity, error)
}

// Static authenticates HTTP Basic credentials against a fixed in-memory
// user set. It is for tests and development only: passwords are held in
// plain text and compared in constant time, but never hashed.
type Static struct {
	users map[string]staticUser
}

type staticUser struct {
	password string
	identity Identity
}

// NewStatic returns an empty Static authenticator.
func NewStatic() *Static {
	return &Static{users: make(map[string]staticUser)}
}

// AddUser registers username/password with a single personal account.
func (s *Static) AddUser(username, password string, accountID jmap.Id) {
	s.users[username] = staticUser{
		password: password,
		identity: Identity{
			Username: username,
			Accounts: map[jmap.Id]Access{accountID: {Name: username, Personal: true}},
			Primary:  accountID,
		},
	}
}

// AddAccess grants an already-added user access to a further account,
// e.g. a shared team account. Unknown usernames are ignored.
func (s *Static) AddAccess(username string, accountID jmap.Id, access Access) {
	if u, ok := s.users[username]; ok {
		u.identity.Accounts[accountID] = access
	}
}

// Authenticate implements Authenticator.
func (s *Static) Authenticate(r *http.Request) (*Identity, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, ErrUnauthenticated
	}
	u, found := s.users[username]
	// Compare even for unknown users so timing does not reveal existence.
	expected := u.password
	if !found {
		expected = ""
	}
	match := subtle.ConstantTimeCompare([]byte(password), []byte(expected)) == 1
	if !found || !match {
		return nil, ErrUnauthenticated
	}
	id := u.identity
	return &id, nil
}
