package postgres

import (
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/lease/leasetest"
)

// TestStoreLeaseContract runs the shared lease contract suite against the
// store-backed manager over a real Postgres backend. Unlike the in-memory and
// SQLite backends, Postgres has independent concurrent writers (one pooled
// connection per statement), so this is the only backend whose claim swap is
// not made atomic for free by global write serialization. The concurrent
// compare-and-swap that underlies that claim is exercised separately by the
// backend contract suite's CompareAndSwap test (see TestContract).
func TestStoreLeaseContract(t *testing.T) {
	leasetest.Run(t,
		func(t *testing.T) backend.Backend { return openTestDB(t) },
		func(be backend.Backend) lease.Manager {
			return lease.NewStoreLease(be, lease.StoreLeaseConfig{})
		},
	)
}
