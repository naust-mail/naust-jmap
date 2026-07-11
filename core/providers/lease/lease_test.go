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

func TestSerializesPerAccount(t *testing.T) {
	be := memory.New()
	m := NewInProcess(be)
	ctx := context.Background()

	const workers = 8
	var inCritical, maxSeen int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := m.Acquire(ctx, "Aone")
			if err != nil {
				t.Error(err)
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
		t.Errorf("%d goroutines in critical section, want 1", maxSeen)
	}
}

func TestDifferentAccountsDoNotBlock(t *testing.T) {
	be := memory.New()
	m := NewInProcess(be)
	ctx := context.Background()

	l1, err := m.Acquire(ctx, "Aone")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()
	done := make(chan struct{})
	go func() {
		l2, err := m.Acquire(ctx, "Atwo")
		if err == nil {
			l2.Release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("independent account blocked behind another account's lease")
	}
}

func TestFencingRejectsStaleHolder(t *testing.T) {
	be := memory.New()
	m := NewInProcess(be)
	ctx := context.Background()

	stale, err := m.Acquire(ctx, "Aone")
	if err != nil {
		t.Fatal(err)
	}
	var staleBatch backend.Batch
	stale.Fence(&staleBatch)
	staleBatch.Set([]byte("data"), []byte("from-stale-holder"))
	stale.Release()

	// A newer holder bumps the generation...
	fresh, err := m.Acquire(ctx, "Aone")
	if err != nil {
		t.Fatal(err)
	}
	var freshBatch backend.Batch
	fresh.Fence(&freshBatch)
	freshBatch.Set([]byte("data"), []byte("from-fresh-holder"))
	if err := be.WriteBatch(ctx, &freshBatch); err != nil {
		t.Fatal(err)
	}
	fresh.Release()

	// ...so the stale holder's late commit must be rejected.
	if err := be.WriteBatch(ctx, &staleBatch); !errors.Is(err, backend.ErrAssertFailed) {
		t.Errorf("stale batch: err = %v, want ErrAssertFailed", err)
	}
	got, _ := be.Get(ctx, []byte("data"))
	if string(got) != "from-fresh-holder" {
		t.Errorf("data = %q", got)
	}
}

func TestAcquireHonorsContext(t *testing.T) {
	be := memory.New()
	m := NewInProcess(be)
	held, err := m.Acquire(context.Background(), "Aone")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(ctx, "Aone"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	held.Release()
	// The lease must be re-acquirable after the abandoned waiter.
	l, err := m.Acquire(context.Background(), "Aone")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}
