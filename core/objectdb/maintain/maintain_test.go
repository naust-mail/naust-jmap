package maintain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

func noteType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestNote",
		Capability: "https://naust.email/test/notes",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString},
		},
	}
}

func note(subject string) objectdb.Object {
	s, _ := json.Marshal(subject)
	return objectdb.Object{"subject": s}
}

func createIn(t *testing.T, db *objectdb.DB, acct jmap.Id) string {
	t.Helper()
	states, err := db.Update(context.Background(), acct, func(u *objectdb.Update) error {
		_, err := u.Create("TestNote", note("x"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return states["TestNote"]
}

// freezeClock pins the package clock and restores it when the test ends.
func freezeClock(t *testing.T, at time.Time) {
	t.Helper()
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
}

// TestRunOnceReclaimsAgedChangeLogs: a pass trims entries older than the
// retention window on every account, including one that has gone dormant -
// retention is a time trigger, so reclamation must not depend on the account
// committing again. A state from before the trim then gets
// cannotCalculateChanges (RFC 8620 section 5.2), while a caught-up client is
// unaffected.
func TestRunOnceReclaimsAgedChangeLogs(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithNow(func() time.Time { return base }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const dormant = jmap.Id("A1")
	stale := createIn(t, db, dormant)
	createIn(t, db, dormant)
	latest := createIn(t, db, dormant)

	// The whole log has aged past the retention window with no further
	// commits on the account.
	freezeClock(t, base.Add(tuning.ChangeLogRetention+time.Hour))
	RunOnce(ctx, db, Config{})

	if _, err := db.Changes(ctx, dormant, "TestNote", stale, 0); !errors.Is(err, objectdb.ErrCannotCalculateChanges) {
		t.Errorf("Changes from a reclaimed state: %v, want ErrCannotCalculateChanges", err)
	}
	cs, err := db.Changes(ctx, dormant, "TestNote", latest, 0)
	if err != nil {
		t.Fatalf("Changes for a caught-up client: %v", err)
	}
	if len(cs.Created) != 0 || len(cs.Updated) != 0 || len(cs.Destroyed) != 0 {
		t.Errorf("caught-up client got changes: %+v", cs)
	}
}

// failingBackend fails reads touching one account once armed, passing
// everything else through. It simulates a single account whose keys have
// become unreadable while the rest of the store is healthy.
type failingBackend struct {
	backend.Backend
	frag  []byte // account id fragment whose keys fail
	armed bool
}

var errInjected = errors.New("injected read failure")

func (b *failingBackend) Get(ctx context.Context, k []byte) ([]byte, error) {
	if b.armed && bytes.Contains(k, b.frag) {
		return nil, errInjected
	}
	return b.Backend.Get(ctx, k)
}

// TestRunOnceStepsOverFailingAccount: one account's failure is reported to
// OnError and the remaining accounts are still maintained - a pass must
// degrade per account, not abort.
func TestRunOnceStepsOverFailingAccount(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := &failingBackend{Backend: memory.New(), frag: []byte("Abad")}
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithNow(func() time.Time { return base }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	staleBad := createIn(t, db, "Abad")
	createIn(t, db, "Abad")
	staleGood := createIn(t, db, "Agood")
	createIn(t, db, "Agood")
	be.armed = true

	freezeClock(t, base.Add(tuning.ChangeLogRetention+time.Hour))
	var failed []jmap.Id
	RunOnce(ctx, db, Config{OnError: func(acct jmap.Id, err error) {
		if !errors.Is(err, errInjected) {
			t.Errorf("OnError(%s) reported %v, want the injected failure", acct, err)
		}
		failed = append(failed, acct)
	}})

	if len(failed) != 1 || failed[0] != "Abad" {
		t.Fatalf("failed accounts = %v, want exactly [Abad]", failed)
	}
	// The healthy account was still trimmed.
	if _, err := db.Changes(ctx, "Agood", "TestNote", staleGood, 0); !errors.Is(err, objectdb.ErrCannotCalculateChanges) {
		t.Errorf("healthy account not maintained: Changes = %v, want ErrCannotCalculateChanges", err)
	}
	// The failing account was left alone rather than half-trimmed: its
	// entries still answer the stale state in full.
	be.armed = false
	cs, err := db.Changes(ctx, "Abad", "TestNote", staleBad, 0)
	if err != nil {
		t.Fatalf("failing account's log damaged: Changes = %v, want success", err)
	}
	if len(cs.Created) != 1 {
		t.Errorf("failing account's diff = %+v, want the one later creation", cs)
	}
}

// TestRunPassesImmediatelyAndStopsOnCancel: Run must do its first pass
// without waiting out an interval (a fresh start may face a backlog), and
// must return promptly when its context ends.
func TestRunPassesImmediatelyAndStopsOnCancel(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithNow(func() time.Time { return base }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}

	stale := createIn(t, db, "A1")
	createIn(t, db, "A1")
	freezeClock(t, base.Add(tuning.ChangeLogRetention+time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// An interval far longer than the test: only the immediate first
		// pass can do the trimming.
		Run(ctx, db, Config{Interval: time.Hour})
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for {
		if _, err := db.Changes(context.Background(), "A1", "TestNote", stale, 0); errors.Is(err, objectdb.ErrCannotCalculateChanges) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first pass never trimmed the aged log")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
