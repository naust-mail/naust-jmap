package mail

// The EmailSubmission sending worker. The submission records ARE the
// queue: the internal nextAttemptAt index holds exactly the pending
// work in due order, so the worker's whole job is claim due records,
// transmit outside the account lease, and finalize what the smarthost
// said (RFC 8621 section 7).
//
// Coordination model: the DATABASE is the coordination point, and the
// wake path layers on top of it in three tiers. Durable truth: the
// records and the queue's account tag hold exactly the pending work,
// so a sweep over the tag reconstructs everything from the store alone
// - the worker retains NOTHING between wakes (no cache, no per-account
// state; an idle worker holds a timer, an empty bell, and a blocked
// goroutine, and that is all). Same-process latency: a commit that
// queues work rings the SubmissionQueue bell directly - lossless, so
// same-process mail leaves immediately. Cross-process latency: the
// worker subscribes firehose to the db's Notifier (when one is
// attached), so a commit by another process sharing the store wakes it
// as fast as the notifier delivers. The Notifier is best-effort by
// contract, so correctness never rests on it: every sleep is capped at
// QueueScanInterval and every wake runs the same sweep, making the
// sweep the reconciliation floor - a dropped notification delays work
// by at most one interval, never loses it.
//
// Crash safety uses the due index AS the claim. Claiming a record
// stamps claimedAt (a wall-clock stamp that doubles as the claim's
// identity) and, in the SAME lease-held write, parks nextAttemptAt at
// claimedAt+ClaimWindow: a claimed record is no longer "due", so no
// second worker's due scan can pick it up and the two coordination
// notions (the due index and the claim stamp) can never disagree.
// Finalize runs under the account lease and verifies the stamp is still
// the one this worker wrote, so a double-finalize is unreachable no
// matter what clocks do. A worker that crashes mid-transmit leaves its
// record parked; it reappears as due exactly ClaimWindow later and is
// reclaimed - reclaim timing falls out of the index, not a separate
// freshness comparison.
//
// Delivery is at-least-once, and no window value changes that: a worker
// that crashes after the smarthost accepted but before finalize leaves
// no trace of the acceptance, so the reclaim relays that mail again.
// SMTP has no transactional handoff to close this; deduplication
// downstream rides on the Message-ID. What the coordination model DOES
// bound is duplicates from racing workers, and that bound assumes
// roughly synchronized clocks (working NTP): a peer running ahead by
// more than ClaimWindow - TransmitTimeout can reclaim a record whose
// original transmit is still live, and while any worker's skew exceeds
// that budget the reclaims - and their duplicate relays - are
// SYSTEMATIC, not one-off. With the defaults that budget is 13 minutes
// against NTP's typical milliseconds; a machine skewed past it has
// problems this library cannot compensate for.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// SubmissionWorkerConfig tunes the sending worker. The zero value of
// every field selects the documented default.
type SubmissionWorkerConfig struct {
	// RetrySchedule is the backoff after each temporarily failed
	// attempt: attempt n reschedules RetrySchedule[n-1] later, and
	// attempts beyond the schedule use RetryPlateau. Defaults: 1m, 5m,
	// 15m, 1h, 4h, then every 8h.
	RetrySchedule []time.Duration
	RetryPlateau  time.Duration
	// GiveUpAfter bounds retrying: when a still-undelivered recipient's
	// submission is older than this (measured from sendAt, the RFC 8621
	// release time - a FUTURERELEASE hold does not consume retry time),
	// delivery is abandoned with a synthetic permanent failure - but
	// never before MinAttempts real attempts, so a long worker outage
	// gives stale mail a genuine burst of tries instead of an instant
	// bounce. Defaults 48h, 3 attempts.
	GiveUpAfter time.Duration
	MinAttempts int
	// ClaimWindow is how long a claim holds a record: on claim the due
	// index is parked ClaimWindow ahead, so a crashed worker's record
	// reappears as due - and is reclaimed - exactly one window later. It
	// MUST comfortably exceed TransmitTimeout (the constructor enforces
	// ClaimWindow >= 3*TransmitTimeout) so a live transmit never outlives
	// its claim. ClaimWindow - TransmitTimeout is also the cross-machine
	// clock-skew budget: workers whose clocks disagree by more than that
	// can reclaim each other's live claims (see the coordination model
	// above), so it must dwarf worst-case skew - trivial under working
	// NTP. It is also the recovery latency for a crashed worker's mail,
	// so bigger is not free. Default 15m.
	ClaimWindow time.Duration
	// TransmitTimeout bounds one transmission attempt end to end.
	// Default 2m.
	TransmitTimeout time.Duration
	// BatchSize caps how many records one account may claim per pass,
	// which is also the fairness unit: accounts take turns in batches,
	// so one flooded account cannot starve the rest. Default 16.
	BatchSize int
	// QueueScanInterval is the reconciliation cadence: the worst-case
	// delay (within scanJitter) before this worker picks up work whose
	// wake signals it never received - committed by another process with
	// no Notifier attached, or a dropped notification (see the
	// coordination model above). Each sweep reads the queue tag's
	// members and probes them, so its cost tracks accounts with actual
	// queued work, near zero when idle. Default 1m.
	QueueScanInterval time.Duration
}

