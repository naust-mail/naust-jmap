package submit

// Queue is the live view of the outbound queue. The durable
// truth is the EmailSubmission records themselves (the internal
// nextAttemptAt index holds exactly the pending work in due order, and
// the queue tag names the accounts holding any); the queue object
// carries only the bell that wakes a worker the moment new work
// commits. Register creates it and rings it
// unconditionally, so "every mutation that may queue work rings" holds
// structurally: with no worker running, rings land harmlessly and the
// records queue durably until one is attached (NewWorker)
// and started.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// submissionQueueTag is the account-tag worklist "accounts with queued
// submissions": set in the same commit as any mutation that leaves work
// queued, cleared by the worker under the account lease once a
// lease-held probe confirms the account's queue is empty. It keeps the
// worker's sweep proportional to accounts with actual work.
const submissionQueueTag = "mail:submission-queued"

// Queue accelerates the durable submission queue for a
// worker. Obtain one from Register. It holds no queue
// state - the worker's sweep reconstructs everything from durable
// records - so an idle queue costs one empty channel.
type Queue struct {
	db    *objectdb.DB
	store blob.Store

	// The sending policy Register resolved: the queue is the door every
	// outgoing message passes through, so it is where the policy is
	// read back from (Policy).
	policy mail.SendPolicy

	// bell has capacity 1: a ring landing while the worker is mid-sweep
	// is retained as one token, never lost, and rings coalesce.
	bell chan struct{}
}

func newSubmissionQueue(db *objectdb.DB, store blob.Store) *Queue {
	return &Queue{db: db, store: store, bell: make(chan struct{}, 1)}
}

// Policy returns the SendPolicy this queue was registered with (the
// resolved value: the deny-everything default when Config.Policy was
// nil). Packages that originate mail through Sender re-check it at use,
// so one policy answers for every outbound path. The returned value
// carries only the two checks: the registered policy's concrete type -
// and any mutator it has - is unreachable through it, so a policy in
// service cannot be modified from here by construction.
func (q *Queue) Policy() mail.SendPolicy { return policyView{q.policy} }

// policyView narrows the registered SendPolicy to the SendPolicy
// interface alone, defeating a type assertion back to the concrete
// implementation.
type policyView struct{ p mail.SendPolicy }

func (v policyView) CanSend(ctx context.Context, acct jmap.Id) (bool, string) {
	return v.p.CanSend(ctx, acct)
}

func (v policyView) CanSendAs(ctx context.Context, acct jmap.Id, from string) bool {
	return v.p.CanSendAs(ctx, acct, from)
}

// ring wakes any worker draining the bell: work may have been queued.
// It carries no payload - the worker's sweep reads what and when from
// durable state - and is safe from any goroutine. In-process it is
// lossless: a same-process commit's mail leaves immediately, whatever
// the Notifier does.
func (q *Queue) ring() {
	select {
	case q.bell <- struct{}{}:
	default:
	}
}

// probe reads acct's earliest pending due time from the index.
func (q *Queue) probe(ctx context.Context, acct jmap.Id) (time.Time, bool, error) {
	ids, err := q.db.IdsWhereAtMost(ctx, acct, record.TypeEmailSubmission, "nextAttemptAt", nil, 1)
	if err != nil || len(ids) == 0 {
		return time.Time{}, false, err
	}
	rec, err := q.db.Get(ctx, acct, record.TypeEmailSubmission, ids[0])
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

// EmailIDForMessageID resolves the Message-ID of a message this account
// submitted to the Email the submission carried, via the indexed
// messageId snapshot on EmailSubmission. It answers the reverse
// correlation an inbound disposition or delivery report poses
// (Original-Message-ID, RFC 8098 section 3.2.5). ok is false when no
// submission matches, when more than one does (an ambiguous Message-ID
// identifies nothing), or when the matching record vanished between
// scan and read; false is an answer, not an error - a caller maps it
// to a null correlation.
func (q *Queue) EmailIDForMessageID(ctx context.Context, acct jmap.Id, messageID string) (jmap.Id, bool, error) {
	ids, err := q.db.IdsWhereEqual(ctx, acct, record.TypeEmailSubmission, "messageId", record.MustJSON(messageID), 2)
	if err != nil {
		return "", false, err
	}
	if len(ids) != 1 {
		return "", false, nil
	}
	rec, err := q.db.Get(ctx, acct, record.TypeEmailSubmission, ids[0])
	if errors.Is(err, objectdb.ErrNotFound) {
		return "", false, nil // destroyed between scan and read
	}
	if err != nil {
		return "", false, err
	}
	var emailID jmap.Id
	if json.Unmarshal(rec["emailId"], &emailID) != nil || emailID == "" {
		return "", false, nil
	}
	return emailID, true, nil
}
