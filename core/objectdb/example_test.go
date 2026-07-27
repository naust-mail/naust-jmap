package objectdb_test

import (
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// The object database is built over two providers: a Backend for storage
// and a lease Manager for per-account write serialization. Swapping the
// backend for a driver module (drivers/sqlite, drivers/postgres) changes
// these two lines and nothing above them.
func ExampleNew() {
	be := memory.New()

	// Single process: the in-process lease manager. A fleet sharing one
	// store uses lease.NewStoreLease instead, with the same interface.
	db := objectdb.New(be, lease.NewInProcess(be))

	_ = db
}

// Options set at construction. WithIdScheme picks how record ids are
// minted; WithNow is the clock seam tests use to make change-log
// retention and blob ages deterministic.
func ExampleNew_options() {
	be := memory.New()

	db := objectdb.New(be, lease.NewInProcess(be),
		objectdb.WithIdScheme(tuning.SchemeULID),
	)

	_ = db
}
