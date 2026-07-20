package mail

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// unparkSubmission rewrites a record's nextAttemptAt to the given time,
// making a parked claim's record due again without moving any clock -
// the staging trick for claim-takeover scenarios.
func unparkSubmission(t *testing.T, db *objectdb.DB, id string, at time.Time) {
	t.Helper()
	_, err := db.Update(context.Background(), testAccount, func(u *objectdb.Update) error {
		obj, err := u.Get(TypeEmailSubmission, jmap.Id(id))
		if err != nil {
			return err
		}
		obj["nextAttemptAt"] = mustJSON(at.UTC().Format(time.RFC3339))
		return u.Put(TypeEmailSubmission, jmap.Id(id), obj)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestWorkerStatsSendFlow: a clean drain moves exactly the claim and
// sweep counters - every failure-signal counter stays zero.
func TestWorkerStatsSendFlow(t *testing.T) {
	ts, db, store, w, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	clock.advance(time.Second)

	sent, _, err := w.ProcessDue(context.Background(), 0)
	if err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	s := w.Stats()
	if s.ClaimsTaken != 1 || s.Sweeps < 2 {
		t.Fatalf("claims %d sweeps %d, want 1 claim and >=2 sweeps", s.ClaimsTaken, s.Sweeps)
	}
	if s.ZeroProgressSweeps != 0 || s.ClaimsReclaimed != 0 ||
		s.ClaimsLostBeforeTransmit != 0 || s.FinalizesSuperseded != 0 || s.FutureStamps != 0 {
		t.Fatalf("failure counters moved on a clean drain: %+v", s)
	}
}

// TestWorkerStatsReclaim: an expired claim's takeover ticks
// ClaimsReclaimed, and the leftover stamp - a full window in the past -
// never reads as future-dated.
func TestWorkerStatsReclaim(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	if _, ok := w.claim(ctx, testAccount, jmap.Id(id)); !ok {
		t.Fatal("claim failed")
	}
	// The claim's window expires undelivered (a crash), and a sweep
	// takes the record over.
	clock.advance(w.cfg.ClaimWindow + time.Second)
	sent, _, _, err := w.sweep(ctx, 0)
	if err != nil || sent != 1 || fake.callCount() != 1 {
		t.Fatalf("reclaim sweep sent %d (submitter %d), err %v", sent, fake.callCount(), err)
	}
	s := w.Stats()
	if s.ClaimsTaken != 2 || s.ClaimsReclaimed != 1 {
		t.Fatalf("claims %d reclaimed %d, want 2/1", s.ClaimsTaken, s.ClaimsReclaimed)
	}
	if s.FutureStamps != 0 {
		t.Fatalf("a past leftover stamp counted as future: %+v", s)
	}
}

// TestWorkerStatsClaimLossAndSkew: a peer running 2s ahead steals two of
// this worker's claims - one discovered before transmit, one at
// finalize. Both loss counters tick on the losing worker, and the
// future-stamp signal ticks ONLY there: the laggard observes future
// stamps, the fast peer observes past ones (the inward framing).
func TestWorkerStatsClaimLossAndSkew(t *testing.T) {
	ts, db, store, wB, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	env := `{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`
	id1 := submitEnvelope(t, ts, identityId, emailId, env)
	id2 := submitEnvelope(t, ts, identityId, emailId, env)
	ctx := context.Background()
	clock.advance(time.Second)

	wA, err := NewSubmissionWorker(newSubmissionQueue(db, store), &fakeSubmitter{}, SubmissionWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	clockA := &testClock{t: clock.now().Add(2 * time.Second)}
	wA.now = clockA.now

	// id1: B claims, A steals it, B's pre-transmit re-read discovers the
	// loss - A's stamp is 2s in B's future.
	cB1, ok := wB.claim(ctx, testAccount, jmap.Id(id1))
	if !ok {
		t.Fatal("B's claim of id1 failed")
	}
	unparkSubmission(t, db, id1, clock.now())
	if _, ok := wA.claim(ctx, testAccount, jmap.Id(id1)); !ok {
		t.Fatal("A's takeover of id1 failed")
	}
	wB.sendOne(ctx, testAccount, cB1)
	if s := wB.Stats(); s.ClaimsLostBeforeTransmit != 1 || s.FinalizesSuperseded != 0 || s.FutureStamps != 1 {
		t.Fatalf("after pre-transmit loss: %+v", s)
	}

	// id2: B claims, A steals it, B's finalize arrives after its (never
	// started) transmit phase - the duplicate-possible counter.
	cB2, ok := wB.claim(ctx, testAccount, jmap.Id(id2))
	if !ok {
		t.Fatal("B's claim of id2 failed")
	}
	unparkSubmission(t, db, id2, clock.now())
	if _, ok := wA.claim(ctx, testAccount, jmap.Id(id2)); !ok {
		t.Fatal("A's takeover of id2 failed")
	}
	wB.finalize(ctx, testAccount, jmap.Id(id2), cB2.stamp,
		[]RecipientResult{{Recipient: "jane@remote.example", Outcome: Accepted, Reply: "250 late"}})
	if s := wB.Stats(); s.FinalizesSuperseded != 1 || s.FutureStamps != 2 {
		t.Fatalf("after superseded finalize: %+v", s)
	}

	// The fast peer took over two leftover stamps, both in ITS past:
	// reclaims tick, future stamps never do.
	if s := wA.Stats(); s.ClaimsReclaimed != 2 || s.FutureStamps != 0 ||
		s.ClaimsLostBeforeTransmit != 0 || s.FinalizesSuperseded != 0 {
		t.Fatalf("fast peer's counters: %+v", s)
	}
}

// TestWorkerStatsZeroProgress: a backward clock step lands between the
// sweep's due scan and the claim's re-check, so the pass goes after due
// work and wins none of it - the zero-progress shape (in the field the
// same window is a peer claiming first).
func TestWorkerStatsZeroProgress(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	clock.advance(5 * time.Second)

	// The first two now() readings (sweep's due gate, processAccount's
	// scan bound) see the record due; the claim's reading has stepped
	// 10s back, so its due re-check refuses.
	base := clock.now()
	var mu sync.Mutex
	calls := 0
	w.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 {
			return base
		}
		return base.Add(-10 * time.Second)
	}
	sent, _, _, err := w.sweep(context.Background(), 0)
	if err != nil || sent != 0 || fake.callCount() != 0 {
		t.Fatalf("stepped sweep sent %d (submitter %d), err %v", sent, fake.callCount(), err)
	}
	s := w.Stats()
	if s.Sweeps != 1 || s.ZeroProgressSweeps != 1 || s.ClaimsTaken != 0 {
		t.Fatalf("zero-progress accounting: %+v", s)
	}
}

// TestClaimStampTime: the token-prefix parse behind the future-stamp
// signal - honest time for real tokens, a clean miss for garbage.
func TestClaimStampTime(t *testing.T) {
	want := time.Date(2026, 7, 18, 12, 0, 0, 123456789, time.UTC)
	ts, ok := claimStampTime(want.Format(time.RFC3339Nano) + "|abcd-1")
	if !ok || !ts.Equal(want) {
		t.Fatalf("real token: %v %v", ts, ok)
	}
	for _, bad := range []string{"", "no-separator", "not-a-time|nonce", "2026-07-18T12:00:00Z"} {
		if _, ok := claimStampTime(bad); ok {
			t.Fatalf("parsed garbage token %q", bad)
		}
	}
}
