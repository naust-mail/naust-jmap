package backend

import "context"

// MultiGetter is an optional Backend capability: reading several keys as one
// round trip instead of one Get call per key. A backend fronted by a network
// database (Postgres) pays a full round trip per Get; a caller reading N
// records - loadAndMatch verifying query candidates, the generic /get
// method reading every requested id - turns N round trips into one when the
// backend implements this, and keeps working unchanged (just without the
// saving) when it doesn't, the same optional-capability shape as
// CompareAndSwapper.
type MultiGetter interface {
	// MultiGet returns one result per key, in the same order, nil for a key
	// that Get would report ErrNotFound for. The overall error is for a
	// failure of the batch itself (a connection error, a canceled context),
	// never for an individual missing key - that is what the nil slot means.
	MultiGet(ctx context.Context, keys [][]byte) ([][]byte, error)
}
