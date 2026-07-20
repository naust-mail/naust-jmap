package mail

// Multi-worker coordination harness: several SubmissionWorkers over ONE
// shared store, exercised under real goroutine concurrency with crash
// injection, validating the unified claim model (step 1) and the claim
// token (step 2) - the guarantees the single-worker tests cannot reach
// because they drive one worker at a time. Two properties are asserted:
// no concurrent double-claim, and duplicate transmits bounded strictly by
// injected crashes. Per-worker clocks are wired through w.now, so the
// suspension/skew scenarios (step 4) extend this same harness.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// crashSubmitter is the harness's smarthost. It records every transmit
// and, on a seeded decision, panics AFTER counting the delivery -
// modeling a worker that dies once the smarthost has accepted the bytes
// but before it could finalize, the at-least-once window that forces a
// reclaim to re-transmit. Its counters make the bound exact: a successful
// (non-crashing) Submit always finalizes its record, so total transmits
// equal one per record plus one wasted transmit per crash.
type crashSubmitter struct {
	mu          sync.Mutex
	transmits   int
	crashes     int
	crashDecide func() bool // called under mu; nil never crashes
}

func (s *crashSubmitter) Submit(_ context.Context, env SubmissionEnvelope, msg io.Reader) ([]RecipientResult, error) {
	io.Copy(io.Discard, msg) // drain the streamed message
	s.mu.Lock()
	s.transmits++
	crash := s.crashDecide != nil && s.crashDecide()
	if crash {
		s.crashes++
	}
	s.mu.Unlock()
	if crash {
		panic("crashSubmitter: simulated worker death after smarthost accept")
	}
	out := make([]RecipientResult, len(env.Recipients))
	for i, r := range env.Recipients {
		out[i] = RecipientResult{Recipient: r.Email, Outcome: Accepted, Reply: "250 2.0.0 accepted"}
	}
	return out, nil
}

func (s *crashSubmitter) counts() (transmits, crashes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transmits, s.crashes
}

// blockingSubmitter holds a single transmit open: it signals entry, then
// blocks until released, so a test can freeze one worker mid-send while a
// peer reclaims the record. reply is the accept reply it returns once
// released.
type blockingSubmitter struct {
	entered chan struct{}
	release chan struct{}
	reply   string
	mu      sync.Mutex
	calls   int
}

func (b *blockingSubmitter) Submit(ctx context.Context, env SubmissionEnvelope, msg io.Reader) ([]RecipientResult, error) {
	io.Copy(io.Discard, msg)
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]RecipientResult, len(env.Recipients))
	for i, r := range env.Recipients {
		out[i] = RecipientResult{Recipient: r.Email, Outcome: Accepted, Reply: b.reply}
	}
	return out, nil
}

func (b *blockingSubmitter) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// subSetHas reports whether id appears under the given EmailSubmission/set
// result key ("updated" or "notUpdated").
func subSetHas(t *testing.T, r *jmap.Response, key, id string) bool {
	t.Helper()
	m, _ := methodArgs(t, r, 0, "EmailSubmission/set")[key].(map[string]any)
	_, ok := m[id]
	return ok
}

