package postgres

import (
	"context"
	"testing"
	"time"
)

// A transaction that has executed its fence assert (the FOR UPDATE read in
// assertOp) holds the claim row lock, so a concurrent CompareAndSwap - a lease
// takeover - must wait for that transaction to finish and only then succeed.
// This is the ordering that keeps a stalled holder's writes from landing after
// a takeover's: the holder's whole batch commits strictly before the takeover
// swap completes.
func TestFenceHoldsTakeoverUntilHolderFinishes(t *testing.T) {
	store := openTestDB(t)
	defer store.Close()
	ctx := context.Background()
	key := []byte("lc/stall-account")

	if swapped, err := store.CompareAndSwap(ctx, key, nil, []byte("tokenA")); err != nil || !swapped {
		t.Fatalf("seeding claim: swapped %v err %v", swapped, err)
	}

	// The stalled holder: fence read done, data written, commit not yet issued.
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := assertOp(ctx, tx, key, []byte("tokenA")); err != nil {
		t.Fatalf("fence assert: %v", err)
	}
	if _, err := tx.Exec(ctx, sqlSet, []byte("acct/data"), []byte("stalled-write")); err != nil {
		t.Fatal(err)
	}

	// The takeover, racing the stalled holder.
	type result struct {
		swapped bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		swapped, err := store.CompareAndSwap(ctx, key, []byte("tokenA"), []byte("tokenB"))
		done <- result{swapped, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("takeover completed while the fence holder was still in flight (swapped %v err %v)", r.swapped, r.err)
	case <-time.After(500 * time.Millisecond):
		// Blocked, as required.
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || !r.swapped {
			t.Fatalf("takeover after holder finished: swapped %v err %v", r.swapped, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeover still blocked after the holder committed")
	}

	// The holder's write landed strictly before the takeover completed.
	if v, err := store.Get(ctx, []byte("acct/data")); err != nil || string(v) != "stalled-write" {
		t.Fatalf("holder write = %q err %v", v, err)
	}
}
