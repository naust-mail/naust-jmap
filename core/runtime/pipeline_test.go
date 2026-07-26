package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

func testIdentity(username string) *auth.Identity {
	return &auth.Identity{
		Username:   username,
		Credential: username,
		Accounts:   map[jmap.Id]auth.Access{"Atest1": {Name: username}},
		Primary:    "Atest1",
	}
}

// With the default derivation, a pool of 4 gives each user a share of
// 2: the third concurrent request from one user must be refused while
// the pool itself still has room for other users.
func TestPerUserSlotShare(t *testing.T) {
	srv := registrarServer(t) // MaxConcurrentRequests 4, per-user share 2
	john := testIdentity("john@example.com")
	jane := testIdentity("jane@example.com")

	s1 := srv.TryAcquireSlot(john)
	s2 := srv.TryAcquireSlot(john)
	if s1 == nil || s2 == nil {
		t.Fatal("user share of 2 refused a slot early")
	}
	if s3 := srv.TryAcquireSlot(john); s3 != nil {
		t.Fatal("third concurrent slot for one user exceeded the per-user share")
	}
	other := srv.TryAcquireSlot(jane)
	if other == nil {
		t.Fatal("pool had room but another user was refused")
	}
	s1.Release()
	if s4 := srv.TryAcquireSlot(john); s4 == nil {
		t.Fatal("released slot not returned to the user share")
	} else {
		s4.Release()
	}
	s2.Release()
	other.Release()
}

// The pool bound applies across users: once maxConcurrentRequests
// slots are held, further users are refused even with per-user share
// to spare.
func TestSharedPoolBound(t *testing.T) {
	srv := registrarServer(t)
	var held []*RequestSlot
	for i := 0; i < 2; i++ {
		u := testIdentity(fmt.Sprintf("user%d@example.com", i))
		for j := 0; j < 2; j++ {
			s := srv.TryAcquireSlot(u)
			if s == nil {
				t.Fatalf("pool refused slot %d for user %d before the bound", j, i)
			}
			held = append(held, s)
		}
	}
	if s := srv.TryAcquireSlot(testIdentity("late@example.com")); s != nil {
		t.Fatal("slot granted beyond maxConcurrentRequests")
	}
	for _, s := range held {
		s.Release()
	}
}

// AcquireSlot waits instead of refusing: a blocked acquire completes
// when a slot frees, and a canceled context aborts the wait cleanly.
func TestAcquireSlotBlocksAndCancels(t *testing.T) {
	srv := registrarServer(t)
	john := testIdentity("john@example.com")
	s1, err := srv.AcquireSlot(context.Background(), john)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := srv.AcquireSlot(context.Background(), john)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan *RequestSlot)
	go func() {
		s, err := srv.AcquireSlot(context.Background(), john)
		if err != nil {
			t.Error(err)
		}
		got <- s
	}()
	select {
	case <-got:
		t.Fatal("acquire beyond the user share did not block")
	case <-time.After(50 * time.Millisecond):
	}
	s1.Release()
	select {
	case s := <-got:
		s.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("blocked acquire never completed after a release")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.AcquireSlot(ctx, john); err == nil {
		t.Fatal("canceled context still acquired a slot")
	}
	s2.Release()

	srv.userMu.Lock()
	defer srv.userMu.Unlock()
	if len(srv.userSlots) != 0 {
		t.Fatalf("user slot entries leaked: %v", srv.userSlots)
	}
}

