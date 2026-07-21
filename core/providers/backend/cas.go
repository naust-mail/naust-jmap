package backend

import "context"

// CompareAndSwapper is an OPTIONAL backend capability: a single-key
// compare-and-swap that is atomic and linearizable with respect to concurrent
// CompareAndSwap and WriteBatch calls on the same key.
//
// The six-operation Batch guarantees atomicity (all-or-nothing) but NOT
// isolation between concurrent batches touching the same key: an Assert reads
// and a Set writes as separate steps, so on a backend with genuinely concurrent
// connection-level writers two batches can both read the old value and both
// write. A backend whose writes are globally serialized (an in-memory backend
// under one lock, or a single-write-connection engine) already makes an
// Assert+Set batch an effective compare-and-swap; a backend with independent
// concurrent writers (for example Postgres) MUST implement this to correctly
// host a per-account writer lease, whose claim is exactly such a compare-and-swap.
type CompareAndSwapper interface {
	// CompareAndSwap sets key to newValue only if its current stored value
	// equals expected (expected == nil means the key must be absent), reporting
	// whether the swap occurred. It exposes no read-modify-write window another
	// writer can enter. newValue follows the same nil-means-empty normalization
	// as Batch.Set.
	CompareAndSwap(ctx context.Context, key, expected, newValue []byte) (swapped bool, err error)
}
