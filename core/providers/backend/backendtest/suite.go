// Package backendtest is the shared contract suite: every Backend
// implementation must pass the identical tests. A backend that passes
// is behaviorally interchangeable with every other.
package backendtest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// Config wires a backend implementation into the suite.
type Config struct {
	// Open returns a fresh, empty backend.
	Open func(t *testing.T) backend.Backend
	// Reopen closes b and reopens the same underlying storage, or nil
	// if the backend has no persistence (persistence tests are skipped).
	Reopen func(t *testing.T, b backend.Backend) backend.Backend
}

// Run executes the full contract suite.
func Run(t *testing.T, cfg Config) {
	t.Run("GetSetDelete", func(t *testing.T) { testGetSetDelete(t, cfg) })
	t.Run("NilValue", func(t *testing.T) { testNilValue(t, cfg) })
	t.Run("BinaryKeysAndValues", func(t *testing.T) { testBinary(t, cfg) })
	t.Run("ScanOrdering", func(t *testing.T) { testScanOrdering(t, cfg) })
	t.Run("ScanBoundsAndEarlyStop", func(t *testing.T) { testScanBounds(t, cfg) })
	t.Run("BatchAtomicity", func(t *testing.T) { testBatchAtomicity(t, cfg) })
	t.Run("Assert", func(t *testing.T) { testAssert(t, cfg) })
	t.Run("Add", func(t *testing.T) { testAdd(t, cfg) })
	t.Run("Persistence", func(t *testing.T) { testPersistence(t, cfg) })
}

func set(t *testing.T, b backend.Backend, key, value string) {
	t.Helper()
	var batch backend.Batch
	batch.Set([]byte(key), []byte(value))
	if err := b.WriteBatch(context.Background(), &batch); err != nil {
		t.Fatalf("Set(%q): %v", key, err)
	}
}

func testGetSetDelete(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()

	if _, err := b.Get(ctx, []byte("missing")); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Get(missing) err = %v, want ErrNotFound", err)
	}
	set(t, b, "k1", "v1")
	got, err := b.Get(ctx, []byte("k1"))
	if err != nil || string(got) != "v1" {
		t.Errorf("Get(k1) = %q, %v", got, err)
	}
	set(t, b, "k1", "v2") // overwrite
	if got, _ = b.Get(ctx, []byte("k1")); string(got) != "v2" {
		t.Errorf("overwrite: got %q", got)
	}
	// Empty: absent and empty must differ. The nil case is separate, and it
	// is NOT the same test - see testNilValue.
	set(t, b, "empty", "")
	if got, err = b.Get(ctx, []byte("empty")); err != nil || len(got) != 0 {
		t.Errorf("empty value: %q, %v", got, err)
	}
	var batch backend.Batch
	batch.Delete([]byte("k1"))
	batch.Delete([]byte("never-existed")) // idempotent
	if err := b.WriteBatch(ctx, &batch); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, []byte("k1")); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("deleted key still present (err %v)", err)
	}
}

// testNilValue pins the value contract: a nil value is a value, and it is the
// same value as an empty one. Only "absent" is different.
//
// This is not hair-splitting. The object DB stores a key-only entry - an index
// entry, a blob reference marker - by writing a nil value, because for those
// the key IS the data (see objectdb.setindex). A driver that maps nil onto
// something other than an empty byte string breaks every indexed write, and
// therefore breaks delivery.
//
// The suite tested `[]byte("")` before this and thought it had the case
// covered. It did not: that slice is empty but NON-NIL, and database/sql binds
// the two differently - an empty slice becomes a zero-length blob, a nil slice
// becomes SQL NULL. The SQLite driver's NOT NULL column rejected the latter,
// so a fully green test suite shipped a driver that could not store an index
// entry.
func testNilValue(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()

	var batch backend.Batch
	batch.Set([]byte("key-only"), nil)
	if err := b.WriteBatch(ctx, &batch); err != nil {
		t.Fatalf("Set(key, nil): %v", err)
	}

	got, err := b.Get(ctx, []byte("key-only"))
	if err != nil {
		t.Fatalf("Get after nil Set: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil value read back as %q, want empty", got)
	}

	// Present-with-nil-value must still be distinguishable from absent.
	if _, err := b.Get(ctx, []byte("absent")); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Get(absent) err = %v, want ErrNotFound", err)
	}

	// A nil value must be visible to a scan, since that is how index entries
	// are read back.
	seen := false
	if err := b.Scan(ctx, []byte("key-only"), []byte("key-onlz"), false,
		func(k, v []byte) bool {
			seen = true
			if len(v) != 0 {
				t.Errorf("scanned nil value as %q, want empty", v)
			}
			return true
		}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !seen {
		t.Error("key written with a nil value did not appear in a scan")
	}
}

