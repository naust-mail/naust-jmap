package objectdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// newClockDB returns a DB whose commit timestamps come from the returned
// clock pointer, so a test can age the change log without sleeping.
func newClockDB(t *testing.T) (*DB, *time.Time) {
	t.Helper()
	clock := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := memory.New()
	db := New(be, lease.NewInProcess(be), WithNow(func() time.Time { return clock }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	return db, &clock
}

// TestTrimChangesRetention: entries past the retention window are deleted,
// and a state below the resulting floor is no longer computable - RFC 8620
// section 5.2 requires the complete set of changes, so the only honest
// answer once the entries are gone is cannotCalculateChanges.
func TestTrimChangesRetention(t *testing.T) {
	db, clock := newClockDB(t)
	ctx := context.Background()

	create(t, db, note("one", "x"))
	_, state2 := create(t, db, note("two", "x"))
	_, state3 := create(t, db, note("three", "x"))
	*clock = clock.Add(48 * time.Hour)
	create(t, db, note("four", "x"))

	// A window that covers the recent commit but not the three older ones.
	deleted, err := db.TrimChanges(ctx, acct, *clock, 24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3", deleted)
	}

	// state2 sits below the floor: its diff would need the deleted entries.
	if _, err := db.Changes(ctx, acct, "TestNote", state2, 0); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("Changes from trimmed state: %v, want ErrCannotCalculateChanges", err)
	}
	// state3 is the oldest still answerable: the entries it needs survive.
	cs, err := db.Changes(ctx, acct, "TestNote", state3, 0)
	if err != nil {
		t.Fatalf("Changes from the floor state: %v", err)
	}
	if len(cs.Created) != 1 {
		t.Errorf("Created from the floor state = %v, want the one surviving commit", cs.Created)
	}

	// Nothing left to age out, so a second pass is a no-op.
	if deleted, err := db.TrimChanges(ctx, acct, *clock, 24*time.Hour, 0); err != nil || deleted != 0 {
		t.Errorf("second trim = %d, %v, want 0, nil", deleted, err)
	}
}

// TestTrimChangesMaxEntries: the entry cap trims regardless of age. It is
// the bound that matters for disk, since retention alone leaves the log
// unbounded when commits arrive fast enough.
func TestTrimChangesMaxEntries(t *testing.T) {
	db, clock := newClockDB(t)
	ctx := context.Background()

	_, state1 := create(t, db, note("one", "x"))
	_, state2 := create(t, db, note("two", "x"))
	create(t, db, note("three", "x"))
	create(t, db, note("four", "x"))

	// Retention disabled: every entry is recent, so only the cap can bite.
	deleted, err := db.TrimChanges(ctx, acct, *clock, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if _, err := db.Changes(ctx, acct, "TestNote", state1, 0); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("Changes from capped-out state: %v, want ErrCannotCalculateChanges", err)
	}
	// The boundary is exact: state2 needs only the two entries the cap
	// kept, so it stays answerable.
	cs, err := db.Changes(ctx, acct, "TestNote", state2, 0)
	if err != nil {
		t.Fatalf("Changes from the floor state: %v", err)
	}
	if len(cs.Created) != 2 {
		t.Errorf("Created from the floor state = %v, want both surviving commits", cs.Created)
	}
}

// TestTrimChangesKeepsUndatedEntries: an entry written before the log
// carried a commit timestamp has no age, so the window must not drop it -
// otherwise upgrading would silently discard an existing log.
func TestTrimChangesKeepsUndatedEntries(t *testing.T) {
	db, clock := newClockDB(t)
	ctx := context.Background()

	id, _ := create(t, db, note("one", "x"))
	create(t, db, note("two", "x"))

	// Rewrite the first entry as a pre-timestamp one would have been.
	raw, err := json.Marshal(logEntry{Types: map[string]*logTypeEntry{
		"TestNote": {Created: []jmap.Id{id}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	batch := &backend.Batch{}
	batch.Set(logKey(acct, 1), raw)
	if err := db.be.WriteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}

	// Far past any window, but the undated entry blocks the prefix.
	*clock = clock.Add(365 * 24 * time.Hour)
	deleted, err := db.TrimChanges(ctx, acct, *clock, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 (an undated entry is never aged out)", deleted)
	}

	// The cap still reclaims it - that trigger does not consult the clock.
	if deleted, err := db.TrimChanges(ctx, acct, *clock, time.Hour, 1); err != nil || deleted != 1 {
		t.Errorf("capped trim = %d, %v, want 1, nil", deleted, err)
	}
}

// countingBackend counts the log entries a scan actually visits, so a test
// can assert on the work a call does and not just its result.
type countingBackend struct {
	backend.Backend
	logEntriesVisited int
}

func (c *countingBackend) Scan(ctx context.Context, start, end []byte, reverse bool, fn func(k, v []byte) bool) error {
	logStart, logEnd := prefixRange(seg(string(acct)), seg("g"))
	return c.Backend.Scan(ctx, start, end, reverse, func(k, v []byte) bool {
		if bytes.Compare(k, logStart) >= 0 && bytes.Compare(k, logEnd) < 0 {
			c.logEntriesVisited++
		}
		return fn(k, v)
	})
}

// TestTrimChangesIdleIsCheap: the overwhelmingly common call is on an
// account with nothing to trim, and it must not cost a scan of the whole
// retained log to establish that. The floor and the sequence counter bound
// the entry count between them, so the cap is ruled out from two point
// reads, and the window is ruled out by the oldest entry alone.
func TestTrimChangesIdleIsCheap(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := &countingBackend{Backend: memory.New()}
	db := New(be, lease.NewInProcess(be), WithNow(func() time.Time { return clock }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		create(t, db, note("filler", "x"))
	}

	be.logEntriesVisited = 0
	deleted, err := db.TrimChanges(ctx, acct, clock, 24*time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 (nothing is expired or over cap)", deleted)
	}
	// One entry read settles the window; the cap never reads one. Anything
	// proportional to the log means the fast path is gone.
	if be.logEntriesVisited > 1 {
		t.Errorf("idle trim visited %d log entries, want at most 1", be.logEntriesVisited)
	}
}

// TestTrimChangesCaughtUpClient: trimming the whole log must not break a
// client that is already up to date - it needs no entries, so it stays on
// the fast path rather than being told to resync.
func TestTrimChangesCaughtUpClient(t *testing.T) {
	db, clock := newClockDB(t)
	ctx := context.Background()

	create(t, db, note("one", "x"))
	_, latest := create(t, db, note("two", "x"))
	*clock = clock.Add(48 * time.Hour)

	deleted, err := db.TrimChanges(ctx, acct, *clock, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	cs, err := db.Changes(ctx, acct, "TestNote", latest, 0)
	if err != nil {
		t.Fatalf("Changes from the current state: %v", err)
	}
	if len(cs.Created) != 0 || len(cs.Updated) != 0 || len(cs.Destroyed) != 0 {
		t.Errorf("caught-up client got changes: %+v", cs)
	}
	if cs.NewState != latest {
		t.Errorf("NewState = %q, want %q", cs.NewState, latest)
	}
}

// staleFloorBackend serves a single read of one key from a stale value
// captured before a concurrent write, passing everything else through. It
// reproduces the interleaving where another writer commits between a
// reader's first read of that key and the reads that follow.
type staleFloorBackend struct {
	backend.Backend
	key   []byte
	armed bool // next Get of key reports not-found, as before the trim
}

func (b *staleFloorBackend) Get(ctx context.Context, k []byte) ([]byte, error) {
	if b.armed && bytes.Equal(k, b.key) {
		b.armed = false
		return nil, backend.ErrNotFound
	}
	return b.Backend.Get(ctx, k)
}

// TestChangesRefusesPartialDiffAfterConcurrentTrim: readers take no lease,
// so a trim can commit between Foo/changes' floor check and its entry scan,
// deleting entries the scan needed. The scan alone would answer with a
// partial diff; the post-scan floor re-read must turn it into
// cannotCalculateChanges instead, because RFC 8620 section 5.2 requires the
// complete set of changes or a refusal, never an incomplete one.
func TestChangesRefusesPartialDiffAfterConcurrentTrim(t *testing.T) {
	clock := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	be := &staleFloorBackend{Backend: memory.New(), key: logFloorKey(acct)}
	db := New(be, lease.NewInProcess(be), WithNow(func() time.Time { return clock }))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	create(t, db, note("one", "x"))
	_, state2 := create(t, db, note("two", "x"))
	create(t, db, note("three", "x"))
	clock = clock.Add(48 * time.Hour)
	create(t, db, note("four", "x"))

	// Trim the three aged entries for real, then arm the wrapper: the next
	// floor read reports the pre-trim state, exactly what a reader that
	// checked the floor just before this trim committed would have seen.
	if _, err := db.TrimChanges(ctx, acct, clock, 24*time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	be.armed = true

	// state2 needs the deleted entry for "three"; with the stale floor the
	// scan finds only "four" and would report a diff missing a creation.
	if _, err := db.Changes(ctx, acct, "TestNote", state2, 0); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("Changes with a stale floor check = %v, want ErrCannotCalculateChanges", err)
	}
	if be.armed {
		t.Fatal("stale floor read never consumed; the interleaving was not exercised")
	}
}
