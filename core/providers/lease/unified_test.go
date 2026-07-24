package lease

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
)

// fenceWrite commits one fenced Set through be and returns the batch error.
func fenceWrite(t *testing.T, be backend.Backend, l Lease, key string) error {
	t.Helper()
	var b backend.Batch
	l.Fence(&b)
	b.Set([]byte(key), []byte("v"))
	return be.WriteBatch(context.Background(), &b)
}

// A fresh InProcess manager over a store holding a dead predecessor's claim
// must take the account over immediately - no expiry wait - and the
// predecessor's fence must be dead from that moment.
func TestInProcessRestartTakesOverPredecessorClaim(t *testing.T) {
	be := memory.New()
	m1 := NewInProcess(be)
	l1, err := m1.Acquire(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	// m1 "crashes": l1 is never released, its claim record stays in the store.

	m2 := NewInProcess(be)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l2, err := m2.Acquire(ctx, "a1")
	if err != nil {
		t.Fatalf("restarted process could not take over: %v", err)
	}
	defer l2.Release()

	if err := fenceWrite(t, be, l1, "a1/x"); !errors.Is(err, backend.ErrAssertFailed) {
		t.Fatalf("predecessor fence write = %v, want ErrAssertFailed", err)
	}
	if err := fenceWrite(t, be, l2, "a1/y"); err != nil {
		t.Fatalf("successor fence write = %v, want nil", err)
	}
}

// An InProcess box and a StoreLease box misconfigured onto one store fence on
// the same claim key: the InProcess steal kills the store-lease holder's fence
// instead of interleaving with it, and a later foreign swap of the InProcess
// claim is detected and reported.
func TestMixedManagersFenceEachOtherAndDetect(t *testing.T) {
	be := memory.New()
	ctx := context.Background()

	sl := NewStoreLease(be, StoreLeaseConfig{})
	slLease, err := sl.Acquire(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	defer slLease.Release()

	ip := NewInProcess(be)
	var warned []string
	ip.warnForeign = func(account jmap.Id, evidence string) {
		warned = append(warned, string(account)+": "+evidence)
	}
	ipLease, err := ip.Acquire(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}

	// The store-lease holder's fence is dead the moment InProcess stole the
	// claim; its late commit tempfails instead of corrupting.
	if err := fenceWrite(t, be, slLease, "a1/x"); !errors.Is(err, backend.ErrAssertFailed) {
		t.Fatalf("stolen-from holder fence write = %v, want ErrAssertFailed", err)
	}
	if err := fenceWrite(t, be, ipLease, "a1/y"); err != nil {
		t.Fatalf("InProcess fence write = %v, want nil", err)
	}
	ipLease.Release()

	// A live foreign writer replaces the InProcess claim behind its back; the
	// next acquire must still succeed and must report the proven foreigner.
	if swapped, err := cas(ctx, be, storeClaimKey("a1"), nil, nil); err != nil || swapped {
		// The claim exists, so an expected-absent swap must fail; this guards
		// the test's own assumption.
		t.Fatalf("claim unexpectedly absent (swapped %v err %v)", swapped, err)
	}
	cur, err := getClaim(ctx, be, storeClaimKey("a1"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := mintToken("feedfacefeedface", new(atomic.Uint64), time.Now().Add(10*time.Second))
	if swapped, err := cas(ctx, be, storeClaimKey("a1"), cur, foreign); err != nil || !swapped {
		t.Fatalf("could not plant foreign claim (swapped %v err %v)", swapped, err)
	}

	l2, err := ip.Acquire(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()
	if len(warned) == 0 {
		t.Fatal("live foreign writer was not reported")
	}
	if err := fenceWrite(t, be, l2, "a1/z"); err != nil {
		t.Fatalf("post-detection fence write = %v, want nil", err)
	}
}

// countingBackend counts every store operation, to pin down the acquire cost.
type countingBackend struct {
	backend.Backend
	ops int
}

func (c *countingBackend) Get(ctx context.Context, key []byte) ([]byte, error) {
	c.ops++
	return c.Backend.Get(ctx, key)
}

func (c *countingBackend) WriteBatch(ctx context.Context, b *backend.Batch) error {
	c.ops++
	return c.Backend.WriteBatch(ctx, b)
}

// The steady-state InProcess acquire/release cycle is exactly one store round
// trip: a single swap against the cached claim, with Release store-free. This
// pins the fast path so it cannot silently regress.
func TestInProcessSteadyStateAcquireIsOneOp(t *testing.T) {
	be := &countingBackend{Backend: memory.New()}
	m := NewInProcess(be)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "a1") // first contact: read + swap
	if err != nil {
		t.Fatal(err)
	}
	l.Release()

	be.ops = 0
	for i := 0; i < 5; i++ {
		l, err := m.Acquire(ctx, "a1")
		if err != nil {
			t.Fatal(err)
		}
		l.Release()
	}
	if be.ops != 5 {
		t.Fatalf("5 steady-state acquire/release cycles cost %d store ops, want 5", be.ops)
	}
}

// The other direction of the mixed-manager scenario: a store-lease instance
// waits out an InProcess claim's expiry, takes the account over, and the
// InProcess holder's fence is dead from that moment.
func TestStoreLeaseStealsExpiredInProcessClaim(t *testing.T) {
	be := memory.New()
	ctx := context.Background()
	base := time.Now()

	ip := NewInProcess(be)
	ip.warnForeign = func(jmap.Id, string) {}
	ip.now = func() time.Time { return base }
	ipLease, err := ip.Acquire(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	// The InProcess box stalls holding the lease; its claim expires on the
	// store-lease box's clock.
	sl := NewStoreLease(be, StoreLeaseConfig{})
	sl.now = func() time.Time { return base.Add(inProcessExpiry + time.Second) }

	slCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	slLease, err := sl.Acquire(slCtx, "a1")
	if err != nil {
		t.Fatalf("store lease could not take over expired claim: %v", err)
	}
	defer slLease.Release()

	if err := fenceWrite(t, be, ipLease, "a1/x"); !errors.Is(err, backend.ErrAssertFailed) {
		t.Fatalf("expired holder fence write = %v, want ErrAssertFailed", err)
	}
	if err := fenceWrite(t, be, slLease, "a1/y"); err != nil {
		t.Fatalf("takeover fence write = %v, want nil", err)
	}
}
