package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/backendtest"
)

func TestContract(t *testing.T) {
	backendtest.Run(t, backendtest.Config{
		Open: func(t *testing.T) backend.Backend { return New() },
		// No Reopen: memory has no persistence, the suite skips it.
	})
}

// set is a one-op WriteBatch helper.
func set(t *testing.T, s *Store, key, val string) error {
	t.Helper()
	b := &backend.Batch{}
	b.Set([]byte(key), []byte(val))
	return s.WriteBatch(context.Background(), b)
}

// TestCapacityRejectsAndAppliesNothing: with a byte budget, a batch that fits
// applies, one that would exceed it fails with ErrNoSpace and changes nothing,
// and freeing space lets a later write through.
func TestCapacityRejectsAndAppliesNothing(t *testing.T) {
	s := New(WithCapacity(10))
	ctx := context.Background()

	if err := set(t, s, "a", "12345"); err != nil { // 5 bytes, used=5
		t.Fatalf("write within capacity: %v", err)
	}
	if err := set(t, s, "b", "123456"); !errors.Is(err, backend.ErrNoSpace) { // +6 -> 11 > 10
		t.Fatalf("over-capacity write -> %v, want ErrNoSpace", err)
	}
	// The rejected batch applied nothing.
	if _, err := s.Get(ctx, []byte("b")); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("rejected key exists: %v", err)
	}
	// A fitting write still succeeds (used=5, +5 -> 10).
	if err := set(t, s, "b", "67890"); err != nil {
		t.Fatalf("write filling capacity exactly: %v", err)
	}
	// Deleting frees the budget for a new write.
	del := &backend.Batch{}
	del.Delete([]byte("a"))
	if err := s.WriteBatch(ctx, del); err != nil { // used=5
		t.Fatal(err)
	}
	if err := set(t, s, "c", "abcde"); err != nil { // +5 -> 10
		t.Fatalf("write after delete freed space: %v", err)
	}
}

// TestUnlimitedNeverRejects: the default backend has no budget and stores
// arbitrarily large values.
func TestUnlimitedNeverRejects(t *testing.T) {
	s := New()
	if err := set(t, s, "big", string(make([]byte, 1<<20))); err != nil {
		t.Fatalf("unlimited backend rejected a write: %v", err)
	}
}
