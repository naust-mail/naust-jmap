package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// These tests exercise the whole point of the store lease: when several
// instances share one database and pound the same account, they must queue
// instead of racing. The runtime serializes every account write under a lease
// and fences every commit on that account's generation (RFC 8620 section 3.10),
// so a writer that lost the account mid-commit fails its fence and the caller
// retries. Two instances whose lease is only process-local (InProcess) do not
// exclude each other and so produce a storm of these fenced failures; the store
// lease excludes them at Acquire and the storm disappears.
//
// They require PG_TEST_DSN and skip otherwise, because genuine cross-instance
// contention needs a real shared server.

// openSharedStores opens n Store connections onto one fresh test database, so
// each connection stands in for a separate instance contending over the same
// server. The first owns the database's creation and teardown.
func openSharedStores(t *testing.T, n int) []*Store {
	t.Helper()
	first := openTestDB(t)
	t.Cleanup(func() { first.Close() })
	stores := []*Store{first}
	if n <= 1 {
		return stores
	}
	dsn, err := withDSN(os.Getenv(dsnEnv), dbNameFor(t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		s, err := Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("open shared store %d: %v", i, err)
		}
		t.Cleanup(func() { s.Close() })
		stores = append(stores, s)
	}
	return stores
}

// contentionResult is the outcome of a contention run.
type contentionResult struct {
	accepted int64
	tempfail int64
	elapsed  time.Duration
}

// runContention has each manager repeatedly acquire the account, read-modify-
// write one fenced counter, and release, mirroring how a real method commits
// under its lease: it reads current state, builds a batch, then commits fenced.
// A commit whose fence fails is a clean tempfail - exactly how a stale
// generation surfaces to Email/set, which the client then retries. The
// accepted/tempfail split distinguishes a racing manager (many tempfails) from a
// serializing one (none); the wall clock shows how quickly the account is handed
// between instances. Every manager uses its own store connection, the way each
// instance would own its own pool.
func runContention(t *testing.T, managers []lease.Manager, stores []*Store, account jmap.Id, iters int) contentionResult {
	t.Helper()
	if len(managers) != len(stores) {
		t.Fatalf("managers/stores length mismatch: %d vs %d", len(managers), len(stores))
	}
	ctx := context.Background()
	dataKey := []byte("cd/" + string(account))
	var accepted, tempfail int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := range managers {
		wg.Add(1)
		go func(mgr lease.Manager, be *Store) {
			defer wg.Done()
			for n := 0; n < iters; n++ {
				l, err := mgr.Acquire(ctx, account)
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				// Read current state under the lease, then commit an
				// incremented value fenced on the generation. The read is the
				// round trip a racing InProcess pair can interleave a rival
				// acquire into, so the loser's fence then fails.
				cur := int64(0)
				v, err := be.Get(ctx, dataKey)
				switch {
				case err == nil:
					if cur, err = backend.DecodeInt64(v); err != nil {
						t.Errorf("decode counter: %v", err)
						l.Release()
						return
					}
				case !errors.Is(err, backend.ErrNotFound):
					t.Errorf("read counter: %v", err)
					l.Release()
					return
				}
				var b backend.Batch
				l.Fence(&b)
				b.Set(dataKey, backend.EncodeInt64(cur+1))
				err = be.WriteBatch(ctx, &b)
				l.Release()
				switch {
				case err == nil:
					atomic.AddInt64(&accepted, 1)
				case errors.Is(err, backend.ErrAssertFailed):
					atomic.AddInt64(&tempfail, 1)
				default:
					t.Errorf("write: %v", err)
					return
				}
			}
		}(managers[i], stores[i])
	}
	wg.Wait()
	return contentionResult{accepted: accepted, tempfail: tempfail, elapsed: time.Since(start)}
}

// TestStoreLeaseEliminatesTempfailStorm is the core justification for the store
// lease: two instances hammering one account race under InProcess (whose mutex
// is process-local) and produce fenced tempfails, but serialize under the store
// lease (whose claim record excludes them at Acquire) and produce none.
func TestStoreLeaseEliminatesTempfailStorm(t *testing.T) {
	const iters = 300
	stores := openSharedStores(t, 2)

	// Baseline: two independent InProcess managers over one database. Their
	// per-account mutex lives in separate process-local maps, so both hold the
	// account at once and the loser's fenced commit fails - the racing storm the
	// store lease exists to remove.
	baseMgrs := []lease.Manager{
		lease.NewInProcess(stores[0]),
		lease.NewInProcess(stores[1]),
	}
	base := runContention(t, baseMgrs, stores, "storm-baseline", iters)
	t.Logf("InProcess baseline: accepted=%d tempfail=%d of %d in %v",
		base.accepted, base.tempfail, 2*iters, base.elapsed)
	if base.tempfail == 0 {
		t.Fatal("baseline produced no fenced tempfails; the racing comparison would be vacuous")
	}

	// Store lease: the claim record excludes the two instances at Acquire, so
	// each fenced commit sees exactly the generation it acquired. No tempfails,
	// full acceptance.
	slMgrs := []lease.Manager{
		lease.NewStoreLease(stores[0], lease.StoreLeaseConfig{}),
		lease.NewStoreLease(stores[1], lease.StoreLeaseConfig{}),
	}
	got := runContention(t, slMgrs, stores, "storm-storelease", iters)
	t.Logf("StoreLease: accepted=%d tempfail=%d of %d in %v",
		got.accepted, got.tempfail, 2*iters, got.elapsed)
	if got.tempfail != 0 {
		t.Errorf("store lease still tempfailed %d times, want 0", got.tempfail)
	}
	if got.accepted != int64(2*iters) {
		t.Errorf("store lease accepted %d, want %d (near-100%% acceptance)", got.accepted, 2*iters)
	}
}

