package maintain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
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

// Scan fails a scan whose range touches the fragment, the same trigger as
// Get: db.Accounts lists via a prefix scan, so this is what lets a test
// simulate that listing itself failing.
func (b *failingBackend) Scan(ctx context.Context, start, end []byte, reverse bool, fn func(key, value []byte) bool) error {
	if b.armed && (bytes.Contains(start, b.frag) || bytes.Contains(end, b.frag)) {
		return errInjected
	}
	return b.Backend.Scan(ctx, start, end, reverse, fn)
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

// uploadUnreferencedBlob stores content that no record ever references, so
// it lands in SweepBlobs' candidate set immediately.
func uploadUnreferencedBlob(t *testing.T, db *objectdb.DB, store blob.Store, acct jmap.Id, content string, at time.Time) jmap.Id {
	t.Helper()
	ctx := context.Background()
	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(bw, content); err != nil {
		t.Fatal(err)
	}
	blobID, err := db.FinalizeBlobUpload(ctx, acct, bw, "uploader", at)
	if err != nil {
		t.Fatal(err)
	}
	return blobID
}

// TestRunOnceSweepsBlobsWhenConfigured: a pass with cfg.Blobs set collects
// unreferenced blobs aged past the grace window; the zero Config (Blobs
// nil) must leave them untouched, since that is the opt-out for hosts that
// sweep on their own schedule.
func TestRunOnceSweepsBlobsWhenConfigured(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithNow(func() time.Time { return base }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	store := kvstore.New(be)
	ctx := context.Background()

	const acct = jmap.Id("A1")
	createIn(t, db, acct) // gets the account onto db.Accounts()
	blobID := uploadUnreferencedBlob(t, db, store, acct, "payload", base)

	freezeClock(t, base.Add(tuning.BlobMinUnreferencedAge+time.Hour))

	// Blobs unset: the blob must survive.
	RunOnce(ctx, db, Config{})
	if _, _, err := store.Open(ctx, acct, blobID); err != nil {
		t.Fatalf("blob swept without cfg.Blobs set: Open = %v", err)
	}

	// Blobs set: the aged, unreferenced blob is collected.
	RunOnce(ctx, db, Config{Blobs: store})
	if _, _, err := store.Open(ctx, acct, blobID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Open after sweep = %v, want ErrNotFound", err)
	}
	if _, err := db.BlobUpload(ctx, acct, blobID); !errors.Is(err, objectdb.ErrNotFound) {
		t.Fatalf("BlobUpload after sweep = %v, want ErrNotFound", err)
	}
}

// TestRunOnceDrainsBlobBacklog: SweepBlobs bounds one call to
// maxSweepPerCall candidates and signals more work with its "more" return.
// RunOnce must keep calling until the account's backlog is fully drained
// in one pass, not just chip at it once.
func TestRunOnceDrainsBlobBacklog(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithNow(func() time.Time { return base }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	store := kvstore.New(be)
	ctx := context.Background()

	const acct = jmap.Id("A1")
	createIn(t, db, acct)
	// Comfortably more than one SweepBlobs call's bound (1024), so a
	// single call cannot possibly drain it.
	const n = 1200
	ids := make([]jmap.Id, n)
	for i := range ids {
		ids[i] = uploadUnreferencedBlob(t, db, store, acct, string(rune('a'+i%26))+jmapIdSuffix(i), base)
	}

	freezeClock(t, base.Add(tuning.BlobMinUnreferencedAge+time.Hour))
	RunOnce(ctx, db, Config{Blobs: store})

	for _, id := range ids {
		if _, _, err := store.Open(ctx, acct, id); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("blob %s survived a full pass: Open = %v", id, err)
		}
	}
}

// jmapIdSuffix keeps each uploaded blob's content distinct so it gets a
// distinct content-addressed blobId.
func jmapIdSuffix(i int) string { return string(rune('0' + i%10)) }

// TestRunOnceCallsExtraPerAccountAfterBuiltins: cfg.Extra is the seam an
// embedder's own datatype-specific reclamation attaches through, on the
// same per-account loop as the built-in passes - it must run for every
// account, after that account's built-in trimming has already applied.
func TestRunOnceCallsExtraPerAccountAfterBuiltins(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithNow(func() time.Time { return base }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	stale := createIn(t, db, "A1")
	createIn(t, db, "A1")
	createIn(t, db, "A2")

	freezeClock(t, base.Add(tuning.ChangeLogRetention+time.Hour))

	var seen []jmap.Id
	RunOnce(ctx, db, Config{Extra: func(_ context.Context, acct jmap.Id) {
		// The built-in trim for this account must already have run.
		if acct == "A1" {
			if _, err := db.Changes(ctx, "A1", "TestNote", stale, 0); !errors.Is(err, objectdb.ErrCannotCalculateChanges) {
				t.Errorf("Extra(A1) ran before the built-in trim took effect")
			}
		}
		seen = append(seen, acct)
	}})

	if len(seen) != 2 || seen[0] != "A1" || seen[1] != "A2" {
		t.Fatalf("Extra saw %v, want [A1 A2]", seen)
	}
}

// TestRunOnceReportsAccountListFailure: when listing accounts itself fails,
// cfg.OnError must be told (with an empty acct, since no account is
// implicated) and the pass must return without touching anything, rather
// than panicking on a nil account slice.
func TestRunOnceReportsAccountListFailure(t *testing.T) {
	be := &failingBackend{Backend: memory.New(), frag: []byte("!tag")}
	db := objectdb.New(be, lease.NewInProcess(be))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	be.armed = true

	var got []jmap.Id
	var gotErr error
	RunOnce(context.Background(), db, Config{OnError: func(acct jmap.Id, err error) {
		got = append(got, acct)
		gotErr = err
	}})

	if len(got) != 1 || got[0] != "" {
		t.Fatalf("OnError acct = %v, want a single empty acct", got)
	}
	if !errors.Is(gotErr, errInjected) {
		t.Fatalf("OnError err = %v, want the injected failure", gotErr)
	}
}

// TestRunOnceStopsWalkingAccountsOnCancel: RunOnce checks ctx between
// accounts, not just at method calls within one account's passes - a
// context that ends partway through the account list must stop the walk
// rather than pressing on to the remaining accounts.
func TestRunOnceStopsWalkingAccountsOnCancel(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithNow(func() time.Time { return base }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	createIn(t, db, "A1")
	createIn(t, db, "A2")
	createIn(t, db, "A3")

	ctx, cancel := context.WithCancel(context.Background())
	var seen []jmap.Id
	RunOnce(ctx, db, Config{Extra: func(_ context.Context, acct jmap.Id) {
		seen = append(seen, acct)
		if len(seen) == 1 {
			cancel()
		}
	}})

	if len(seen) != 1 {
		t.Fatalf("Extra ran for %v after cancellation, want exactly one account", seen)
	}
}
