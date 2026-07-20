package authtest

import (
	"net/http"
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/auth"
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

	// Wrong passwords are rejected regardless of length: the comparison
	// hashes both sides to a fixed width first, so a same-length, a shorter,
	// and a much longer guess all fail the same way (and none can be told
	// apart from the others by timing).
	for name, r := range map[string]*http.Request{
		"no credentials":     req("", "", false),
		"wrong same length":  req("alice", "pw2", true),
		"wrong shorter":      req("alice", "p", true),
		"wrong much longer":  req("alice", "pw1pw1pw1pw1pw1pw1pw1", true),
		"unknown user":       req("bob", "pw1", true),
		"empty password":     req("alice", "", true),
		"empty pass unknown": req("bob", "", true),
	} {
		if _, err := s.Authenticate(r); err != auth.ErrUnauthenticated {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", name, err)
		}
	}
}
