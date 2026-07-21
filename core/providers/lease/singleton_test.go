package lease

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
)

// TestSingletonElectsOneHolder proves the core guarantee: across several
// candidates sharing one store, at most one holds the role at any instant, and
// a live holder keeps it (its renewals stop anyone else taking over), so the
// role does not change hands without a crash.
func TestSingletonElectsOneHolder(t *testing.T) {
	be := memory.New()
	defer be.Close()
	// Renew four times per TTL, so a live holder never lets its claim lapse.
	cfg := SingletonConfig{TTL: 80 * time.Millisecond, Renew: 20 * time.Millisecond, Poll: 30 * time.Millisecond}

	var mu sync.Mutex
	active, maxActive := 0, 0
	holders := map[int]bool{}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for id := 0; id < 3; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			RunSingleton(ctx, be, "role", cfg, func(hc context.Context) {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				holders[id] = true
				mu.Unlock()
				<-hc.Done()
				mu.Lock()
				active--
				mu.Unlock()
			})
		}(id)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Errorf("max concurrent holders = %d, want 1", maxActive)
	}
	if len(holders) != 1 {
		t.Errorf("role changed hands to %d distinct candidates without a crash, want 1 stable holder", len(holders))
	}
}

// TestSingletonTakeover proves failover: while the holder renews, no candidate
// takes over; once the holder stops renewing (a crash), another candidate takes
// the role once the claim expires.
func TestSingletonTakeover(t *testing.T) {
	be := memory.New()
	defer be.Close()
	cfg := SingletonConfig{TTL: 80 * time.Millisecond, Renew: 20 * time.Millisecond, Poll: 25 * time.Millisecond}

	var wg sync.WaitGroup
	held := func(ctx context.Context, ch chan struct{}) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RunSingleton(ctx, be, "role", cfg, func(hc context.Context) {
				select {
				case ch <- struct{}{}:
				default:
				}
				<-hc.Done()
			})
		}()
	}

	aCtx, aCancel := context.WithCancel(context.Background())
	bCtx, bCancel := context.WithCancel(context.Background())
	aHeld := make(chan struct{}, 1)
	bHeld := make(chan struct{}, 1)

	held(aCtx, aHeld)
	select {
	case <-aHeld:
	case <-time.After(2 * time.Second):
		aCancel()
		bCancel()
		wg.Wait()
		t.Fatal("candidate A never took the role")
	}

	// B campaigns behind A. While A is alive and renewing, B must not win.
	held(bCtx, bHeld)
	select {
	case <-bHeld:
		aCancel()
		bCancel()
		wg.Wait()
		t.Fatal("candidate B took the role while A still held and renewed it")
	case <-time.After(3 * cfg.TTL):
	}

	// A crashes: it stops renewing. B takes over once A's claim expires.
	aCancel()
	select {
	case <-bHeld:
	case <-time.After(2 * time.Second):
		bCancel()
		wg.Wait()
		t.Fatal("candidate B did not take over after A stopped renewing")
	}

	bCancel()
	wg.Wait()
}
