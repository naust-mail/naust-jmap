package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
)

// fakeClock is a manually advanced wall clock for tests that need to age a
// claim past its expiry deterministically.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestStoreLeaseCrossManagerExclusion is the guarantee InProcess cannot give:
// two managers over one store, contending for one account, never hold at once.
// This is the store lease's whole reason to exist.
func TestStoreLeaseCrossManagerExclusion(t *testing.T) {
	be := memory.New()
	defer be.Close()
	m1 := NewStoreLease(be, StoreLeaseConfig{})
	m2 := NewStoreLease(be, StoreLeaseConfig{})
	ctx := context.Background()

	var mu sync.Mutex
	var inCritical, maxSeen int
	var wg sync.WaitGroup
	for i, m := range []*StoreLease{m1, m2, m1, m2, m1, m2} {
		wg.Add(1)
		go func(m *StoreLease, i int) {
			defer wg.Done()
			l, err := m.Acquire(ctx, "account")
			if err != nil {
				t.Errorf("worker %d Acquire: %v", i, err)
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
		}(m, i)
	}
	wg.Wait()
	if maxSeen != 1 {
		t.Errorf("%d holders across two managers at once, want 1", maxSeen)
	}
}

// TestStoreLeaseExpiryStealThenFenceFail proves the liveness/safety split: a
// crashed holder that never releases has its account taken over once the claim
// expires, and the crashed holder's late Fenced commit then fails cleanly
// because the takeover replaced the claim token it fences on.
func TestStoreLeaseExpiryStealThenFenceFail(t *testing.T) {
	be := memory.New()
	defer be.Close()
	clock := newFakeClock()
	const expiry = 30 * time.Second

	crashed := NewStoreLease(be, StoreLeaseConfig{Expiry: expiry})
	crashed.now = clock.now
	successor := NewStoreLease(be, StoreLeaseConfig{Expiry: expiry})
	successor.now = clock.now
	ctx := context.Background()

	// The first holder acquires and then "crashes": it never releases.
	crashedLease, err := crashed.Acquire(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	var stale backend.Batch
	crashedLease.Fence(&stale)
	stale.Set([]byte("data"), []byte("from-crashed-holder"))

	// Time passes beyond the claim's expiry, so a second instance takes over.
	clock.advance(expiry + time.Second)
	successorLease, err := successor.Acquire(ctx, "account")
	if err != nil {
		t.Fatalf("successor could not steal an expired claim: %v", err)
	}
	var fresh backend.Batch
	successorLease.Fence(&fresh)
	fresh.Set([]byte("data"), []byte("from-successor"))
	if err := be.WriteBatch(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	successorLease.Release()

	// The crashed holder's late commit must be rejected and change nothing.
	if err := be.WriteBatch(ctx, &stale); !errors.Is(err, backend.ErrAssertFailed) {
		t.Errorf("crashed holder's late commit: err = %v, want ErrAssertFailed", err)
	}
	if got, _ := be.Get(ctx, []byte("data")); string(got) != "from-successor" {
		t.Errorf("data = %q, want from-successor", got)
	}
}

// TestStoreLeaseClaimValuesUnique proves every written claim is distinct, both
// within one instance (the per-acquire counter) and across instances (the
// per-instance nonce). Uniqueness is what lets Release assert on its own exact
// claim and never delete a successor's.
func TestStoreLeaseClaimValuesUnique(t *testing.T) {
	be := memory.New()
	defer be.Close()
	m1 := NewStoreLease(be, StoreLeaseConfig{})
	m2 := NewStoreLease(be, StoreLeaseConfig{})

	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		for _, m := range []*StoreLease{m1, m2} {
			v := string(m.newClaimValue())
			if seen[v] {
				t.Fatalf("duplicate claim value %q", v)
			}
			seen[v] = true
		}
	}
}

// TestStoreLeaseMalformedClaimTreatedAsExpired feeds the acquire path hostile
// claim records - anything a corrupt store or a bad actor with store access
// might leave behind - and proves each is treated as expired and taken over,
// never as a permanent lock. The takeover Assert keeps this race-free.
func TestStoreLeaseMalformedClaimTreatedAsExpired(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"no separator":     "not-a-valid-claim",
		"bad timestamp":    "bogus|nonce-1",
		"separator only":   "|",
		"trailing garbage": "2020-01-01T00:00:00Z-not-parseable",
	}
	for name, seeded := range cases {
		t.Run(name, func(t *testing.T) {
			be := memory.New()
			defer be.Close()
			ctx := context.Background()

			var seed backend.Batch
			seed.Set(storeClaimKey("account"), []byte(seeded))
			if err := be.WriteBatch(ctx, &seed); err != nil {
				t.Fatal(err)
			}

			m := NewStoreLease(be, StoreLeaseConfig{})
			l, err := m.Acquire(ctx, "account")
			if err != nil {
				t.Fatalf("Acquire over a malformed claim: %v", err)
			}
			l.Release()
		})
	}
}

// TestStoreLeaseWaitDuration pins the jittered poll interval: it scales with
// the configured Poll, never overshoots the claim's expiry, and never drops
// below 1ms even once the claim is already in the past.
func TestStoreLeaseWaitDuration(t *testing.T) {
	be := memory.New()
	defer be.Close()
	clock := newFakeClock()
	m := NewStoreLease(be, StoreLeaseConfig{Poll: 100 * time.Millisecond})
	m.now = clock.now

	m.rand = func() float64 { return 0.5 } // factor 1.0
	if d := m.waitDuration(clock.now().Add(time.Hour)); d != 100*time.Millisecond {
		t.Errorf("mid jitter, far expiry: d = %v, want 100ms", d)
	}
	m.rand = func() float64 { return 0 } // factor 0.5
	if d := m.waitDuration(clock.now().Add(time.Hour)); d != 50*time.Millisecond {
		t.Errorf("low jitter, far expiry: d = %v, want 50ms", d)
	}
	// Expiry nearer than the poll interval caps the wait so an expiring claim
	// is retried promptly.
	m.rand = func() float64 { return 0.5 }
	if d := m.waitDuration(clock.now().Add(10 * time.Millisecond)); d != 10*time.Millisecond {
		t.Errorf("near expiry: d = %v, want 10ms", d)
	}
	// A claim already in the past still yields a positive minimum wait.
	if d := m.waitDuration(clock.now().Add(-5 * time.Millisecond)); d != time.Millisecond {
		t.Errorf("past expiry: d = %v, want 1ms", d)
	}
}