func testBinary(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()
	key := []byte{0x00, 0xff, 0x00, 'k'}
	val := []byte{0xde, 0xad, 0x00, 0xbe, 0xef}
	var batch backend.Batch
	batch.Set(key, val)
	if err := b.WriteBatch(ctx, &batch); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, key)
	if err != nil || !bytes.Equal(got, val) {
		t.Errorf("binary roundtrip: %x, %v", got, err)
	}
}

func testScanOrdering(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()
	// Inserted deliberately unsorted; scan order must be bytes.Compare.
	for _, k := range []string{"b", "a/2", "a", "c", "a/10", "a\xff"} {
		set(t, b, k, "v")
	}
	want := []string{"a", "a/10", "a/2", "a\xff", "b", "c"}
	var got []string
	if err := b.Scan(ctx, nil, nil, false, func(k, _ []byte) bool {
		got = append(got, string(k))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("scan returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ascending order %v, want %v", got, want)
		}
	}
	got = got[:0]
	if err := b.Scan(ctx, nil, nil, true, func(k, _ []byte) bool {
		got = append(got, string(k))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[len(want)-1-i] {
			t.Fatalf("descending order %v", got)
		}
	}
}

func testScanBounds(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()
	for _, k := range []string{"p/a", "p/b", "p/c", "q/a"} {
		set(t, b, k, "v")
	}
	var got []string
	collect := func(k, _ []byte) bool { got = append(got, string(k)); return true }

	// Half-open [start, end).
	got = nil
	if err := b.Scan(ctx, []byte("p/a"), []byte("p/c"), false, collect); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "p/a" || got[1] != "p/b" {
		t.Errorf("[p/a, p/c) = %v", got)
	}
	// Prefix scan via end = prefix+0xff-successor convention.
	got = nil
	if err := b.Scan(ctx, []byte("p/"), []byte("p/\xff"), false, collect); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("prefix scan = %v", got)
	}
	// Empty range.
	got = nil
	if err := b.Scan(ctx, []byte("x"), []byte("y"), false, collect); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty range = %v", got)
	}
	// Early stop.
	got = nil
	if err := b.Scan(ctx, nil, nil, false, func(k, _ []byte) bool {
		got = append(got, string(k))
		return len(got) < 2
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("early stop visited %v", got)
	}
}

func testBatchAtomicity(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()
	set(t, b, "existing", "old")

	// A batch whose Assert fails must apply nothing, regardless of the
	// op order around the assert.
	var batch backend.Batch
	batch.Set([]byte("new1"), []byte("x"))
	batch.Assert([]byte("existing"), []byte("WRONG"))
	batch.Set([]byte("existing"), []byte("clobbered"))
	if err := b.WriteBatch(ctx, &batch); !errors.Is(err, backend.ErrAssertFailed) {
		t.Fatalf("err = %v, want ErrAssertFailed", err)
	}
	if _, err := b.Get(ctx, []byte("new1")); !errors.Is(err, backend.ErrNotFound) {
		t.Error("failed batch leaked a Set")
	}
	if got, _ := b.Get(ctx, []byte("existing")); string(got) != "old" {
		t.Errorf("failed batch mutated existing key: %q", got)
	}

	// Multi-op success is all-or-nothing observable.
	var ok backend.Batch
	ok.Set([]byte("a"), []byte("1"))
	ok.Set([]byte("b"), []byte("2"))
	ok.Delete([]byte("existing"))
	ok.Add([]byte("n"), 5)
	if err := b.WriteBatch(ctx, &ok); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(ctx, []byte("b")); string(got) != "2" {
		t.Error("batch op missing")
	}
	if _, err := b.Get(ctx, []byte("existing")); !errors.Is(err, backend.ErrNotFound) {
		t.Error("batch delete missing")
	}
}

