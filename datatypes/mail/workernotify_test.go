package mail

// Notifier-wake tests: the cross-process fast path. The sweep is the
// correctness floor (TestWorkerCrossProcessDiscovery proves it alone
// suffices); these prove the accelerator - a worker whose bell no
// producer rings still wakes promptly when the db's Notifier carries an
// EmailSubmission commit, and the pump only rings for submission
// changes.

import (
	"context"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// TestWorkerPumpFiltersForSubmissionChanges: the pump rings the bell
// only for commits whose changed types include EmailSubmission - other
// types' churn never wakes the worker.
func TestWorkerPumpFiltersForSubmissionChanges(t *testing.T) {
	_, db, store, _, _, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	w, err := NewSubmissionWorker(newSubmissionQueue(db, store), &fakeSubmitter{}, SubmissionWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	n := notify.NewInProcess()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := n.SubscribeAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	go w.pump(ctx, sub)

	// An Email-only commit must not ring.
	n.Publish(ctx, testAccount, jmap.TypeState{"Email": "3"})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-w.q.bell:
		t.Fatal("pump rang for a non-submission change")
	default:
	}

	// A commit touching EmailSubmission rings.
	n.Publish(ctx, testAccount, jmap.TypeState{TypeEmailSubmission: "4"})
	deadline := time.After(2 * time.Second)
	select {
	case <-w.q.bell:
	case <-deadline:
		t.Fatal("pump never rang for a submission change")
	}
}

// TestWorkerNotifierCrossProcessWake: a second worker over the same
// store whose own bell no producer rings (the cross-process shape) is
// woken by the db's Notifier and sends promptly - the scan interval is
// set to an hour, so only the firehose subscription can explain a fast
// pickup.
func TestWorkerNotifierCrossProcessWake(t *testing.T) {
	ts, db, store, _, _, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	db.SetNotifier(notify.NewInProcess())
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	// "Process B": its own queue view; nothing rings its bell directly.
	fakeB := &fakeSubmitter{}
	wB, err := NewSubmissionWorker(newSubmissionQueue(db, store), fakeB,
		SubmissionWorkerConfig{QueueScanInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	// Seed one due submission BEFORE B starts: B's first sweep sends it,
	// which proves B is past its subscribe point and asleep on the bell.
	submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"one@remote.example"}]}`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- wB.Run(ctx) }()
	waitForCalls := func(want int, why string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for fakeB.callCount() < want {
			select {
			case <-deadline:
				t.Fatalf("%s: worker B saw %d transmits, want %d", why, fakeB.callCount(), want)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	waitForCalls(1, "startup sweep")

	// Process A commits a new submission. A's bell is not B's; with the
	// next sweep an hour out, only the Notifier can wake B now.
	submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"two@remote.example"}]}`)
	waitForCalls(2, "notifier wake")

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
}
