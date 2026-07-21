// Package leasetest is the shared contract suite for lease.Manager
// implementations: the single-node in-process manager and any store-backed
// cluster variant must pass the identical tests, so a Manager that passes is
// behaviorally interchangeable with every other for the guarantees exercised
// here.
//
// The suite asserts ONLY guarantees that every Manager provides, and that is a
// deliberate boundary, not an omission. Cross-instance behavior differs by
// implementation on purpose: an in-process manager keeps its per-account mutex
// in instance-local memory, so two such managers over one backend do not block
// each other at Acquire - both hold at once, and only the fencing assertion at
// commit time stops corruption (the loser's Fenced batch fails with
// backend.ErrAssertFailed). A store-backed manager adds Acquire-level exclusion
// across instances. A test asserting "two managers never overlap" therefore
// belongs to the store-backed implementation's own tests, never here; the
// shared suite keeps exclusion single-manager and tests fencing, which both
// kinds provide.
//
// Nothing here touches expiry or wall-clock liveness: those are properties of a
// specific store-backed manager, not of the Manager contract, and belong in
// that manager's own tests.
package leasetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// Run executes the full contract suite. newBackend returns a fresh, empty
// backend; newManager wraps a backend in the Manager under test. They are
// separate parameters so a store-backed manager's own tests can put two
// managers over one shared backend - the shared suite here never does, since
// cross-instance exclusion is not a universal Manager guarantee.
func Run(t *testing.T,
	newBackend func(t *testing.T) backend.Backend,
	newManager func(be backend.Backend) lease.Manager) {
	t.Run("Exclusion", func(t *testing.T) { testExclusion(t, newBackend, newManager) })
	t.Run("AccountIndependence", func(t *testing.T) { testIndependence(t, newBackend, newManager) })
	t.Run("Fencing", func(t *testing.T) { testFencing(t, newBackend, newManager) })
	t.Run("ContextCancel", func(t *testing.T) { testContextCancel(t, newBackend, newManager) })
	t.Run("DoubleRelease", func(t *testing.T) { testDoubleRelease(t, newBackend, newManager) })
}

// testExclusion proves one manager serializes concurrent Acquires of a single
// account: a guard counter incremented inside every held lease must never be
// seen above one. Run under -race, this also catches an unsynchronized
// implementation directly.
func testExclusion(t *testing.T,
	newBackend func(t *testing.T) backend.Backend,
	newManager func(be backend.Backend) lease.Manager) {
	be := newBackend(t)
	defer be.Close()
	m := newManager(be)
	ctx := context.Background()

	const workers = 8
	var mu sync.Mutex
	var inCritical, maxSeen int
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := m.Acquire(ctx, "account")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			mu.Lock()
			inCritical++
			if inCritical > maxSeen {
				maxSeen = inCritical
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inCritical--
			mu.Unlock()
			l.Release()
		}()
	}
	wg.Wait()
	if maxSeen != 1 {
		t.Errorf("%d holders in the critical section at once, want 1", maxSeen)
	}
}

// testIndependence proves the lease is per-account: a held lease on one account
// must not delay acquiring a different account.
func testIndependence(t *testing.T,
	newBackend func(t *testing.T) backend.Backend,
	newManager func(be backend.Backend) lease.Manager) {
	be := newBackend(t)
	defer be.Close()
	m := newManager(be)
	ctx := context.Background()

	held, err := m.Acquire(ctx, "account-one")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	done := make(chan struct{})
	go func() {
		l, err := m.Acquire(ctx, "account-two")
		if err == nil {
			l.Release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquiring an independent account blocked behind another account's lease")
	}
}

// testFencing proves the safety guarantee both kinds of manager share: a commit
// carrying the current lease's Fence applies, but once a newer holder has taken
// the account (bumping the generation) an older lease's Fenced commit fails and
// changes nothing. This is what lets a stalled or superseded holder's late write
// fail cleanly instead of corrupting, which is the only reason the atomic-batch
// backend model is safe without isolation.
func testFencing(t *testing.T,
	newBackend func(t *testing.T) backend.Backend,
	newManager func(be backend.Backend) lease.Manager) {
	be := newBackend(t)
	defer be.Close()
	m := newManager(be)
	ctx := context.Background()
	key := []byte("data")

	// A commit fenced by the currently held lease applies.
	first, err := m.Acquire(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	var live backend.Batch
	first.Fence(&live)
	live.Set(key, []byte("from-first-holder"))
	if err := be.WriteBatch(ctx, &live); err != nil {
		t.Fatalf("fenced commit while held: %v", err)
	}

	// Prepare a second commit still fenced by the first lease, then release it
	// and let a newer holder take the account and bump the generation.
	var stale backend.Batch
	first.Fence(&stale)
	stale.Set(key, []byte("from-stale-holder"))
	first.Release()

	second, err := m.Acquire(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	var fresh backend.Batch
	second.Fence(&fresh)
	fresh.Set(key, []byte("from-second-holder"))
	if err := be.WriteBatch(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	second.Release()

	// The first holder's late commit must now be rejected and leave the value
	// the second holder wrote untouched.
	if err := be.WriteBatch(ctx, &stale); !errors.Is(err, backend.ErrAssertFailed) {
		t.Errorf("stale fenced commit: err = %v, want ErrAssertFailed", err)
	}
	if got, _ := be.Get(ctx, key); string(got) != "from-second-holder" {
		t.Errorf("data = %q, want from-second-holder", got)
	}
}

// testContextCancel proves a waiter that gives up (ctx canceled) returns
// ctx.Err() and does not poison the account: once the holder releases, a later
// Acquire still succeeds.
func testContextCancel(t *testing.T,
	newBackend func(t *testing.T) backend.Backend,
	newManager func(be backend.Backend) lease.Manager) {
	be := newBackend(t)
	defer be.Close()
	m := newManager(be)

	held, err := m.Acquire(context.Background(), "account")
	if err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		l, err := m.Acquire(cctx, "account")
		if err == nil {
			l.Release()
		}
		errc <- err
	}()
	// Let the waiter block on the held account, then abandon it.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled Acquire err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled Acquire did not return")
	}

	held.Release()
	// The abandoned waiter must not have left the account locked.
	l, err := m.Acquire(context.Background(), "account")
	if err != nil {
		t.Fatalf("account poisoned after a canceled waiter: %v", err)
	}
	l.Release()
}

// testDoubleRelease proves Release is idempotent: calling it twice is a safe
// no-op (never a panic from a double unlock), and the account stays usable.
func testDoubleRelease(t *testing.T,
	newBackend func(t *testing.T) backend.Backend,
	newManager func(be backend.Backend) lease.Manager) {
	be := newBackend(t)
	defer be.Close()
	m := newManager(be)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	l.Release()

	next, err := m.Acquire(ctx, "account")
	if err != nil {
		t.Fatalf("account unusable after a double Release: %v", err)
	}
	next.Release()
}