func (c *SubmissionWorkerConfig) applyDefaults() {
	// An explicit empty (non-nil) slice is a config foot-gun - it would put
	// every retry straight onto the plateau - so treat empty like unset and
	// use the default schedule; a caller wanting plateau-only sets a
	// single-entry schedule.
	if len(c.RetrySchedule) == 0 {
		c.RetrySchedule = []time.Duration{
			time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour,
		}
	}
	if c.RetryPlateau <= 0 {
		c.RetryPlateau = 8 * time.Hour
	}
	if c.GiveUpAfter <= 0 {
		c.GiveUpAfter = 48 * time.Hour
	}
	if c.MinAttempts <= 0 {
		c.MinAttempts = 3
	}
	if c.ClaimWindow <= 0 {
		c.ClaimWindow = 15 * time.Minute
	}
	if c.TransmitTimeout <= 0 {
		c.TransmitTimeout = 2 * time.Minute
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 16
	}
	if c.QueueScanInterval <= 0 {
		c.QueueScanInterval = time.Minute
	}
}

// SubmissionWorker sends queued EmailSubmissions through a Submitter.
// It consumes the SubmissionQueue returned by RegisterEmailSubmission;
// start it with `go w.Run(ctx)` - the worker never starts goroutines on
// its own - or drive it by hand with ProcessDue, which turns the same
// engine once. Records queue up durably whether or not a worker is running,
// and each process sharing a store runs its own worker (double-claims
// across workers are prevented by the claim tokens).
// Jitter constants: herding is broken by small random spreads, never by
// coordination. All three multiply a base duration by a factor of the
// worker's rand seam, so a test pinning rand to 0.5 collapses every
// formula to exactly its base value.
const (
	// backoffJitter spreads each retry delay by +-20%: temp-failures
	// land in bursts (a smarthost outage defers everything in one
	// sweep), and exact rescheduling would carry the burst forward as a
	// synchronized retry wave forever.
	backoffJitter = 0.2
	// scanJitter spreads the sleep cap by +-10% so workers deployed
	// together do not tick their reconciliation sweeps in phase.
	scanJitter = 0.1
	// pumpDelayMax bounds the random yield before a Notifier-driven
	// ring: the committing process's worker was already rung directly
	// (lossless bell) and claims first; remote workers arriving a beat
	// later find the records parked instead of racing for them.
	pumpDelayMax = 500 * time.Millisecond
)

