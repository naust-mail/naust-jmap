package lease_test

import (
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/lease/leasetest"
)

// TestStoreLeaseContract runs the shared lease contract suite against the
// store-backed manager over the in-memory backend.
func TestStoreLeaseContract(t *testing.T) {
	leasetest.Run(t,
		func(t *testing.T) backend.Backend { return memory.New() },
		func(be backend.Backend) lease.Manager {
			return lease.NewStoreLease(be, lease.StoreLeaseConfig{})
		},
	)
}