// Process stamps every response with the identity's sessionState
// (RFC 8620 section 3.4) and reports request-level problems as
// section 3.6.1 request errors.
func TestSlotProcess(t *testing.T) {
	srv := registrarServer(t)
	john := testIdentity("john@example.com")
	slot := srv.TryAcquireSlot(john)
	if slot == nil {
		t.Fatal("no slot")
	}
	defer slot.Release()

	echo := fmt.Sprintf(`{"using":[%q],"methodCalls":[["Core/echo",{"hi":1},"c0"]]}`, jmap.CoreCapability)
	resp, rerr := slot.Process(context.Background(), []byte(echo))
	if rerr != nil {
		t.Fatalf("echo failed: %+v", rerr)
	}
	if resp.SessionState == "" || resp.SessionState != srv.session(john).State {
		t.Errorf("sessionState = %q, want %q", resp.SessionState, srv.session(john).State)
	}
	if len(resp.MethodResponses) != 1 || resp.MethodResponses[0].Name != "Core/echo" {
		t.Errorf("responses: %+v", resp.MethodResponses)
	}

	for _, tc := range []struct {
		name, body, wantType string
	}{
		{"not json", "the quick brown fox", jmap.ProblemNotJSON},
		{"json but not a request", `{"hello":1}`, jmap.ProblemNotRequest},
		{"unknown capability", `{"using":["urn:example:absent"],"methodCalls":[]}`, jmap.ProblemUnknownCapability},
		{"too many calls", fmt.Sprintf(`{"using":[%q],"methodCalls":[%s]}`, jmap.CoreCapability,
			strings.Repeat(`["Core/echo",{},"c"],`, 16)+`["Core/echo",{},"c"]`), jmap.ProblemLimit},
		{"oversized body", fmt.Sprintf(`{"using":[%q],"methodCalls":[["Core/echo",{"pad":%q},"c0"]]}`,
			jmap.CoreCapability, strings.Repeat("x", 10_000_001)), jmap.ProblemLimit},
	} {
		_, rerr := slot.Process(context.Background(), []byte(tc.body))
		if rerr == nil || rerr.Type != tc.wantType {
			t.Errorf("%s: got %+v, want type %s", tc.name, rerr, tc.wantType)
		}
	}
}

// The size gate is exact: a body of exactly maxSizeRequest bytes is
// processed normally, while one byte more is refused with a limit
// request error naming "maxSizeRequest" (RFC 8620 section 3.6.1).
func TestSlotProcessSizeBoundary(t *testing.T) {
	srv := registrarServer(t)
	slot := srv.TryAcquireSlot(testIdentity("john@example.com"))
	if slot == nil {
		t.Fatal("no slot")
	}
	defer slot.Release()

	prefix := fmt.Sprintf(`{"using":[%q],"methodCalls":[["Core/echo",{"pad":"`, jmap.CoreCapability)
	suffix := `"},"c0"]]}`
	pad := int(srv.core.MaxSizeRequest) - len(prefix) - len(suffix)
	exact := prefix + strings.Repeat("x", pad) + suffix
	if int64(len(exact)) != srv.core.MaxSizeRequest {
		t.Fatalf("fixture is %d bytes, want %d", len(exact), srv.core.MaxSizeRequest)
	}
	if _, rerr := slot.Process(context.Background(), []byte(exact)); rerr != nil {
		t.Fatalf("body of exactly maxSizeRequest bytes refused: %+v", rerr)
	}
	over := prefix + strings.Repeat("x", pad+1) + suffix
	_, rerr := slot.Process(context.Background(), []byte(over))
	if rerr == nil || rerr.Type != jmap.ProblemLimit || rerr.Limit != "maxSizeRequest" {
		t.Fatalf("one byte over: got %+v, want limit maxSizeRequest", rerr)
	}
}

// The HTTP endpoint runs on the same pool: slots held elsewhere (as a
// second transport would hold them) surface as 429 on /api.
func TestAPISharesPoolWithSlots(t *testing.T) {
	srv := registrarServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	john := testIdentity("john@example.com")

	s1 := srv.TryAcquireSlot(john)
	s2 := srv.TryAcquireSlot(john)
	if s1 == nil || s2 == nil {
		t.Fatal("no slots")
	}
	body := fmt.Sprintf(`{"using":[%q],"methodCalls":[]}`, jmap.CoreCapability)
	resp := post(t, ts, body, "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d with user share exhausted, want 429", resp.StatusCode)
	}
	s1.Release()
	s2.Release()
	resp = post(t, ts, body, "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d after release, want 200", resp.StatusCode)
	}
}

// Churning acquires and releases across goroutines and users must
// neither exceed either bound nor leak per-user entries.
func TestSlotChurn(t *testing.T) {
	srv := registrarServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		u := testIdentity(fmt.Sprintf("churn%d@example.com", i%3))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s, err := srv.AcquireSlot(context.Background(), u)
				if err != nil {
					t.Error(err)
					return
				}
				s.Release()
				s.Release() // idempotent by contract
			}
		}()
	}
	wg.Wait()
	srv.userMu.Lock()
	defer srv.userMu.Unlock()
	if len(srv.userSlots) != 0 {
		t.Fatalf("user slot entries leaked: %v", srv.userSlots)
	}
	if len(srv.apiSlots) != 0 {
		t.Fatalf("%d pool slots still held", len(srv.apiSlots))
	}
}