type SubmissionWorker struct {
	q      *SubmissionQueue
	submit Submitter
	cfg    SubmissionWorkerConfig
	now    func() time.Time // test seam; time.Now outside tests
	rand   func() float64   // test seam; jitter source in [0,1)
	nonce  string           // per-worker random claim-token base
	seq    atomic.Uint64    // per-claim counter appended to nonce
	stats  workerStats      // observability counters, read via Stats
}

// NewSubmissionWorker builds a worker over the given queue and
// Submitter. It returns an error rather than a worker when the
// configuration violates the claim-freshness invariant
// ClaimWindow >= 3*TransmitTimeout: a window that a single transmit
// could plausibly outlive would make live claims look abandoned. Each
// worker draws a random nonce base so its claim tokens never collide
// with another worker's, whatever the two machines' clocks read.
func NewSubmissionWorker(q *SubmissionQueue, submitter Submitter, cfg SubmissionWorkerConfig) (*SubmissionWorker, error) {
	if q == nil || submitter == nil {
		return nil, errors.New("mail: SubmissionWorker needs a SubmissionQueue and a Submitter")
	}
	cfg.applyDefaults()
	if cfg.ClaimWindow < 3*cfg.TransmitTimeout {
		return nil, fmt.Errorf("mail: ClaimWindow %v must be at least 3x TransmitTimeout %v", cfg.ClaimWindow, cfg.TransmitTimeout)
	}
	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return nil, fmt.Errorf("mail: SubmissionWorker nonce: %w", err)
	}
	return &SubmissionWorker{
		q:      q,
		submit: submitter,
		cfg:    cfg,
		now:    time.Now,
		rand:   mrand.Float64,
		nonce:  hex.EncodeToString(nb[:]),
	}, nil
}

