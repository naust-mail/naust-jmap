package pushsub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

func newStore() *Store {
	be := memory.New()
	return NewStore(be, lease.NewInProcess(be))
}

func utc(d time.Duration) string {
	return time.Now().UTC().Add(d).Truncate(time.Second).Format(time.RFC3339)
}

func mk(id jmap.Id, credential string, expiresIn time.Duration) *Subscription {
	return &Subscription{
		Id: id, Credential: credential, DeviceClientId: "dev-" + string(id),
		URL: "https://push.example.com/" + string(id), ExpectedCode: "code",
		Expires: utc(expiresIn), Accounts: []jmap.Id{"Aone"},
	}
}

func TestCRUDAndCredentialIsolation(t *testing.T) {
	st := newStore()
	ctx := context.Background()

	if _, err := st.Get(ctx, "john", "S1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get: %v", err)
	}
	if err := st.Create(ctx, mk("S1", "john", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, mk("S2", "jane", time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Only the creating credential's records are visible (7.2.1).
	johns, err := st.List(ctx, "john")
	if err != nil || len(johns) != 1 || johns[0].Id != "S1" {
		t.Fatalf("john's list: %v, %v", johns, err)
	}
	if _, err := st.Get(ctx, "jane", "S1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-credential Get: %v", err)
	}

	if err := st.Update(ctx, "john", "S1", func(s *Subscription) error {
		s.VerificationCode = "code"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, "john", "S1")
	if err != nil || !got.Verified() {
		t.Fatalf("after update: %+v, %v", got, err)
	}

	// An error from the callback aborts without writing.
	boom := errors.New("boom")
	if err := st.Update(ctx, "john", "S1", func(s *Subscription) error {
		s.VerificationCode = ""
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("callback error: %v", err)
	}
	if got, _ := st.Get(ctx, "john", "S1"); !got.Verified() {
		t.Fatal("aborted update was persisted")
	}

	all, err := st.All(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("All: %d, %v", len(all), err)
	}

	if err := st.Destroy(ctx, "john", "S1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Destroy(ctx, "john", "S1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double destroy: %v", err)
	}
	if err := st.Update(ctx, "john", "S1", func(*Subscription) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update after destroy: %v", err)
	}
}

// TestCapPurgesExpired: the per-credential cap (section 8.6) counts
// live records only; creating past the cap fails, and expired records
// are purged rather than counted.
func TestCapPurgesExpired(t *testing.T) {
	st := newStore()
	st.MaxPerCredential = 2
	ctx := context.Background()

	if err := st.Create(ctx, mk("S1", "john", -time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, mk("S2", "john", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, mk("S3", "john", time.Hour)); err != nil {
		t.Fatal(err) // S1 is expired: only S2 counted
	}
	if err := st.Create(ctx, mk("S4", "john", time.Hour)); !errors.Is(err, ErrTooMany) {
		t.Fatalf("over cap: %v", err)
	}
	// The expired S1 was purged by the successful create.
	if _, err := st.Get(ctx, "john", "S1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired record survived: %v", err)
	}
	// Another credential is unaffected by john's cap.
	if err := st.Create(ctx, mk("S5", "jane", time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// TestDestroyAll is the credential-revocation hook: 7.2 requires all
// of a credential's subscriptions destroyed when it is revoked.
func TestDestroyAll(t *testing.T) {
	st := newStore()
	ctx := context.Background()
	for _, id := range []jmap.Id{"S1", "S2"} {
		if err := st.Create(ctx, mk(id, "john", time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Create(ctx, mk("S3", "jane", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.DestroyAll(ctx, "john"); err != nil {
		t.Fatal(err)
	}
	if subs, _ := st.List(ctx, "john"); len(subs) != 0 {
		t.Fatalf("john still has %d", len(subs))
	}
	if subs, _ := st.List(ctx, "jane"); len(subs) != 1 {
		t.Fatalf("jane lost hers: %d", len(subs))
	}
	// Idempotent on an empty credential.
	if err := st.DestroyAll(ctx, "john"); err != nil {
		t.Fatal(err)
	}
}
