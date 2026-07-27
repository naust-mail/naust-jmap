package auth_test

import (
	"net/http"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

// staticAuth is the smallest useful Authenticator: one bearer token, one
// account. Real implementations verify against a credential store, but
// the shape is the same - identify the caller, or return
// ErrUnauthenticated.
//
// The runtime never sees the credential itself. It asks only "whose
// request is this, and which accounts may it touch?", and the Accounts
// map is that answer: every account the identity can reach, and whether
// it may write to each.
type staticAuth struct {
	token   string
	account jmap.Id
}

func (a staticAuth) Authenticate(r *http.Request) (*auth.Identity, error) {
	if r.Header.Get("Authorization") != "Bearer "+a.token {
		return nil, auth.ErrUnauthenticated
	}
	return &auth.Identity{
		Username: "user@example.com",
		Accounts: map[jmap.Id]auth.Access{
			a.account: {Name: "user@example.com", Personal: true},
		},
		Primary: a.account,
	}, nil
}

func ExampleAuthenticator() {
	var authn auth.Authenticator = staticAuth{
		token:   "s3cret",
		account: jmap.Id("A1"),
	}

	// Hand it to runtime.NewServer; the runtime calls Authenticate once
	// per request and rejects anything that returns ErrUnauthenticated.
	_ = authn
}
