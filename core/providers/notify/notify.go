// Package notify defines the Notifier socket: post-commit change
// fan-out feeding push (RFC 8620 section 7).
//
// The Notifier is the ephemeral, best-effort tier of the runtime's
// consistency model. Everything correctness-critical (objects, indexes,
// the change log entry) commits in one atomic batch; a notification
// only tells a connected client to resync sooner. Section 7 makes the
// lossy contract explicit: "It doesn't matter if some push events are
// dropped before they reach the client; the next time it gets/sets any
// records of a changed type, it will discover the data has changed and
// still sync all changes."
//
// It is deliberately named Notifier, not EventBus: it must never be
// used as a durable stream. Durable consumers (search indexing, audit,
// replication) follow the change log with a cursor instead.
package notify

import (
	"context"
	"errors"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// ErrClosed reports a Wait on a closed Subscription.
var ErrClosed = errors.New("notify: subscription closed")

// Changes maps an account id to the new state strings of the types
// that changed there - the exact shape of a StateChange "changed"
// property (RFC 8620 section 7.1).
type Changes map[jmap.Id]jmap.TypeState

// Notifier fans commit notifications out to subscribers. In-process is
// the single-node implementation; cluster deployments plug a
// store-backed one (e.g. Postgres LISTEN/NOTIFY) with the same
// interface.
type Notifier interface {
	// Publish announces that types in account changed, carrying their
	// new state strings. It is best-effort and deliberately returns
	// nothing: the commit it describes has already durably happened,
	// and a caller that could observe a delivery failure might be
	// tempted to fail the write over it.
	Publish(ctx context.Context, account jmap.Id, types jmap.TypeState)
	// Subscribe registers interest in a set of accounts.
	Subscribe(ctx context.Context, accounts []jmap.Id) (Subscription, error)
}

// Subscription receives coalesced changes for its accounts. Changes
// accumulate while the subscriber is busy, merging per type with the
// newest state winning - exactly the coalescing section 7 asks for
// ("multiple changes to be coalesced into a single minimal StateChange
// object").
type Subscription interface {
	// Wait blocks until at least one change is pending, then returns
	// the pending set and clears it. It returns ctx.Err() if ctx ends
	// first, or ErrClosed once the subscription is closed.
	Wait(ctx context.Context) (Changes, error)
	// Close unregisters the subscription and releases any blocked Wait.
	Close()
}
