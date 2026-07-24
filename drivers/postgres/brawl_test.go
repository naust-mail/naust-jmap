package postgres

import (
	"context"
	"errors"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// A deliberate worst-case brawl on one account: several store-lease instances
// with a tiny expiry plus one misconfigured InProcess manager, all over one
// Postgres store, with holders that randomly stall past the expiry so takeovers
// and late fenced commits happen constantly. Every worker does a fenced
// read-increment-write on one counter. The invariant is a ledger: the final
// counter must equal the number of commits that reported success - one lost
// update, one stale write landing past a takeover, or one fence false-positive
// breaks the equality.
func TestLeaseBrawlNeverLosesAnUpdate(t *testing.T) {
	store := openTestDB(t)
	defer store.Close()
	ctx := context.Background()

	// The misconfigured InProcess manager legitimately spams its foreign-writer
	// warning here; silence logging for the duration.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	const (
		account = "brawl"
		expiry  = 40 * time.Millisecond
		workers = 8
		iters   = 10
	)
	counterKey := []byte("brawl/counter")
	managers := []lease.Manager{
		lease.NewStoreLease(store, lease.StoreLeaseConfig{Expiry: expiry, Poll: 2 * time.Millisecond}),
		lease.NewStoreLease(store, lease.StoreLeaseConfig{Expiry: expiry, Poll: 2 * time.Millisecond}),
		lease.NewStoreLease(store, lease.StoreLeaseConfig{Expiry: expiry, Poll: 2 * time.Millisecond}),
		lease.NewInProcess(store),
	}

	var successes atomic.Int64
	var fenced atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		m := managers[w%len(managers)]
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := mrand.New(mrand.NewPCG(seed, seed))
			for i := 0; i < iters; i++ {
				acqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				l, err := m.Acquire(acqCtx, account)
				cancel()
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				cur := int64(0)
				if v, err := store.Get(ctx, counterKey); err == nil {
					if cur, err = backend.DecodeInt64(v); err != nil {
						t.Errorf("decode: %v", err)
					}
				} else if !errors.Is(err, backend.ErrNotFound) {
					t.Errorf("get: %v", err)
				}
				// Stall, sometimes well past the expiry, inviting a takeover
				// between the read and the fenced write.
				time.Sleep(time.Duration(rng.Int64N(int64(expiry) * 3 / 2)))
				var b backend.Batch
				l.Fence(&b)
				b.Set(counterKey, backend.EncodeInt64(cur+1))
				switch err := store.WriteBatch(ctx, &b); {
				case err == nil:
					successes.Add(1)
				case errors.Is(err, backend.ErrAssertFailed):
					fenced.Add(1) // superseded holder, correctly rejected
				default:
					t.Errorf("commit: %v", err)
				}
				l.Release()
			}
		}(uint64(w + 1))
	}
	wg.Wait()

	final := int64(0)
	if v, err := store.Get(ctx, counterKey); err == nil {
		var derr error
		if final, derr = backend.DecodeInt64(v); derr != nil {
			t.Fatal(derr)
		}
	}
	t.Logf("commits: %d succeeded, %d fenced off, counter %d", successes.Load(), fenced.Load(), final)
	if fenced.Load() == 0 {
		t.Error("no commit was ever fenced off; the brawl did not actually contend")
	}
	if final != successes.Load() {
		t.Fatalf("LOST UPDATE: counter %d != %d successful commits", final, successes.Load())
	}
}
