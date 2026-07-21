// Package backendtest is the shared contract suite: every Backend
// implementation must pass the identical tests. A backend that passes
// is behaviorally interchangeable with every other.
package backendtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	t.Run("CompareAndSwap", func(t *testing.T) { testCompareAndSwap(t, cfg) })
	t.Run("MultiGet", func(t *testing.T) { testMultiGet(t, cfg) })
	t.Run("MultiGetConcurrent", func(t *testing.T) { testMultiGetConcurrent(t, cfg) })
}

// testMultiGet exercises the optional backend.MultiGetter for basic
// correctness against Get: present keys, an absent key (nil, not an error),
// and a duplicate key in one call (both slots must carry its value).
// Backends that do not implement it are skipped - objectdb.DB.GetMany falls
// back to sequential Get calls on those, so there is nothing of this
// capability's own to verify.
func testMultiGet(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	mg, ok := b.(backend.MultiGetter)
	if !ok {
		t.Skip("backend does not implement MultiGetter")
	}
	ctx := context.Background()
	set(t, b, "mg-a", "val-a")
	set(t, b, "mg-b", "val-b")

	keys := [][]byte{[]byte("mg-a"), []byte("mg-missing"), []byte("mg-b"), []byte("mg-a")}
	got, err := mg.MultiGet(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Fatalf("MultiGet returned %d results, want %d", len(got), len(keys))
	}
	want := [][]byte{[]byte("val-a"), nil, []byte("val-b"), []byte("val-a")}
	for i := range keys {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("key %d (%q): MultiGet = %q, want %q", i, keys[i], got[i], want[i])
		}
	}
	if got, err := mg.MultiGet(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("MultiGet(empty) = %v, %v, want (empty, nil)", got, err)
	}
}

// testMultiGetConcurrent is specifically for a reuse or locking bug in a
// MultiGetter implementation that a single-goroutine test cannot show: many
// goroutines call MultiGet on overlapping keys at once, each checking every
// returned value against that key's known fingerprint - a bug that mixed up
// which result belongs to which key (a pooled buffer, a map race, an
// off-by-one in matching results back to input order) surfaces as a
// mismatched fingerprint, not just a crash -race would already catch.
func testMultiGetConcurrent(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	mg, ok := b.(backend.MultiGetter)
	if !ok {
		t.Skip("backend does not implement MultiGetter")
	}
	ctx := context.Background()

	const nKeys = 40
	keys := make([][]byte, nKeys)
	want := make(map[string][]byte, nKeys)
	for i := 0; i < nKeys; i++ {
		k := []byte(fmt.Sprintf("mgc-key-%d", i))
		v := []byte(fmt.Sprintf("fingerprint-%d-%x", i, i*104729))
		set(t, b, string(k), string(v))
		keys[i] = k
		want[string(k)] = v
	}

	const goroutines = 32
	const itersPerGoroutine = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < itersPerGoroutine; i++ {
				got, err := mg.MultiGet(ctx, keys)
				if err != nil {
					errs <- fmt.Errorf("goroutine %d iter %d: %w", g, i, err)
					return
				}
				if len(got) != len(keys) {
					errs <- fmt.Errorf("goroutine %d iter %d: got %d results, want %d", g, i, len(got), len(keys))
					return
				}
				for j, k := range keys {
					if !bytes.Equal(got[j], want[string(k)]) {
						errs <- fmt.Errorf("goroutine %d iter %d: key %q = %q, want %q (fingerprint mismatch)", g, i, k, got[j], want[string(k)])
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// testCompareAndSwap exercises the optional backend.CompareAndSwapper: under
// concurrent contention on one key, exactly one CompareAndSwap succeeds per
// round and the stored value ends as that winner's. Backends that do not
// implement the capability are skipped - their globally serialized writes make
// an Assert+Set batch an effective compare-and-swap already, so a lease built on
// them is safe without it. A backend with genuinely concurrent connection-level
// writers (where an Assert and a Set can interleave with another writer) must
// implement it, and only a real concurrent run against such a backend catches a
// non-atomic claim: a globally serialized backend cannot reproduce the race, so
// this coverage lives with the driver that has real concurrency.
func testCompareAndSwap(t *testing.T, cfg Config) {
	b := cfg.Open(t)
	defer b.Close()
	cas, ok := b.(backend.CompareAndSwapper)
	if !ok {
		t.Skip("backend does not implement CompareAndSwapper")
	}
	ctx := context.Background()
	key := []byte("cas/contended")

	const racers = 8
	const rounds = 100
	var expected []byte // nil for the first round: the key must be absent
	for r := 0; r < rounds; r++ {
		var winners int64
		var winner atomic.Value // []byte
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			token := []byte(fmt.Sprintf("r%d-w%d", r, i))
			wg.Add(1)
			go func() {
				defer wg.Done()
				swapped, err := cas.CompareAndSwap(ctx, key, expected, token)
				if err != nil {
					t.Errorf("round %d: CompareAndSwap: %v", r, err)
					return
				}
				if swapped {
					atomic.AddInt64(&winners, 1)
					winner.Store(token)
				}
			}()
		}
		wg.Wait()
		if winners != 1 {
			t.Fatalf("round %d: %d concurrent swaps succeeded, want exactly 1", r, winners)
		}
		won := winner.Load().([]byte)
		got, err := b.Get(ctx, key)
		if err != nil {
			t.Fatalf("round %d: Get: %v", r, err)
		}
		if !bytes.Equal(got, won) {
			t.Fatalf("round %d: stored %q, want the winner's %q", r, got, won)
		}
		// The next round contends against the value this round settled on, so
		// every round after the first exercises the present->present swap.
		expected = won
	}
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
