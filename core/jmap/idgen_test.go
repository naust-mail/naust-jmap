package jmap

import (
	"sort"
	"testing"
	"time"
)

// Tests for the three id generators. All three must emit valid RFC 8620
// section 1.2 ids with the defensive leading letter; the ULID and Sequence
// forms additionally promise that lexical order equals creation order, which
// rests on two facts these tests pin: the Crockford alphabet ascends (so
// base32 order equals byte order at equal length) and each generator's output
// length is constant (so no shorter id ever sorts by truncation).

// ulidLen and sequenceIdLen are the constant encoded lengths: the prefix plus
// unpadded base32 of 16 bytes (26 chars) and of 20 bytes (32 chars).
const (
	ulidLen       = 1 + 26
	sequenceIdLen = 1 + 32
)

func checkGeneratedId(t *testing.T, id Id, wantLen int) {
	t.Helper()
	if !id.Valid() {
		t.Fatalf("generated id %q is not a valid section 1.2 id", id)
	}
	if id[0] != 'N' {
		t.Fatalf("generated id %q does not carry the N prefix", id)
	}
	if len(id) != wantLen {
		t.Fatalf("generated id %q is %d chars, want the constant %d", id, len(id), wantLen)
	}
}

func TestNewIdShape(t *testing.T) {
	seen := make(map[Id]bool)
	for i := 0; i < 200; i++ {
		id := NewId()
		checkGeneratedId(t, id, ulidLen) // same 16 random bytes as a ULID
		if seen[id] {
			t.Fatalf("NewId repeated %q", id)
		}
		seen[id] = true
	}
}

// TestNewULIDSortsByTime: ids stamped at increasing wall-clock milliseconds
// must sort lexically in stamp order regardless of their random bits.
func TestNewULIDSortsByTime(t *testing.T) {
	base := time.UnixMilli(1_720_000_000_000)
	var ids []Id
	for i := 0; i < 100; i++ {
		id := NewULID(base.Add(time.Duration(i) * time.Millisecond))
		checkGeneratedId(t, id, ulidLen)
		ids = append(ids, id)
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		t.Fatal("ULIDs with increasing timestamps are not in lexical order")
	}
}

// TestNewULIDDistinctWithinMillisecond: two ids in the same millisecond share
// a stamp but must still differ (80 random bits).
func TestNewULIDDistinctWithinMillisecond(t *testing.T) {
	now := time.UnixMilli(1_720_000_000_000)
	seen := make(map[Id]bool)
	for i := 0; i < 200; i++ {
		id := NewULID(now)
		if seen[id] {
			t.Fatalf("NewULID repeated %q within one millisecond", id)
		}
		seen[id] = true
	}
}

// TestNewSequenceIdSortsByPair: ids must sort by (sequence, index) - the
// in-account creation order - regardless of their random tails, across both
// the within-commit boundary and the commit boundary, including values that
// straddle byte boundaries in the encoding.
func TestNewSequenceIdSortsByPair(t *testing.T) {
	pairs := []struct{ seq, index int64 }{
		{1, 0}, {1, 1}, {1, 2}, {1, 255}, {1, 256},
		{2, 0}, {255, 7}, {256, 0}, {1 << 20, 3}, {1 << 40, 0},
	}
	var ids []Id
	for _, p := range pairs {
		id := NewSequenceId(p.seq, p.index)
		checkGeneratedId(t, id, sequenceIdLen)
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		if !(ids[i-1] < ids[i]) {
			t.Fatalf("sequence id for %+v does not sort after %+v: %q >= %q",
				pairs[i], pairs[i-1], ids[i-1], ids[i])
		}
	}
}

// TestNewSequenceIdDistinctAtSamePair: the same (sequence, index) pair must
// still yield distinct ids (64 random tail bits), since two accounts share
// the same sequence numbering space.
func TestNewSequenceIdDistinctAtSamePair(t *testing.T) {
	seen := make(map[Id]bool)
	for i := 0; i < 200; i++ {
		id := NewSequenceId(7, 3)
		if seen[id] {
			t.Fatalf("NewSequenceId repeated %q at a fixed pair", id)
		}
		seen[id] = true
	}
}

// TestCrockfordAlphabetAscends pins the fact the ordering claims above rest
// on: the alphabet ascends in ASCII, so base32 order equals byte order for
// equal-length encodings. A reordered alphabet would silently break every
// sorted-id promise while still producing valid ids.
func TestCrockfordAlphabetAscends(t *testing.T) {
	for i := 1; i < len(crockfordAlphabet); i++ {
		if crockfordAlphabet[i-1] >= crockfordAlphabet[i] {
			t.Fatalf("alphabet not ascending at %q >= %q",
				crockfordAlphabet[i-1], crockfordAlphabet[i])
		}
	}
}
