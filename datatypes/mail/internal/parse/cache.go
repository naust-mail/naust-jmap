package parse

import "context"

// Cache memoizes the parsed structure of the record currently being
// matched, so the several text conditions evaluated against one record
// (RFC 8621 section 4.4.1) open and parse its blob once rather than once
// per condition. It holds a single record's result; the query runtime
// installs a fresh one per record via NewRecordContext.
//
// BlobID is a plain string (rather than a core jmap.Id) because this
// package must not import core: the caller converts.
type Cache struct {
	BlobID string
	Msg    *Parsed
	Err    error
	Done   bool
	// Memo holds values the caller wants remembered for the record's
	// blob - the naive searcher's per-terms body scans, keyed by the same
	// terms key it looks them up with. The caller resets it when the blob
	// changes.
	Memo map[string]any
}

// cacheKey is the context key under which NewRecordContext installs the
// per-record Cache.
type cacheKey struct{}

// NewRecordContext installs a fresh, empty Cache in ctx for the record
// about to be matched.
func NewRecordContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheKey{}, &Cache{})
}

// CacheFrom returns the Cache installed by NewRecordContext, if any.
func CacheFrom(ctx context.Context) (*Cache, bool) {
	c, ok := ctx.Value(cacheKey{}).(*Cache)
	return c, ok
}
