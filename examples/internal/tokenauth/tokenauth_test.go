package tokenauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// login runs the mint endpoint and returns the bearer token.
func login(t *testing.T, a *Authenticator, username, password string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.SetBasicAuth(username, password)
	rec := httptest.NewRecorder()
	a.LoginHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

// bearer builds a request carrying the token.
func bearer(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestMintAndAuthenticate(t *testing.T) {
	a := New()
	a.AddUser("john@example.com", "secret", "A1")
	tok := login(t, a, "john@example.com", "secret")
	id, err := a.Authenticate(bearer(tok))
	if err != nil || id.Username != "john@example.com" {
		t.Fatalf("id=%v err=%v", id, err)
	}
	if _, err := a.Authenticate(bearer("not-a-token")); err == nil {
		t.Fatal("bogus token accepted")
	}
}

// RevokeUser must kill every minted token for that user immediately and
// emit the revocation to subscribers, while other users' tokens and no
// one else's connections are touched.
func TestRevokeUser(t *testing.T) {
	a := New()
	a.AddUser("john@example.com", "secret", "A1")
	a.AddUser("jane@example.com", "hunter2", "A2")
	johnTok1 := login(t, a, "john@example.com", "secret")
	johnTok2 := login(t, a, "john@example.com", "secret")
	janeTok := login(t, a, "jane@example.com", "hunter2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := a.Revocations(ctx)

	a.RevokeUser("john@example.com")

	for _, tok := range []string{johnTok1, johnTok2} {
		if _, err := a.Authenticate(bearer(tok)); err == nil {
			t.Fatal("revoked user's token still authenticates")
		}
	}
	if _, err := a.Authenticate(bearer(janeTok)); err != nil {
		t.Fatal("revocation of one user broke another's token")
	}
	select {
	case got := <-events:
		if got != "john@example.com" {
			t.Fatalf("revocation for %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation never emitted")
	}

	// Ending the subscription closes the stream after unregistering.
	cancel()
	select {
	case _, open := <-events:
		if open {
			t.Fatal("unexpected value after context end")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream not closed after context end")
	}
}

// A revocation with no subscribers (or for an unknown user) is a no-op,
// never a block or panic.
func TestRevokeUserNoSubscribers(t *testing.T) {
	a := New()
	a.AddUser("john@example.com", "secret", "A1")
	a.RevokeUser("john@example.com")
	a.RevokeUser("ghost@example.com")
}