// TestWorkerCancelAfterClaimRejected: once the worker has claimed a record
// (a transmit may be imminent), a user cancel is refused as cannotUnsend
// and the send proceeds. The claim's presence is the unsend cutoff.
func TestWorkerCancelAfterClaimRejected(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	cA, ok := w.claim(ctx, testAccount, jmap.Id(id))
	if !ok {
		t.Fatal("claim failed")
	}
	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"undoStatus":"canceled"}}}`, testAccount, id), "0"))
	if subSetHas(t, r, "updated", id) {
		t.Fatal("cancel of a claimed (in-flight) submission was accepted")
	}
	if !subSetHas(t, r, "notUpdated", id) {
		t.Fatalf("claimed-submission cancel not reported notUpdated: %v", r.MethodResponses[0].Args)
	}
	w.sendOne(ctx, testAccount, cA)
	if fake.callCount() != 1 {
		t.Fatalf("send did not proceed after the rejected cancel: %d transmits", fake.callCount())
	}
	if recString(subRecord(t, db, id), "undoStatus") != undoFinal {
		t.Error("record not final after send")
	}
}

// TestWorkerCancelBeforeClaimSkipped: a cancel that lands before the
// worker claims takes the record out of the queue, and the worker's claim
// then finds nothing to do (no nextAttemptAt) - the canceled message is
// never sent.
func TestWorkerCancelBeforeClaimSkipped(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"undoStatus":"canceled"}}}`, testAccount, id), "0"))
	if !subSetHas(t, r, "updated", id) {
		t.Fatalf("cancel of an unclaimed submission failed: %v", r.MethodResponses[0].Args)
	}
	if _, ok := w.claim(ctx, testAccount, jmap.Id(id)); ok {
		t.Fatal("worker claimed a canceled submission")
	}
	w.sweep(ctx, 0)
	if fake.callCount() != 0 {
		t.Fatalf("canceled submission was sent: %d transmits", fake.callCount())
	}
	if recString(subRecord(t, db, id), "undoStatus") != undoCanceled {
		t.Errorf("undoStatus = %s, want canceled", recString(subRecord(t, db, id), "undoStatus"))
	}
}

// TestWorkerDestroyDuringSendTolerated: a record destroyed while the
// worker holds a claim simply vanishes - finalize on the missing record is
// a no-op (no panic, no resurrection). Destroy MUST NOT recall a message
// already handed to transmit (section 7.5); this only proves the worker
// tolerates the record disappearing under it.
func TestWorkerDestroyDuringSendTolerated(t *testing.T) {
	ts, db, store, w, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	cA, ok := w.claim(ctx, testAccount, jmap.Id(id))
	if !ok {
		t.Fatal("claim failed")
	}
	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, id), "0"))
	destroyed, _ := methodArgs(t, r, 0, "EmailSubmission/set")["destroyed"].([]any)
	if len(destroyed) != 1 {
		t.Fatalf("destroy of a claimed submission not accepted: %v", r.MethodResponses[0].Args)
	}
	// finalize on the vanished record must be a silent no-op.
	w.finalize(ctx, testAccount, jmap.Id(id), cA.stamp,
		[]RecipientResult{{Recipient: "jane@remote.example", Outcome: Accepted, Reply: "250 gone"}})
	if _, err := db.Get(ctx, testAccount, TypeEmailSubmission, jmap.Id(id)); err == nil {
		t.Fatal("finalize resurrected a destroyed submission")
	}
}

// TestWorkerConcurrentFinalizeSupersededDropped races two finalize legs on
// one record under -race: the current claim token and a superseded one.
// Whatever the lease ordering, only the current token's result is applied
// - never a mix, never a double count.
func TestWorkerConcurrentFinalizeSupersededDropped(t *testing.T) {
	ts, db, store, w, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	cA, ok := w.claim(ctx, testAccount, jmap.Id(id))
	if !ok {
		t.Fatal("claim failed")
	}
	staleToken := "2020-01-01T00:00:00.000000000Z|00000000-1"

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.finalize(ctx, testAccount, jmap.Id(id), cA.stamp,
			[]RecipientResult{{Recipient: "jane@remote.example", Outcome: Accepted, Reply: "250 current"}})
	}()
	go func() {
		defer wg.Done()
		w.finalize(ctx, testAccount, jmap.Id(id), staleToken,
			[]RecipientResult{{Recipient: "jane@remote.example", Outcome: Rejected, Reply: "550 stale"}})
	}()
	wg.Wait()

	rec := subRecord(t, db, id)
	got := recDeliveryStatus(t, rec)["jane@remote.example"]
	if got.Delivered != "unknown" || got.SmtpReply != "250 current" {
		t.Fatalf("superseded finalize won or mixed: %+v", got)
	}
	var attempts uint64
	json.Unmarshal(rec["attempts"], &attempts)
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (only the current token finalized)", attempts)
	}
}