func testAssert(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()

	// Assert-absent passes on a missing key...
	var b1 backend.Batch
	b1.Assert([]byte("lease"), nil)
	b1.Set([]byte("lease"), []byte("gen1"))
	if err := b.WriteBatch(ctx, &b1); err != nil {
		t.Fatalf("assert-absent: %v", err)
	}
	// ...and fails once present.
	var b2 backend.Batch
	b2.Assert([]byte("lease"), nil)
	if err := b.WriteBatch(ctx, &b2); !errors.Is(err, backend.ErrAssertFailed) {
		t.Errorf("assert-absent on present key: %v", err)
	}
	// Assert-value: match passes, stale fails (the fencing case).
	var b3 backend.Batch
	b3.Assert([]byte("lease"), []byte("gen1"))
	b3.Set([]byte("guarded"), []byte("ok"))
	if err := b.WriteBatch(ctx, &b3); err != nil {
		t.Errorf("assert-match: %v", err)
	}
	var b4 backend.Batch
	b4.Assert([]byte("lease"), []byte("gen0"))
	b4.Set([]byte("guarded"), []byte("stale-write"))
	if err := b.WriteBatch(ctx, &b4); !errors.Is(err, backend.ErrAssertFailed) {
		t.Errorf("stale fencing token accepted: %v", err)
	}
	if got, _ := b.Get(ctx, []byte("guarded")); string(got) != "ok" {
		t.Errorf("guarded = %q", got)
	}
}

func testAdd(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	ctx := context.Background()

	add := func(delta int64) error {
		var batch backend.Batch
		batch.Add([]byte("ctr"), delta)
		return b.WriteBatch(ctx, &batch)
	}
	counter := func() int64 {
		v, err := b.Get(ctx, []byte("ctr"))
		if err != nil {
			t.Fatal(err)
		}
		n, err := backend.DecodeInt64(v)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := add(5); err != nil || counter() != 5 {
		t.Fatalf("create-at-delta: %d, %v", counter(), err)
	}
	if err := add(3); err != nil || counter() != 8 {
		t.Fatalf("increment: %d", counter())
	}
	if err := add(-10); err != nil || counter() != -2 {
		t.Fatalf("negative: %d", counter())
	}
	// Two Adds to one key within a batch accumulate.
	var batch backend.Batch
	batch.Add([]byte("ctr"), 1)
	batch.Add([]byte("ctr"), 1)
	if err := b.WriteBatch(ctx, &batch); err != nil || counter() != 0 {
		t.Fatalf("in-batch accumulation: %d, %v", counter(), err)
	}
	// Adding to a non-counter value is an error and applies nothing.
	set(t, b, "text", "not-a-counter")
	var bad backend.Batch
	bad.Set([]byte("side"), []byte("x"))
	bad.Add([]byte("text"), 1)
	if err := b.WriteBatch(ctx, &bad); err == nil {
		t.Error("Add on malformed counter succeeded")
	}
	if _, err := b.Get(ctx, []byte("side")); !errors.Is(err, backend.ErrNotFound) {
		t.Error("failed Add batch leaked a Set")
	}
}

func testPersistence(t *testing.T, cfg Config) {
	if cfg.Reopen == nil {
		t.Skip("backend has no persistence")
	}
	b := cfg.Open(t)
	set(t, b, "durable", "value")
	var batch backend.Batch
	batch.Add([]byte("ctr"), 42)
	if err := b.WriteBatch(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}
	b = cfg.Reopen(t, b)
	defer b.Close()
	got, err := b.Get(context.Background(), []byte("durable"))
	if err != nil || string(got) != "value" {
		t.Errorf("after reopen: %q, %v", got, err)
	}
	v, err := b.Get(context.Background(), []byte("ctr"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := backend.DecodeInt64(v); n != 42 {
		t.Errorf("counter after reopen = %d", n)
	}
}