// Run processes the queue until ctx ends, returning ctx's error. It
// heals the queue tag from the full account registry once at startup
// (resuming whatever any previous process left queued), subscribes to
// the db's Notifier when one is attached, then loops: sweep, sleep
// until the earliest due time capped at QueueScanInterval, wake on the
// bell or the timer, sweep again. Transient store or transmit errors
// never stop the loop - an interrupted record's claim goes stale and
// is reclaimed a window later.
func (w *SubmissionWorker) Run(ctx context.Context) error {
	if err := w.healTags(ctx); err != nil {
		return err
	}
	if n := w.q.db.Notifier(); n != nil {
		sub, err := n.SubscribeAll(ctx)
		if err == nil {
			defer sub.Close()
			go w.pump(ctx, sub)
		}
		// A failed subscribe is not fatal: the sweep floor still covers
		// cross-process discovery at QueueScanInterval latency.
	}
	for {
		_, nextDue, pending, _ := w.sweep(ctx, 0)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Sleep on a DURATION computed against a single reading of now, not
		// on an absolute wake time. A stored due time (nextAttemptAt) carries
		// no monotonic reading, so only a duration handed to the monotonic
		// timer is immune to a wall-clock step landing mid-sleep; recomputing
		// every pass bounds any step's effect to one interval, and the
		// (scanJitter-spread) QueueScanInterval cap bounds it again.
		d := time.Duration(float64(w.cfg.QueueScanInterval) * (1 + scanJitter*(2*w.rand()-1)))
		if pending {
			if dd := nextDue.Sub(w.now()); dd < d {
				d = dd
			}
		}
		if d <= 0 {
			// A past due time surviving a sweep is abnormal - a claim race
			// or a store error mid-pass. The floor turns a hot spin against
			// a failing store into a paced retry; the bell still bypasses
			// it for genuinely new work.
			d = time.Second
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-w.q.bell:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// pump forwards Notifier deliveries into the bell: any commit whose
// changed types include EmailSubmission rings - including this worker's
// own finalizes, which cost one coalesced redundant sweep per burst.
// Each ring waits a random beat (up to pumpDelayMax) first: the
// committing process's worker was rung directly and losslessly, so
// yielding lets it claim first and turns the cross-worker claim race
// into a parked-record miss; changes arriving during the yield coalesce
// in the subscription. It exits when the subscription ends (ctx cancel
// or Close).
func (w *SubmissionWorker) pump(ctx context.Context, sub notify.Subscription) {
	for {
		changes, err := sub.Wait(ctx)
		if err != nil {
			return
		}
		touched := false
		for _, ts := range changes {
			if _, ok := ts[TypeEmailSubmission]; ok {
				touched = true
				break
			}
		}
		if !touched {
			continue
		}
		timer := time.NewTimer(time.Duration(w.rand() * float64(pumpDelayMax)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		w.q.ring()
	}
}

// ProcessDue drains due work immediately: sweeps (discovery over the
// queue tag, then claim/transmit/finalize in due order) until nothing
// more is due or limit submissions have been processed (limit <= 0
// means no limit). It is the manual crank on the same engine Run turns
// - an operator's queue flush, a pacer's metered drain, a test's
// deterministic step - and is safe to call whether or not a Run loop is
// live, here or in another process: the claim stamps that serialize
// workers serialize manual drains the same way. sent counts the
// submission records this call processed an attempt for; next is the
// earliest remaining due time (zero when nothing is queued).
func (w *SubmissionWorker) ProcessDue(ctx context.Context, limit int) (sent int, next time.Time, err error) {
	for {
		remaining := 0
		if limit > 0 {
			remaining = limit - sent
			if remaining <= 0 {
				break
			}
		}
		n, nextDue, pending, serr := w.sweep(ctx, remaining)
		sent += n
		if serr != nil {
			return sent, time.Time{}, serr
		}
		next = time.Time{}
		if pending {
			next = nextDue
		}
		if n == 0 || ctx.Err() != nil {
			break
		}
	}
	return sent, next, ctx.Err()
}

// healTags is the thorough startup pass over the full account registry:
// any account holding queued work without carrying the queue tag
// (possible after a partial restore) is tagged, so the sweep's
// tag-driven discovery sees everything durable state holds. Setting a
// tag needs no verification - supersets are the tag contract.
func (w *SubmissionWorker) healTags(ctx context.Context) error {
	tagged := make(map[jmap.Id]bool)
	tlist, err := w.q.db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil {
		return err
	}
	for _, acct := range tlist {
		tagged[acct] = true
	}
	accts, err := w.q.db.Accounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		if tagged[acct] {
			continue
		}
		_, found, err := w.q.probe(ctx, acct)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		w.q.db.Update(ctx, acct, func(u *objectdb.Update) error {
			return u.SetAccountTag(submissionQueueTag)
		})
	}
	return nil
}

// sweep is the single engine behind Run and ProcessDue: one pass of
// discovery plus processing, reconstructed from durable state alone.
// It reads the queue tag's members, probes each, processes what is due
// (claim, transmit, finalize, in due order across accounts), clears
// the tag of drained accounts, and reports the earliest future due
// time it saw - computed as a byproduct of the walk it already does,
// so the worker retains no queue state between sweeps. limit > 0 caps
// how many records this pass may process, limit <= 0 is unbounded. err
// reports only a failure to read the tag list; per-account probe
// errors skip that account (it stays tagged, and the next sweep - at
// most QueueScanInterval later - retries it).
func (w *SubmissionWorker) sweep(ctx context.Context, limit int) (sent int, nextDue time.Time, pending bool, err error) {
	w.stats.sweeps.Add(1)
	accts, err := w.q.db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil {
		return 0, time.Time{}, false, err
	}
	note := func(due time.Time) {
		if !pending || due.Before(nextDue) {
			nextDue, pending = due, true
		}
	}
	type acctDue struct {
		acct jmap.Id
		due  time.Time
	}
	todo := make([]acctDue, 0, len(accts))
	for _, acct := range accts {
		if ctx.Err() != nil {
			return sent, nextDue, pending, ctx.Err()
		}
		due, found, perr := w.q.probe(ctx, acct)
		if perr != nil {
			continue
		}
		if !found {
			w.clearDrainedTag(ctx, acct)
			continue
		}
		todo = append(todo, acctDue{acct, due})
	}
	sort.Slice(todo, func(i, j int) bool {
		if !todo[i].due.Equal(todo[j].due) {
			return todo[i].due.Before(todo[j].due)
		}
		return todo[i].acct < todo[j].acct
	})
	now := w.now()
	attempted := false
	for _, t := range todo {
		if ctx.Err() != nil {
			return sent, nextDue, pending, ctx.Err()
		}
		if t.due.After(now) {
			note(t.due)
			continue
		}
		remaining := 0
		if limit > 0 {
			remaining = limit - sent
			if remaining <= 0 {
				note(t.due)
				continue
			}
		}
		attempted = true
		sent += w.processAccount(ctx, t.acct, remaining)
		due, found, perr := w.q.probe(ctx, t.acct)
		if perr != nil {
			continue
		}
		if !found {
			w.clearDrainedTag(ctx, t.acct)
			continue
		}
		note(due)
	}
	// A pass that went after due work and claimed none of it: healthy
	// claim racing when occasional, a stuck queue when constant. Judged
	// only on completed passes - a ctx abort above is neither.
	if attempted && sent == 0 {
		w.stats.zeroProgressSweeps.Add(1)
	}
	return sent, nextDue, pending, nil
}

// clearDrainedTag removes the queue tag from an account confirmed
// empty. The probe re-runs INSIDE the account lease: tags are set only
// by commits, which are lease-serialized, so a lease-held probe seeing
// no queued work proves no set can be racing the clear.
func (w *SubmissionWorker) clearDrainedTag(ctx context.Context, acct jmap.Id) {
	w.q.db.Update(ctx, acct, func(u *objectdb.Update) error {
		ids, err := w.q.db.IdsWhereAtMost(u.Context(), acct, TypeEmailSubmission, "nextAttemptAt", nil, 1)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			return nil // requeued since the outside probe; keep the tag
		}
		return u.ClearAccountTag(submissionQueueTag)
	})
}

// claimedRecord is one record this pass claimed, with the stamp that
// identifies the claim.
type claimedRecord struct {
	id    jmap.Id
	stamp string
}

// processAccount works up to BatchSize due records (fewer when the
// caller's limit is tighter), claiming, transmitting, and finalizing
// each one independently before the next - so every claim is taken
// immediately before its own transmit (always fresh) and a hot account
// spreads across workers instead of one worker holding the whole batch.
// BatchSize is the per-pass fairness cap. Returns how many it processed.
func (w *SubmissionWorker) processAccount(ctx context.Context, acct jmap.Id, limit int) (sent int) {
	now := w.now()
	quota := w.cfg.BatchSize
	if limit > 0 && limit < quota {
		quota = limit
	}
	maxRaw := mustJSON(now.UTC().Format(time.RFC3339))
	ids, err := w.q.db.IdsWhereAtMost(ctx, acct, TypeEmailSubmission, "nextAttemptAt", maxRaw, quota)
	if err != nil || len(ids) == 0 {
		return 0
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return sent
		}
		c, ok := w.claim(ctx, acct, id)
		if !ok {
			continue // no longer due, or a peer claimed it first
		}
		w.sendOne(ctx, acct, c)
		sent++
	}
	return sent
}

// claim takes a single due record under the account lease: it stamps
// claimedAt (the claim's identity) and in the SAME write parks
// nextAttemptAt at claimedAt+ClaimWindow, then returns the identity. A
// claimed record is thereby not "due" - it leaves the due scan, so a
// due-but-live-claimed record (and its busy-loop) is structurally
// impossible and the wake path never sees a past due time it cannot act
// on. If this worker crashes before finalize the record reappears as due
// exactly ClaimWindow later and another worker reclaims it (reclaim
// timing falls out of the index, no freshness comparison). finalize
// ALWAYS rewrites nextAttemptAt, so this parked value never survives as a
// real schedule. ok is false when the record left the queue or a racing
// claim under the same lease parked it first.
func (w *SubmissionWorker) claim(ctx context.Context, acct jmap.Id, id jmap.Id) (claimedRecord, bool) {
	now := w.now()
	stamp := w.claimToken(now)
	taken := false
	leftover := ""
	_, err := w.q.db.Update(ctx, acct, func(u *objectdb.Update) error {
		taken, leftover = false, ""
		obj, err := u.Get(TypeEmailSubmission, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			return nil // destroyed while queued
		}
		if err != nil {
			return err
		}
		due, err := parseUTCDateValue(obj["nextAttemptAt"])
		if err != nil || due.After(now) {
			return nil // not due: left the queue, held, or a peer parked it
		}
		json.Unmarshal(obj["claimedAt"], &leftover)
		obj["claimedAt"] = mustJSON(stamp)
		parked := now.Add(w.cfg.ClaimWindow).UTC().Format(time.RFC3339)
		obj["nextAttemptAt"] = mustJSON(parked)
		if err := u.Put(TypeEmailSubmission, id, obj); err != nil {
			return err
		}
		taken = true
		return nil
	})
	if err != nil || !taken {
		return claimedRecord{}, false
	}
	w.stats.claimsTaken.Add(1)
	if leftover != "" {
		// The record still carried a claim stamp: its window expired
		// undelivered, so a worker died or stalled mid-send.
		w.stats.claimsReclaimed.Add(1)
		w.noteForeignStamp(leftover)
	}
	return claimedRecord{id: id, stamp: stamp}, true
}

// claimToken returns a globally-unique claim identity: an RFC3339Nano
// wall-clock stamp (human-readable, and the basis for later skew
// observability) joined by "|" to a per-worker-random, per-claim
// nonce. The nonce is what makes the token unique. Identity is the WHOLE
// token, matched exactly under the lease by finalize; two workers whose
// clocks land on the same rounded nanosecond - the realistic vector is a
// backward clock step of about ClaimWindow onto a prior claim's instant,
// e.g. a VM snapshot restore or a bad NTP correction, not nanosecond
// granularity - would otherwise mint identical timestamp-only tokens, and
// the exact-match check could then pass for the wrong worker's leg (a
// double-finalize). Distinct nonces keep every claim's identity distinct
// whatever the clocks read. claimedAt is not indexed, so this richer
// value costs nothing at the store.
func (w *SubmissionWorker) claimToken(now time.Time) string {
	return now.UTC().Format(time.RFC3339Nano) + "|" + w.nonce + "-" + strconv.FormatUint(w.seq.Add(1), 36)
}

// sendOne transmits one claimed record through the Submitter outside the
// lease and finalizes the results. It re-reads under the lease and
// verifies the claim stamp before transmitting, and finalize verifies it
// again: if a reclaimer replaced the stamp (this worker was suspended
// past ClaimWindow and lost the claim), this worker's leg is abandoned
// without touching the record.
func (w *SubmissionWorker) sendOne(ctx context.Context, acct jmap.Id, c claimedRecord) {
	var env subEnvelope
	var blobId jmap.Id
	var ds map[string]deliveryStatusObj
	claimOK, lostClaim, lost := false, false, ""
	_, err := w.q.db.Update(ctx, acct, func(u *objectdb.Update) error {
		claimOK, lostClaim, lost = false, false, ""
		obj, err := u.Get(TypeEmailSubmission, c.id)
		if errors.Is(err, objectdb.ErrNotFound) {
			return nil // destroyed while queued; nothing to send
		}
		if err != nil {
			return err
		}
		var cur string
		json.Unmarshal(obj["claimedAt"], &cur)
		if cur != c.stamp {
			lostClaim, lost = true, cur
			return nil // reclaimed by someone else
		}
		if err := json.Unmarshal(obj["envelope"], &env); err != nil || env.MailFrom == nil {
			return nil // unreadable record; leave for the give-up path
		}
		json.Unmarshal(obj["blobId"], &blobId)
		json.Unmarshal(obj["deliveryStatus"], &ds)
		claimOK = true
		return nil
	})
	if err != nil || !claimOK {
		if lostClaim {
			// A peer replaced the stamp before anything went on the
			// wire: the claim is lost but no duplicate exists.
			w.stats.claimsLostBeforeTransmit.Add(1)
			w.noteForeignStamp(lost)
		}
		return
	}

	var rcpts []SubmissionRecipient
	for _, r := range env.RcptTo {
		if ds[r.Email].Delivered == "queued" {
			rcpts = append(rcpts, SubmissionRecipient{Email: r.Email, Parameters: r.Parameters})
		}
	}
	var results []RecipientResult
	if len(rcpts) > 0 {
		results = w.transmit(ctx, acct, env, blobId, rcpts)
	}
	w.finalize(ctx, acct, c.id, c.stamp, results)
}

// transmit performs the single attempt: open the message blob, strip
// Bcc, and hand the stream to the Submitter under TransmitTimeout.
// Results the Submitter returns are applied even when they accompany an
// error - an Accepted result reports an irrevocable relay, and dropping
// it would re-send that recipient on the next attempt. Recipients the
// attempt produced no verdict for (a Submitter error, an unopenable
// blob, or a result the Submitter omitted) temporarily fail with a
// synthetic reply, so the normal backoff machinery owns the retry.
func (w *SubmissionWorker) transmit(ctx context.Context, acct jmap.Id, env subEnvelope, blobId jmap.Id, rcpts []SubmissionRecipient) []RecipientResult {
	rd, size, err := w.q.store.Open(ctx, acct, blobId)
	if err != nil {
		return mergeResults(rcpts, nil, "451 4.2.0 message content unavailable")
	}
	defer rd.Close()
	tctx, cancel := context.WithTimeout(ctx, w.cfg.TransmitTimeout)
	defer cancel()
	results, err := w.submit.Submit(tctx, SubmissionEnvelope{
		MailFrom:       env.MailFrom.Email,
		MailParameters: env.MailFrom.Parameters,
		Recipients:     rcpts,
		Size:           size,
	}, stripBcc(rd))
	if err != nil {
		return mergeResults(rcpts, results, "451 4.4.1 could not transmit to smarthost")
	}
	return mergeResults(rcpts, results, "451 4.4.1 no result reported for recipient")
}

// mergeResults returns one result per attempted recipient: the
// Submitter's verdict where one exists, and a synthetic temporary
// failure for the rest. This is what makes partial results safe to
// apply - the recipients a failed attempt said nothing about retry,
// while the ones it settled stay settled.
func mergeResults(rcpts []SubmissionRecipient, results []RecipientResult, reply string) []RecipientResult {
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		seen[r.Recipient] = true
	}
	for _, r := range rcpts {
		if !seen[r.Email] {
			results = append(results, RecipientResult{Recipient: r.Email, Outcome: TempFailed, Reply: reply})
		}
	}
	return results
}

// finalize records one attempt's results under the account lease:
// per-recipient deliveryStatus per RFC 8621 section 7 (Accepted ->
// delivered "unknown" - relaying to an external server MUST NOT be
// taken as final delivery; Rejected -> "no"; TempFailed stays "queued"
// with the reply updated), then the queue transition. Recipients still
// queued reschedule on the backoff schedule unless the submission's age
// passed GiveUpAfter after at least MinAttempts attempts, which
// abandons them with a synthetic permanent failure (RFC 3463 5.4.7).
// undoStatus turns final once any recipient was irrevocably relayed or
// nothing is left to retry; a submission with no accepted recipients
// and retries remaining stays pending, so cancel can still stop it
// (section 7.5).
func (w *SubmissionWorker) finalize(ctx context.Context, acct jmap.Id, id jmap.Id, stamp string, results []RecipientResult) {
	superseded, lost := false, ""
	w.q.db.Update(ctx, acct, func(u *objectdb.Update) error {
		superseded, lost = false, ""
		obj, err := u.Get(TypeEmailSubmission, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			return nil // destroyed mid-send; results are dropped
		}
		if err != nil {
			return err
		}
		var cur string
		json.Unmarshal(obj["claimedAt"], &cur)
		if cur != stamp {
			superseded, lost = true, cur
			return nil // reclaimed mid-send; the reclaimer's leg owns it
		}
		now := w.now()
		var ds map[string]deliveryStatusObj
		if err := json.Unmarshal(obj["deliveryStatus"], &ds); err != nil {
			return nil
		}
		for _, r := range results {
			st, known := ds[r.Recipient]
			if !known || st.Delivered != "queued" {
				continue // never regress a final recipient
			}
			st.SmtpReply = r.Reply
			switch r.Outcome {
			case Accepted:
				st.Delivered = "unknown"
			case Rejected:
				st.Delivered = "no"
			case TempFailed:
				// stays queued; only the reply changes
			}
			ds[r.Recipient] = st
		}
		var attempts uint64
		json.Unmarshal(obj["attempts"], &attempts)
		attempts++
		anyAccepted, anyQueued := false, false
		for _, st := range ds {
			switch st.Delivered {
			case "unknown", "yes":
				anyAccepted = true
			case "queued":
				anyQueued = true
			}
		}
		if anyQueued && attempts >= uint64(w.cfg.MinAttempts) {
			// Fail safe: an unparseable sendAt (store corruption or a format
			// change) abandons the record rather than retrying it forever -
			// the give-up floor (MinAttempts) has already been honored above.
			sendAt, err := parseUTCDateValue(obj["sendAt"])
			if err != nil || now.Sub(sendAt) > w.cfg.GiveUpAfter {
				for rcpt, st := range ds {
					if st.Delivered == "queued" {
						st.Delivered = "no"
						st.SmtpReply = "554 5.4.7 delivery attempts abandoned after retry timeout"
						ds[rcpt] = st
					}
				}
				anyQueued = false
			}
		}
		obj["deliveryStatus"] = mustJSON(ds)
		obj["attempts"] = mustJSON(attempts)
		delete(obj, "claimedAt")
		if anyQueued {
			next := now.Add(w.backoff(attempts))
			obj["nextAttemptAt"] = mustJSON(next.UTC().Format(time.RFC3339))
			if anyAccepted {
				// Partially relayed: irrevocable, so no longer
				// cancelable, but the temp-failed recipients retry.
				obj["undoStatus"] = mustJSON(undoFinal)
			}
			// Still queued: keep the account on the tag worklist. The
			// create set it, but re-asserting costs one idempotent
			// write and keeps the invariant local: every commit that
			// leaves work queued tags the account.
			if err := u.SetAccountTag(submissionQueueTag); err != nil {
				return err
			}
		} else {
			delete(obj, "nextAttemptAt")
			obj["undoStatus"] = mustJSON(undoFinal)
		}
		return u.Put(TypeEmailSubmission, id, obj)
	})
	if superseded {
		// The claim was lost after the transmit phase: the message may
		// already be relayed, and the reclaimer may relay it again - the
		// one counter that should always read zero.
		w.stats.finalizesSuperseded.Add(1)
		w.noteForeignStamp(lost)
	}
}

// backoff returns the delay after the n-th attempt (1-based), spread by
// backoffJitter so a burst of simultaneous temp-failures never returns
// as a synchronized retry wave (and, once spread, never re-aligns).
func (w *SubmissionWorker) backoff(attempts uint64) time.Duration {
	idx := int(attempts) - 1
	d := w.cfg.RetryPlateau
	if idx >= 0 && idx < len(w.cfg.RetrySchedule) {
		d = w.cfg.RetrySchedule[idx]
	}
	return time.Duration(float64(d) * (1 + backoffJitter*(2*w.rand()-1)))
}
