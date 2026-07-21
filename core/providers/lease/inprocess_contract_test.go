package lease_test

import (
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/lease/leasetest"
)

// TestInProcessContract runs the shared lease contract suite against the
// single-node in-process manager over the in-memory backend.
func TestInProcessContract(t *testing.T) {
	leasetest.Run(t,
		func(t *testing.T) backend.Backend { return memory.New() },
		func(be backend.Backend) lease.Manager { return lease.NewInProcess(be) },
	)
}