// TestHintWakeBeatsPolling proves the two remaining Step-4 claims by measuring
// the lease handoff latency directly: how long a parked waiter on one instance
// takes to acquire an account after another instance releases it. With only the
// poll timer (also the degraded mode, a box whose hint transport is absent or
// down) the waiter sleeps out most of a poll interval; with the wake hint it
// unblocks at once. Both paths hand off correctly, so this also confirms the
// polling fallback is not merely slower but still right.
func TestHintWakeBeatsPolling(t *testing.T) {
	const rounds = 8
	const poll = 120 * time.Millisecond
	ctx := context.Background()

	// One shared database, two connections standing in for two boxes.
	stores := openSharedStores(t, 2)

	// Polling only: no waker. The waiter falls back entirely to the poll timer.
	pollHolder := lease.NewStoreLease(stores[0], lease.StoreLeaseConfig{Poll: poll})
	pollWaiter := lease.NewStoreLease(stores[1], lease.StoreLeaseConfig{Poll: poll})
	var pollTotal time.Duration
	for r := 0; r < rounds; r++ {
		pollTotal += measureHandoff(t, pollHolder, pollWaiter, jmap.Id(fmt.Sprintf("handoff-poll-%d", r)))
	}

	// Hint wake: the holder's release publishes a lease-freed hint over
	// LISTEN/NOTIFY, so the waiter on the other instance unblocks immediately
	// instead of sleeping out the poll interval. Each instance runs its own
	// hint transport, as separate boxes would.
	hintsA, err := OpenHints(ctx, stores[0])
	if err != nil {
		t.Fatalf("open hints A: %v", err)
	}
	t.Cleanup(func() { hintsA.Close() })
	hintsB, err := OpenHints(ctx, stores[1])
	if err != nil {
		t.Fatalf("open hints B: %v", err)
	}
	t.Cleanup(func() { hintsB.Close() })
	wakeHolder := lease.NewStoreLease(stores[0], lease.StoreLeaseConfig{Poll: poll, Waker: hintsA.Waker()})
	wakeWaiter := lease.NewStoreLease(stores[1], lease.StoreLeaseConfig{Poll: poll, Waker: hintsB.Waker()})
	var wakeTotal time.Duration
	for r := 0; r < rounds; r++ {
		wakeTotal += measureHandoff(t, wakeHolder, wakeWaiter, jmap.Id(fmt.Sprintf("handoff-wake-%d", r)))
	}

	t.Logf("handoff latency over %d rounds: polling=%v hint-wake=%v", rounds, pollTotal, wakeTotal)
	// The hint unblocks within a notify round trip while polling waits out most
	// of a 120ms interval; the real gap is an order of magnitude. A 2x floor
	// keeps the assertion meaningful while tolerating a loaded, race-instrumented
	// run.
	if wakeTotal*2 > pollTotal {
		t.Errorf("hint wake (%v) was not measurably faster than polling (%v)", wakeTotal, pollTotal)
	}
}

// measureHandoff parks a waiter on an account another instance already holds,
// then releases and returns how long the waiter took to acquire after the
// release - the handoff latency. Both instances acquire without error, so a
// polling run (no hint transport) also proves the fallback hands off correctly.
func measureHandoff(t *testing.T, holder, waiter lease.Manager, account jmap.Id) time.Duration {
	t.Helper()
	ctx := context.Background()
	held, err := holder.Acquire(ctx, account)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	got := make(chan time.Time, 1)
	go func() {
		l, err := waiter.Acquire(ctx, account)
		got <- time.Now()
		if err != nil {
			t.Errorf("waiter acquire: %v", err)
			return
		}
		l.Release()
	}()
	// Let the waiter reach its parked wait before releasing. The first jittered
	// wait is at least half the poll interval, so 30ms lands inside it.
	time.Sleep(30 * time.Millisecond)
	t0 := time.Now()
	held.Release()
	return (<-got).Sub(t0)
}
