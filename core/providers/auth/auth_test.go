package auth

import (
	"net/http"
	"testing"
)

func TestStaticAuthenticate(t *testing.T) {
	s := NewStatic()
	s.AddUser("alice", "pw1", "Aalice")

	req := func(user, pass string, withCreds bool) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "https://example.com/.well-known/jmap", nil)
		if withCreds {
			r.SetBasicAuth(user, pass)
		}
		return r
	}

	ident, err := s.Authenticate(req("alice", "pw1", true))
	if err != nil {
		t.Fatalf("valid creds rejected: %v", err)
	}
	if ident.Username != "alice" || ident.Primary != "Aalice" {
		t.Errorf("identity = %+v", ident)
	}
	acc, ok := ident.Accounts["Aalice"]
	if !ok || !acc.Personal || acc.ReadOnly {
		t.Errorf("access = %+v", ident.Accounts)
	}

	for name, r := range map[string]*http.Request{
		"no credentials": req("", "", false),
		"wrong password": req("alice", "nope", true),
		"unknown user":   req("bob", "pw1", true),
		"empty password": req("alice", "", true),
	} {
		if _, err := s.Authenticate(r); err != ErrUnauthenticated {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", name, err)
		}
	}
}
