package mail

// SubmissionQueue is the live view of the outbound queue. The durable
// truth is the EmailSubmission records themselves (the internal
// nextAttemptAt index holds exactly the pending work in due order, and
// the queue tag names the accounts holding any); the queue object
// carries only the bell that wakes a worker the moment new work
// commits. RegisterEmailSubmission creates it and rings it
// unconditionally, so "every mutation that may queue work rings" holds
// structurally: with no worker running, rings land harmlessly and the
// records queue durably until one is attached (NewSubmissionWorker)
// and started.

import (
	"context"
	"errors"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

// submissionQueueTag is the account-tag worklist "accounts with queued
// submissions": set in the same commit as any mutation that leaves work
// queued, cleared by the worker under the account lease once a
// lease-held probe confirms the account's queue is empty. It keeps the
// worker's sweep proportional to accounts with actual work.
const submissionQueueTag = "mail:submission-queued"

// SubmissionQueue accelerates the durable submission queue for a
// worker. Obtain one from RegisterEmailSubmission. It holds no queue
// state - the worker's sweep reconstructs everything from durable
// records - so an idle queue costs one empty channel.
type SubmissionQueue struct {
	db    *objectdb.DB
	store blob.Store

	// bell has capacity 1: a ring landing while the worker is mid-sweep
	// is retained as one token, never lost, and rings coalesce.
	bell chan struct{}
}

func newSubmissionQueue(db *objectdb.DB, store blob.Store) *SubmissionQueue {
	return &SubmissionQueue{db: db, store: store, bell: make(chan struct{}, 1)}
}

// ring wakes any worker draining the bell: work may have been queued.
// It carries no payload - the worker's sweep reads what and when from
// durable state - and is safe from any goroutine. In-process it is
// lossless: a same-process commit's mail leaves immediately, whatever
// the Notifier does.
func (q *SubmissionQueue) ring() {
	select {
	case q.bell <- struct{}{}:
	default:
	}
}

// probe reads acct's earliest pending due time from the index.
func (q *SubmissionQueue) probe(ctx context.Context, acct jmap.Id) (time.Time, bool, error) {
	ids, err := q.db.IdsWhereAtMost(ctx, acct, TypeEmailSubmission, "nextAttemptAt", nil, 1)
	if err != nil || len(ids) == 0 {
		return time.Time{}, false, err
	}
	rec, err := q.db.Get(ctx, acct, TypeEmailSubmission, ids[0])
	if errors.Is(err, objectdb.ErrNotFound) {
		return time.Time{}, false, nil // destroyed between scan and read
	}
	if err != nil {
		return time.Time{}, false, err
	}
	due, err := parseUTCDateValue(rec["nextAttemptAt"])
	if err != nil {
		return time.Time{}, false, nil
	}
	return due, true, nil
}