// TestWorkerSuspendedClaimAbandonedByReread is the EVIDENCE that the
// existing pre-transmit re-read (kept from step 1) already is the
// suspension defense, clock-independently - no monotonic check added.
// Worker A claims, then is suspended (never sends). The clock skews past
// ClaimWindow; worker B reclaims and delivers. A resumes and runs its
// send: sendOne's lease-held re-read sees the claim is no longer A's and
// abandons BEFORE transmitting. A's submitter is never called.
func TestWorkerSuspendedClaimAbandonedByReread(t *testing.T) {
	cfg := SubmissionWorkerConfig{}
	ts, db, store, _, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), cfg)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	aSub := &fakeSubmitter{} // records a transmit if A ever sends
	wA, err := NewSubmissionWorker(newSubmissionQueue(db, store), aSub, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wA.now = clock.now
	bSub := &fakeSubmitter{}
	wB, err := NewSubmissionWorker(newSubmissionQueue(db, store), bSub, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wB.now = clock.now

	// A claims (token A, parked), then "suspends" - it does not transmit.
	cA, ok := wA.claim(ctx, testAccount, jmap.Id(id))
	if !ok {
		t.Fatal("A's claim failed")
	}

	// Time passes beyond ClaimWindow; B reclaims and delivers the record.
	clock.advance(16 * time.Minute)
	sentB, _, err := wB.ProcessDue(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sentB != 1 || bSub.callCount() != 1 {
		t.Fatalf("B reclaimed/sent %d (submitter %d), want 1", sentB, bSub.callCount())
	}

	// A resumes and runs its send. The re-read must abandon before transmit.
	wA.sendOne(ctx, testAccount, cA)
	if aSub.callCount() != 0 {
		t.Fatalf("suspended worker A transmitted %d times after its claim was reclaimed", aSub.callCount())
	}
	if recString(subRecord(t, db, id), "undoStatus") != undoFinal {
		t.Error("record not left final by B")
	}
}

// TestWorkerLateClaimUncontestedStillSends is the EVIDENCE for the other
// half: a worker that took longer than ClaimWindow between claim and
// transmit, but whose claim NO peer took, still sends and finalizes -
// transmitting late is correct when uncontested (a later reclaimer finds
// the record already done). This is exactly the send a 3C elapsed-abandon
// check would wrongly suppress, forcing an unnecessary re-send.
func TestWorkerLateClaimUncontestedStillSends(t *testing.T) {
	cfg := SubmissionWorkerConfig{}
	ts, db, store, _, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), cfg)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	aSub := &fakeSubmitter{}
	wA, err := NewSubmissionWorker(newSubmissionQueue(db, store), aSub, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wA.now = clock.now

	cA, ok := wA.claim(ctx, testAccount, jmap.Id(id))
	if !ok {
		t.Fatal("A's claim failed")
	}
	clock.advance(16 * time.Minute) // A is now "late" (past ClaimWindow) but nobody reclaimed
	wA.sendOne(ctx, testAccount, cA)
	if aSub.callCount() != 1 {
		t.Fatalf("late uncontested worker transmitted %d times, want 1", aSub.callCount())
	}
	if recString(subRecord(t, db, id), "undoStatus") != undoFinal {
		t.Error("late uncontested send did not finalize")
	}
}

// TestWorkerReclaimDuringLiveTransmit drives the multi-box race the
// round-structured stress test structurally cannot reach: a reclaim that
// runs CONCURRENTLY with the original holder's still-in-flight transmit.
// Worker A claims a record and enters transmit (held open); the clock
// skews past ClaimWindow (A suspended, or its wall clock jumped); worker B
// reclaims the now-due record over the same store and delivers it; then
// A's held transmit completes. The claim token must reject A's stale
// finalize - so B's delivery stands, and A's transmit is one bounded
// duplicate, never a double-finalize. (The duplicate itself is the
// at-least-once window the step-4 suspension check is meant to shrink.)
func TestWorkerReclaimDuringLiveTransmit(t *testing.T) {
	cfg := SubmissionWorkerConfig{}
	ts, db, store, _, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), cfg)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second)

	aSub := &blockingSubmitter{entered: make(chan struct{}), release: make(chan struct{}), reply: "250 2.0.0 A"}
	bSub := &fakeSubmitter{respond: func(env SubmissionEnvelope) ([]RecipientResult, error) {
		out := make([]RecipientResult, len(env.Recipients))
		for i, r := range env.Recipients {
			out[i] = RecipientResult{Recipient: r.Email, Outcome: Accepted, Reply: "250 2.0.0 B"}
		}
		return out, nil
	}}
	wA, err := NewSubmissionWorker(newSubmissionQueue(db, store), aSub, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wA.now = clock.now
	wB, err := NewSubmissionWorker(newSubmissionQueue(db, store), bSub, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wB.now = clock.now

	// A claims and blocks inside transmit.
	aDone := make(chan struct{})
	go func() { defer close(aDone); wA.ProcessDue(ctx, 1) }()
	select {
	case <-aSub.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker A never entered transmit")
	}

	// A's claim is parked ClaimWindow ahead; skew past it and let B reclaim.
	clock.advance(16 * time.Minute)
	sentB, _, err := wB.ProcessDue(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sentB != 1 {
		t.Fatalf("B reclaimed and sent %d, want 1", sentB)
	}
	if got := recDeliveryStatus(t, subRecord(t, db, id))["jane@remote.example"]; got.Delivered != "unknown" || got.SmtpReply != "250 2.0.0 B" {
		t.Fatalf("B's delivery not recorded: %+v", got)
	}

	// Release A: its held transmit completes and it tries to finalize with
	// its now-superseded token.
	close(aSub.release)
	select {
	case <-aDone:
	case <-time.After(5 * time.Second):
		t.Fatal("worker A did not finish after release")
	}

	// The token rejected A's finalize: B's result stands, unregressed.
	rec := subRecord(t, db, id)
	if got := recDeliveryStatus(t, rec)["jane@remote.example"]; got.Delivered != "unknown" || got.SmtpReply != "250 2.0.0 B" {
		t.Fatalf("A's stale finalize corrupted B's result: %+v", got)
	}
	if recString(rec, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s, want final", recString(rec, "undoStatus"))
	}
	var attempts uint64
	json.Unmarshal(rec["attempts"], &attempts)
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (A's finalize dropped)", attempts)
	}
	// Bounded duplicate: A transmitted exactly once (the wasted send), B once.
	if aSub.callCount() != 1 {
		t.Errorf("A transmits = %d, want 1", aSub.callCount())
	}
	if bSub.callCount() != 1 {
		t.Errorf("B transmits = %d, want 1", bSub.callCount())
	}
}

// TestWorkerConcurrentClaimMutualExclusion is the tight unit proof of the
// step-1 claim model under a real race: two workers over one store claim
// the SAME due record at the same instant, and exactly one wins - the
// lease serializes the claim writes and the loser sees the record already
// parked out of the due set. Run many times, under -race, to shake the
// interleaving.
func TestWorkerConcurrentClaimMutualExclusion(t *testing.T) {
	ts, db, store, _, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	ctx := context.Background()

	mkWorker := func() *SubmissionWorker {
		w, err := NewSubmissionWorker(newSubmissionQueue(db, store), &fakeSubmitter{}, SubmissionWorkerConfig{})
		if err != nil {
			t.Fatal(err)
		}
		w.now = clock.now
		return w
	}

	for iter := 0; iter < 40; iter++ {
		emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
		id := submitEnvelope(t, ts, identityId, emailId,
			`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
		clock.advance(time.Second) // clear the record's creation-second boundary

		wA, wB := mkWorker(), mkWorker()
		var cA, cB claimedRecord
		var okA, okB bool
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; cA, okA = wA.claim(ctx, testAccount, jmap.Id(id)) }()
		go func() { defer wg.Done(); <-start; cB, okB = wB.claim(ctx, testAccount, jmap.Id(id)) }()
		close(start)
		wg.Wait()

		if okA == okB {
			t.Fatalf("iter %d: both claims returned ok=%v - mutual exclusion broken", iter, okA)
		}
		winner := cA.stamp
		if okB {
			winner = cB.stamp
		}
		if got := recString(subRecord(t, db, id), "claimedAt"); got != winner {
			t.Fatalf("iter %d: stored claim %q != winner's token %q", iter, got, winner)
		}
	}
}

// TestWorkerMultiWorkerStress runs K workers over one store across many
// rounds, each worker a fresh process (new nonce) that may die mid-send.
// Within a round the workers run concurrently at a frozen clock; between
// rounds the clock jumps past ClaimWindow so a crashed record becomes due
// and is reclaimed. It asserts:
//
//   - liveness: every submission eventually finalizes;
//   - no partial-finalize leak: each ends terminal with attempts == 1
//     (a crash never finalizes, so only the one successful attempt counts);
//   - the exact bound transmits == N + crashes: each recipient succeeds
//     exactly once and every crash wastes exactly one transmit, so any
//     concurrent double-claim (two live claims on one record) would push
//     the count above the bound.
func TestWorkerMultiWorkerStress(t *testing.T) {
	const (
		seed      = 20260718
		N         = 15
		K         = 3
		maxRounds = 200
		crashProb = 0.35
	)
	rng := rand.New(rand.NewSource(seed))
	cfg := SubmissionWorkerConfig{GiveUpAfter: 1000 * time.Hour} // give-up must never interfere
	ts, db, store, _, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), cfg)
	ctx := context.Background()
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	sub := &crashSubmitter{crashDecide: func() bool { return rng.Float64() < crashProb }}

	var ids []string
	for n := 0; n < N; n++ {
		emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
		id := submitEnvelope(t, ts, identityId, emailId, fmt.Sprintf(
			`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"r%d@remote.example"}]}`, n))
		ids = append(ids, id)
	}
	clock.advance(time.Second) // clear the creation-second boundary

	finalCount := func() int {
		n := 0
		for _, id := range ids {
			if recString(subRecord(t, db, id), "undoStatus") == undoFinal {
				n++
			}
		}
		return n
	}

	rounds := 0
	for ; rounds < maxRounds && finalCount() < N; rounds++ {
		var wg sync.WaitGroup
		for k := 0; k < K; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = recover() }() // a crashed worker dies mid-pass
				w, err := NewSubmissionWorker(newSubmissionQueue(db, store), sub, cfg)
				if err != nil {
					return
				}
				w.now = clock.now
				w.ProcessDue(ctx, 0)
			}()
		}
		wg.Wait()
		clock.advance(16 * time.Minute) // past ClaimWindow: crashed records become due again
	}

	if n := finalCount(); n < N {
		t.Fatalf("after %d rounds, only %d/%d submissions finalized", rounds, n, N)
	}
	for _, id := range ids {
		rec := subRecord(t, db, id)
		var attempts uint64
		json.Unmarshal(rec["attempts"], &attempts)
		if attempts != 1 {
			t.Errorf("submission %s: attempts = %d, want exactly 1", id, attempts)
		}
		for rcpt, st := range recDeliveryStatus(t, rec) {
			if st.Delivered != "unknown" {
				t.Errorf("submission %s recipient %s: delivered = %q, want unknown", id, rcpt, st.Delivered)
			}
		}
		if _, has := rec["nextAttemptAt"]; has {
			t.Errorf("submission %s kept nextAttemptAt after completion", id)
		}
		if _, has := rec["claimedAt"]; has {
			t.Errorf("submission %s kept claimedAt after completion", id)
		}
	}
	tx, crashes := sub.counts()
	if tx != N+crashes {
		t.Fatalf("transmits = %d, want N+crashes = %d+%d = %d - excess implies a concurrent double-claim",
			tx, N, crashes, N+crashes)
	}
	t.Logf("stress: %d rounds, %d transmits (%d recipients + %d crash retries)", rounds, tx, N, crashes)
}
