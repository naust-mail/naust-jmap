package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/lease/leasetest"
)

// TestStoreLeaseContract runs the shared lease contract suite against the
// store-backed manager over a real SQLite backend, guarding against any
// backend-specific behavior the in-memory backend would not surface.
func TestStoreLeaseContract(t *testing.T) {
	leasetest.Run(t,
		func(t *testing.T) backend.Backend {
			s, err := Open(filepath.Join(t.TempDir(), "lease.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		func(be backend.Backend) lease.Manager {
			return lease.NewStoreLease(be, lease.StoreLeaseConfig{})
		},
	)
}
