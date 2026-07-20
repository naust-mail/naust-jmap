package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// fill writes n keys k0000..k<n-1> in one batch and returns their sorted
// names - enough keys to force Scan through several chunks.
func fill(t *testing.T, s *Store, n int) []string {
	t.Helper()
	b := &backend.Batch{}
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%04d", i)
		b.Set([]byte(keys[i]), []byte{byte(i)})
	}
	if err := s.WriteBatch(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return keys
}

func collect(t *testing.T, s *Store, start, end string, reverse bool) []string {
	t.Helper()
	var got []string
	var sb, eb []byte
	if start != "" {
		sb = []byte(start)
	}
	if end != "" {
		eb = []byte(end)
	}
	err := s.Scan(context.Background(), sb, eb, reverse, func(k, _ []byte) bool {
		got = append(got, string(k))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestScanChunkResume: scans larger than one internal chunk resume
// without skipping or repeating keys, ascending and descending, with
// bounds that land mid-chunk. The conformance suite's sets are smaller
// than a chunk, so this is the resume path's direct gate.
func TestScanChunkResume(t *testing.T) {
	s := New()
	keys := fill(t, s, 200)

	got := collect(t, s, "", "", false)
	if !slices.Equal(got, keys) {
		t.Fatalf("full ascending scan: got %d keys, first/last %q/%q", len(got), got[0], got[len(got)-1])
	}
	rev := collect(t, s, "", "", true)
	slices.Reverse(rev)
	if !slices.Equal(rev, keys) {
		t.Fatalf("full descending scan mismatch: %d keys", len(rev))
	}
	if got := collect(t, s, "k0050", "k0150", false); !slices.Equal(got, keys[50:150]) {
		t.Fatalf("bounded ascending scan: got %d keys, want 100", len(got))
	}
	if got := collect(t, s, "k0050", "k0150", true); len(got) != 100 || got[0] != "k0149" || got[99] != "k0050" {
		t.Fatalf("bounded descending scan: got %d keys, edges %q/%q", len(got), got[0], got[len(got)-1])
	}
}

// TestScanReentrantDelete: fn deleting the key it was just handed (the
// scan-and-sweep shape chunkstore uses) must not deadlock and must
// still visit every key - the resume cursor is the key itself, so
// removals behind it cannot shift what comes next.
func TestScanReentrantDelete(t *testing.T) {
	s := New()
	keys := fill(t, s, 200)
	var visited []string
	err := s.Scan(context.Background(), nil, nil, false, func(k, _ []byte) bool {
		visited = append(visited, string(k))
		b := &backend.Batch{}
		b.Delete(k)
		if err := s.WriteBatch(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(visited, keys) {
		t.Fatalf("visited %d of %d keys", len(visited), len(keys))
	}
	if got := collect(t, s, "", "", false); len(got) != 0 {
		t.Fatalf("%d keys survived the sweep", len(got))
	}
}
