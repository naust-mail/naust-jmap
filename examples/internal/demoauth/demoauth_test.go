package demoauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func basic(username, password string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	req.SetBasicAuth(username, password)
	return req
}

func TestAuthenticate(t *testing.T) {
	a := New(Fast())
	a.AddUser("john@example.com", "secret", "A1")

	id, err := a.Authenticate(basic("john@example.com", "secret"))
	if err != nil || id.Username != "john@example.com" {
		t.Fatalf("id=%v err=%v", id, err)
	}
	if _, err := a.Authenticate(basic("john@example.com", "wrong")); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := a.Authenticate(basic("ghost@example.com", "secret")); err == nil {
		t.Fatal("unknown user accepted")
	}
	if _, err := a.Authenticate(httptest.NewRequest(http.MethodPost, "/api", nil)); err == nil {
		t.Fatal("missing credentials accepted")
	}
}

// A repeated correct password must hit the verdict cache: exactly one
// KDF run for any number of requests, and a wrong password must never
// ride the cache.
func TestVerdictCacheSkipsKDF(t *testing.T) {
	a := New(Fast())
	a.AddUser("john@example.com", "secret", "A1")
	base := a.kdfCalls.Load() // AddUser hashed once

	for i := 0; i < 5; i++ {
		if _, err := a.Authenticate(basic("john@example.com", "secret")); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.kdfCalls.Load() - base; got != 1 {
		t.Fatalf("5 identical logins cost %d KDF runs, want 1", got)
	}

	// A wrong password fails and pays the KDF, and does not poison the
	// cache for the right one.
	if _, err := a.Authenticate(basic("john@example.com", "nope")); err == nil {
		t.Fatal("wrong password accepted with a warm cache")
	}
	if got := a.kdfCalls.Load() - base; got != 2 {
		t.Fatalf("wrong password cost %d extra KDF runs, want 1", got-1)
	}
	if _, err := a.Authenticate(basic("john@example.com", "secret")); err != nil {
		t.Fatal("correct password rejected after a failed attempt")
	}
	if got := a.kdfCalls.Load() - base; got != 2 {
		t.Fatalf("cached verdict lost after a failed attempt (%d KDF runs)", got)
	}
}

// A password change must invalidate the cached verdict immediately: the
// old password dies on its next use even though it verified moments
// ago, and the new one pays a fresh KDF before being cached.
func TestSetPasswordInvalidatesCache(t *testing.T) {
	a := New(Fast())
	a.AddUser("john@example.com", "old-password", "A1")
	if _, err := a.Authenticate(basic("john@example.com", "old-password")); err != nil {
		t.Fatal(err)
	}

	a.SetPassword("john@example.com", "new-password")

	if _, err := a.Authenticate(basic("john@example.com", "old-password")); err == nil {
		t.Fatal("old password survived a password change")
	}
	base := a.kdfCalls.Load()
	if _, err := a.Authenticate(basic("john@example.com", "new-password")); err != nil {
		t.Fatal("new password rejected")
	}
	if got := a.kdfCalls.Load() - base; got != 1 {
		t.Fatalf("first login with the new password cost %d KDF runs, want 1", got)
	}
	if _, err := a.Authenticate(basic("john@example.com", "new-password")); err != nil {
		t.Fatal(err)
	}
	if got := a.kdfCalls.Load() - base; got != 1 {
		t.Fatal("new password's verdict not cached")
	}

	a.SetPassword("ghost@example.com", "whatever") // unknown user: no-op
}

// Concurrent logins and password changes must be race-free and never
// authenticate a stale password.
func TestConcurrentAuthAndChange(t *testing.T) {
	a := New(Fast())
	for i := 0; i < 4; i++ {
		a.AddUser(fmt.Sprintf("user%d@example.com", i), "pw", "A1")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		u := fmt.Sprintf("user%d@example.com", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				a.Authenticate(basic(u, "pw"))
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				a.SetPassword(u, "pw")
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		u := fmt.Sprintf("user%d@example.com", i)
		if _, err := a.Authenticate(basic(u, "pw")); err != nil {
			t.Fatalf("%s cannot log in after churn: %v", u, err)
		}
	}
}
