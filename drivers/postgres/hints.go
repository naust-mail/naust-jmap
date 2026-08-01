package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// This file adds the cluster hint transport: a best-effort accelerator that
// lets change notifications and lease-wake hints cross process boundaries over
// Postgres LISTEN/NOTIFY. It is strictly optional. For change and lease hints,
// correctness never depends on a hint arriving: change delivery is reconciled
// by clients on state strings (RFC 8620 section 7), and lease safety is the
// store's generation fence, not a hint. A dropped or duplicated hint costs only
// latency. Revocations are held to a stronger contract: the durable
// revocations table is the truth and NOTIFY is only the fast path. Each
// publish upserts the table in the publisher's transaction, and every
// process re-reads the table's retention window on a slow poll, so a
// NOTIFY lost while this process's listener is reconnecting is
// re-delivered by the next poll - at-least-once delivery, applied
// idempotently by the consumer (see auth.Revoker). The residuals: the
// database being unreachable for longer than the retention window while
// connections stay up, and clock skew beyond the consumer's slack.
//
// One dedicated listener connection per process carries every hint. That
// connection is the only part of the system that needs a real session (LISTEN
// is session state), so it is dialed straight from the pool's connection config
// rather than borrowed from the pool. It never runs a transaction: a LISTENer
// sitting in a long transaction stops Postgres from reclaiming its notification
// queue. Everything published is a plain, poolable pg_notify statement.
//
// Payloads are UNTRUSTED input. Any database role that can NOTIFY on these
// channels can forge or corrupt one, so the listener decodes strictly into a
// typed struct and drops anything malformed. The worst a forgery achieves is a
// spurious wakeup or resync - no data is exposed, and a role able to NOTIFY
// already holds database access.

const (
	chanChanges = "naust_changes"
	chanLease   = "naust_lease"
	chanRevoke  = "naust_revoke"

	// revokeSubQueueDepth buffers each revocation subscriber. Unlike change
	// and lease hints, a revocation is not latency-only - but the runtime
	// consumer drains promptly (it only closes connections), so a full
	// buffer means a pathological flood, and dropping with a loud log beats
	// letting a hostile NOTIFY flood park the listener goroutine.
	revokeSubQueueDepth = 64

	// notifyPayloadBudget keeps a single NOTIFY payload well under Postgres's
	// ~8000-byte cap; a change carrying more type states than fits is split
	// across several notifications.
	notifyPayloadBudget = 7000

	// publishQueueDepth bounds outstanding async publishes. A full queue drops
	// hints (lossy by contract) rather than blocking a committer or releaser.
	publishQueueDepth = 1024

	backoffInitial = 1 * time.Second
	backoffMax     = 30 * time.Second

	// dispatchBudget caps how many notifications the listener processes per
	// second; the rest of that second is dropped and summarized in one warning.
	// Hints are lossy by contract, so shedding costs only latency (everything
	// reconciles through the store's timers), and the cap is far above any
	// healthy commit rate - it exists so a buggy or hostile publisher flooding
	// the channels cannot pin this process's CPU on decode and fan-out.
	dispatchBudget = 2000

	// stableConnThreshold is how long a listener connection must stay up to be
	// counted healthy. A connection that drops sooner is treated as flapping and
	// escalates the reconnect backoff, so a session that keeps dying right after
	// LISTEN - an aggressive pooler, a terminate loop, a flaky path - cannot
	// become an unthrottled reconnect storm.
	stableConnThreshold = 10 * time.Second

	// revocationRetention is how long a revocation row stays in the table
	// and how far back each poll re-asserts. It must exceed any realistic
	// listener outage: a revocation is only lost if this process cannot
	// reach the database for longer than this while the revoked user's
	// connections stay open. Rows older than the window are pruned. The
	// two roles deliberately share one value: consumers apply events
	// against the event's own timestamp, so re-asserting an old row
	// still closes a connection that predates it. The re-assert depth is
	// therefore the horizon of that safety net - shortening it would
	// narrow the net, not just save work - and pruning deeper than the
	// scan would keep rows nothing reads.
	revocationRetention = 24 * time.Hour

	// revocationPollInterval is the base period of the per-process poll
	// that re-reads the revocations table. Each tick is jittered so a
	// fleet restarted together does not thundering-herd the table. The
	// poll is the delivery floor - NOTIFY latency when the listener is
	// healthy, at most about one interval when it is not.
	revocationPollInterval = time.Minute

	// revocationWarnRows is the poll result size above which a warning is
	// logged. The poll deliberately has no row cap - capping would let an
	// attacker flood dummy revocations to push a real one past the cap -
	// so an oversized table is reported loudly instead of trimmed.
	revocationWarnRows = 10_000
)

// changePayload is the wire form of a change hint.
type changePayload struct {
	Origin  string         `json:"o"`
	Account jmap.Id        `json:"a"`
	Types   jmap.TypeState `json:"t"`
}

// leasePayload is the wire form of a lease-freed hint.
type leasePayload struct {
	Origin  string  `json:"o"`
	Account jmap.Id `json:"a"`
}

// revokePayload is the wire form of a credential revocation. It carries
// no origin: revocations are deliberately delivered back to the
// publishing process too, because the box that revoked the credential
// must kill its own live connections just like every other box.
type revokePayload struct {
	Username string `json:"u"`
}

// decodeChange parses an untrusted change payload. A malformed payload, or one
// missing its origin or account, is rejected so the listener can drop it.
func decodeChange(payload []byte) (changePayload, error) {
	var p changePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return changePayload{}, err
	}
	if p.Origin == "" || p.Account == "" {
		return changePayload{}, errors.New("postgres: change hint missing origin or account")
	}
	return p, nil
}

// decodeRevoke parses an untrusted revocation payload; empty usernames
// are rejected so the listener can drop malformed or forged blanks.
func decodeRevoke(payload []byte) (revokePayload, error) {
	var p revokePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return revokePayload{}, err
	}
	if p.Username == "" {
		return revokePayload{}, errors.New("postgres: revocation hint missing username")
	}
	return p, nil
}

// decodeLease parses an untrusted lease payload, with the same rejection rule.
func decodeLease(payload []byte) (leasePayload, error) {
	var p leasePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return leasePayload{}, err
	}
	if p.Origin == "" || p.Account == "" {
		return leasePayload{}, errors.New("postgres: lease hint missing origin or account")
	}
	return p, nil
}

// publishReq is one queued NOTIFY.
type publishReq struct {
	channel string
	payload string
}

// Hints is the process-wide hint transport. Create one per process with
// OpenHints and share its Notifier and Waker across every consumer.
type Hints struct {
	store  *Store
	origin string
	local  *notify.InProcess

	pub chan publishReq

	wmu     sync.Mutex
	waiters map[jmap.Id][]chan struct{}

	rmu     sync.Mutex
	revSubs map[chan auth.Revocation]struct{}

	notifier *hintsNotifier
	waker    *hintsWaker
	revoker  *hintsRevoker

	// Flood-shed state, touched only by the listener goroutine (see shed).
	// shedNow is a test seam; time.Now outside tests.
	shedNow     func() time.Time
	shedWindow  time.Time
	shedCount   int
	shedDropped uint64

	cancel      context.CancelFunc
	listenDone  chan struct{}
	publishDone chan struct{}
	pollDone    chan struct{}
}

// OpenHints starts the shared hint transport for this process: one dedicated
// listener connection carrying change notifications and lease wake hints for
// every consumer in the process. It returns immediately; the listener connects
// in the background and retries, so a transport that cannot yet reach Postgres
// simply runs degraded (every hint missed, everything falling back to the
// store's own polling and expiry) until the connection comes up.
func OpenHints(ctx context.Context, store *Store) (*Hints, error) {
	if store == nil {
		return nil, errors.New("postgres: OpenHints needs a store")
	}
	var ob [16]byte
	// crypto/rand.Read is documented never to fail on supported platforms.
	_, _ = rand.Read(ob[:])

	h := &Hints{
		store:       store,
		origin:      hex.EncodeToString(ob[:]),
		local:       notify.NewInProcess(),
		pub:         make(chan publishReq, publishQueueDepth),
		waiters:     make(map[jmap.Id][]chan struct{}),
		revSubs:     make(map[chan auth.Revocation]struct{}),
		shedNow:     time.Now,
		listenDone:  make(chan struct{}),
		publishDone: make(chan struct{}),
		pollDone:    make(chan struct{}),
	}
	h.notifier = &hintsNotifier{h: h}
	h.waker = &hintsWaker{h: h}
	h.revoker = &hintsRevoker{h: h}

	loopCtx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	// Best-effort synchronous first connect: by the time OpenHints returns the
	// listener is already subscribed, so a hint published immediately after is
	// delivered rather than missed (Postgres only routes a NOTIFY to sessions
	// already listening when it commits). A failure here is not fatal - the loop
	// retries and the transport runs degraded until the connection comes up.
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := h.dialListener(dialCtx)
	dialCancel()
	if err != nil {
		slog.Warn("postgres: hint listener initial connect failed, starting degraded", "err", err)
	}

	go h.listen(loopCtx, conn)
	go h.publish(loopCtx)
	go h.pollRevocations(loopCtx)
	return h, nil
}

// Notifier returns the cross-instance Notifier backed by this transport.
func (h *Hints) Notifier() notify.Notifier { return h.notifier }

// Waker returns the cross-instance lease Waker backed by this transport.
func (h *Hints) Waker() lease.Waker { return h.waker }

// Revoker returns the cross-instance credential Revoker backed by this
// transport: every revocation published on the fleet's database (see
// PublishRevocation) is delivered to every subscriber in every process,
// including the one that published it.
func (h *Hints) Revoker() auth.Revoker { return h.revoker }

// Close stops the listener, publisher, and revocation poll loops and
// closes the listener connection. Local subscriptions are owned by their
// callers and are not force closed here.
func (h *Hints) Close() error {
	h.cancel()
	<-h.listenDone
	<-h.publishDone
	<-h.pollDone
	return nil
}

// enqueue hands a NOTIFY to the async publisher without blocking. A full queue
// drops the hint - lossy by contract, and never a reason to stall a committer.
func (h *Hints) enqueue(channel, payload string) {
	select {
	case h.pub <- publishReq{channel: channel, payload: payload}:
	default:
		slog.Warn("postgres: hint publish queue full, dropping", "channel", channel)
	}
}

// publish drains the async queue, issuing each pg_notify on the pool. Errors
// are logged and dropped: a failed hint only costs latency.
func (h *Hints) publish(ctx context.Context) {
	defer close(h.publishDone)
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-h.pub:
			if _, err := h.store.pool.Exec(ctx, "SELECT pg_notify($1, $2)", req.channel, req.payload); err != nil && ctx.Err() == nil {
				slog.Warn("postgres: hint publish failed", "channel", req.channel, "err", err)
			}
		}
	}
}

// listen owns the dedicated listener connection: connect, LISTEN both channels,
// and pump notifications until an error, then reconnect with capped backoff.
// This loop is also the degraded mode - behind a transaction-mode pooler LISTEN
// keeps failing and the loop keeps retrying at the backoff cap while everything
// else runs on the store's timers.
// conn is the connection dialed by OpenHints (may be nil if that first attempt
// failed); every subsequent connection is dialed here.
func (h *Hints) listen(ctx context.Context, conn *pgx.Conn) {
	defer close(h.listenDone)
	backoff := backoffInitial
	for {
		if ctx.Err() != nil {
			if conn != nil {
				_ = conn.Close(context.Background())
			}
			return
		}
		if conn == nil {
			var err error
			conn, err = h.dialListener(ctx)
			if err != nil {
				if sleepCtx(ctx, backoff) {
					return
				}
				backoff = nextBackoff(backoff)
				continue
			}
		}
		start := time.Now()
		h.consume(ctx, conn)
		_ = conn.Close(context.Background())
		conn = nil
		if ctx.Err() != nil {
			return
		}
		// A connection that stayed up past the stability threshold is healthy:
		// reconnect at once and reset the backoff. One that dropped sooner is
		// treated as flapping and throttled, so a session dying right after
		// LISTEN cannot become an unthrottled reconnect storm.
		delay, next := afterConsume(time.Since(start), backoff)
		backoff = next
		if delay > 0 && sleepCtx(ctx, delay) {
			return
		}
	}
}

// afterConsume computes the delay before the next dial and the backoff to carry
// forward, given how long the just-closed connection stayed up. A healthy
// long-lived connection reconnects immediately and resets the backoff; a
// short-lived one is throttled by the escalating backoff.
func afterConsume(uptime, backoff time.Duration) (delay, next time.Duration) {
	if uptime >= stableConnThreshold {
		return 0, backoffInitial
	}
	return backoff, nextBackoff(backoff)
}

// dialListener opens a fresh session (never a pool connection) and subscribes to
// both hint channels.
func (h *Hints) dialListener(ctx context.Context) (*pgx.Conn, error) {
	cfg := h.store.pool.Config().ConnConfig.Copy()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Channel names are fixed constants, never user input.
	for _, ch := range []string{chanChanges, chanLease, chanRevoke} {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			_ = conn.Close(context.Background())
			return nil, err
		}
	}
	return conn, nil
}

// consume pumps notifications until the connection errors or ctx ends.
func (h *Hints) consume(ctx context.Context, conn *pgx.Conn) {
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		h.dispatch(n.Channel, n.Payload)
	}
}

// dispatch decodes one untrusted notification and applies it, dropping anything
// malformed, self-originated, or over the per-second dispatchBudget. It runs
// only on the listener goroutine, so the shed state needs no lock.
func (h *Hints) dispatch(channel, payload string) {
	// Revocations are security events and are exempt from load
	// shedding: dropping one leaves revoked credentials live. The
	// budget exists to protect cache-refresh traffic, not access
	// control.
	if channel != chanRevoke && h.shed() {
		return
	}
	switch channel {
	case chanChanges:
		p, err := decodeChange([]byte(payload))
		if err != nil {
			slog.Warn("postgres: dropping malformed change hint", "err", err)
			return
		}
		if p.Origin == h.origin {
			return // our own publish, already delivered to local subscribers
		}
		h.local.Publish(context.Background(), p.Account, p.Types)
	case chanLease:
		p, err := decodeLease([]byte(payload))
		if err != nil {
			slog.Warn("postgres: dropping malformed lease hint", "err", err)
			return
		}
		if p.Origin == h.origin {
			return
		}
		h.signalWaiters(p.Account)
	case chanRevoke:
		p, err := decodeRevoke([]byte(payload))
		if err != nil {
			slog.Warn("postgres: dropping malformed revocation hint", "err", err)
			return
		}
		// No self-origin filter, deliberately: the publishing process
		// must close its own live connections too, and its runtime's
		// connection index is only reached through this delivery.
		// A live NOTIFY is stamped with the delivery instant rather than
		// carrying a time on the wire: it arrives within moments of the
		// publishing commit, so now() is the honest bound, and the exact
		// at is only needed by the poll, which reads it from the table.
		if n := h.deliverRevocation(auth.Revocation{Username: p.Username, At: time.Now()}); n > 0 {
			slog.Warn("postgres: revocation subscriber queue full, delivery deferred to the poll", "dropped", n)
		}
	}
}

// deliverRevocation fans one revocation out to every subscriber and
// reports how many sends were dropped. Sends are non-blocking under the
// lock: a subscriber whose buffer is full is skipped rather than allowed
// to stall the caller (see revokeSubQueueDepth). A dropped send is not a
// lost revocation - the poll re-asserts every row in the retention
// window, so delivery is retried within about one interval; each caller
// logs drops at a volume fitting its path.
func (h *Hints) deliverRevocation(ev auth.Revocation) int {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	dropped := 0
	for ch := range h.revSubs {
		select {
		case ch <- ev:
		default:
			dropped++
		}
	}
	return dropped
}

// pollRevocations is the revocation delivery floor: on a jittered
// interval it prunes rows older than the retention window and
// re-delivers every row inside it through the normal fan-out. It runs
// on the pool, independent of the listener connection, so a NOTIFY
// missed during a listener outage is re-asserted here at most about one
// interval late. Redelivery of already-applied revocations is by
// design - consumers apply events idempotently (see auth.Revoker), so
// the loop needs no memory of what it has delivered before. No
// watermark, deliberately: row visibility order is not commit order, so
// an incremental scan could skip a row committed late, and the full
// window is small (distinct revoked users in the window, warned above
// revocationWarnRows).
func (h *Hints) pollRevocations(ctx context.Context) {
	defer close(h.pollDone)
	for {
		if sleepCtx(ctx, jitteredInterval(revocationPollInterval)) {
			return
		}
		h.pollRevocationsOnce(ctx)
	}
}

// pollRevocationsOnce runs one prune-and-redeliver pass. Query errors are
// logged and left for the next tick - the transport runs degraded, same
// philosophy as the listener.
func (h *Hints) pollRevocationsOnce(ctx context.Context) {
	if _, err := h.store.pool.Exec(ctx, "DELETE FROM revocations WHERE at < now() - $1::interval", revocationRetention.String()); err != nil && ctx.Err() == nil {
		// Prune failure never blocks the read: a fat table costs scan
		// time, not correctness.
		slog.Warn("postgres: revocation prune failed", "err", err)
	}
	rows, err := h.store.pool.Query(ctx, "SELECT username, at FROM revocations WHERE at >= now() - $1::interval", revocationRetention.String())
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("postgres: revocation poll failed", "err", err)
		}
		return
	}
	var evs []auth.Revocation
	for rows.Next() {
		var ev auth.Revocation
		if err := rows.Scan(&ev.Username, &ev.At); err != nil {
			slog.Warn("postgres: revocation poll scan failed", "err", err)
			break
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("postgres: revocation poll failed", "err", err)
	}
	rows.Close()
	if len(evs) > revocationWarnRows {
		slog.Warn("postgres: revocations table unusually large", "rows", len(evs))
	}
	// One summary per pass, not one log per drop: a full window fanning
	// into slow subscribers could otherwise emit thousands of lines per
	// poll, and a dropped redelivery is retried on the next pass anyway.
	dropped := 0
	for _, ev := range evs {
		dropped += h.deliverRevocation(ev)
	}
	if dropped > 0 {
		slog.Warn("postgres: revocation redeliveries dropped on full subscriber queues", "dropped", dropped)
	}
}

// jitteredInterval spreads d over [0.75d, 1.25d) so a fleet of processes
// started together does not poll in lockstep.
func jitteredInterval(d time.Duration) time.Duration {
	return d*3/4 + mrand.N(d/2)
}

// shed reports whether this notification is over the listener's per-second
// budget and must be dropped. On the first notification of each new second it
// summarizes anything shed during the previous one, so a sustained flood
// produces one warning per second, never one per drop. Listener-goroutine only.
func (h *Hints) shed() bool {
	now := h.shedNow()
	if now.Sub(h.shedWindow) >= time.Second {
		if h.shedDropped > 0 {
			slog.Warn("postgres: hint flood, dropped notifications", "count", h.shedDropped)
		}
		h.shedWindow = now
		h.shedCount = 0
		h.shedDropped = 0
	}
	h.shedCount++
	if h.shedCount > dispatchBudget {
		h.shedDropped++
		return true
	}
	return false
}

// signalWaiters releases every waiter currently parked on account.
func (h *Hints) signalWaiters(account jmap.Id) {
	h.wmu.Lock()
	chs := h.waiters[account]
	delete(h.waiters, account)
	h.wmu.Unlock()
	for _, ch := range chs {
		close(ch)
	}
}

// hintsNotifier fans changes out locally and, in the same call, publishes a
// cross-instance hint.
type hintsNotifier struct{ h *Hints }

func (n *hintsNotifier) Publish(ctx context.Context, account jmap.Id, types jmap.TypeState) {
	if len(types) == 0 {
		return
	}
	// Local subscribers first, synchronously - they must never depend on the
	// round trip through Postgres.
	n.h.local.Publish(ctx, account, types)
	for _, payload := range n.h.changePayloads(account, types) {
		n.h.enqueue(chanChanges, payload)
	}
}

func (n *hintsNotifier) Subscribe(ctx context.Context, accounts []jmap.Id) (notify.Subscription, error) {
	return n.h.local.Subscribe(ctx, accounts)
}

func (n *hintsNotifier) SubscribeAll(ctx context.Context) (notify.Subscription, error) {
	return n.h.local.SubscribeAll(ctx)
}

// changePayloads marshals the change into one payload, or several if the type
// map is large enough to approach the NOTIFY size cap.
func (h *Hints) changePayloads(account jmap.Id, types jmap.TypeState) []string {
	if s, ok := marshalChange(h.origin, account, types); ok && len(s) <= notifyPayloadBudget {
		return []string{s}
	}
	var out []string
	chunk := jmap.TypeState{}
	flush := func() {
		if len(chunk) == 0 {
			return
		}
		if s, ok := marshalChange(h.origin, account, chunk); ok {
			out = append(out, s)
		}
		chunk = jmap.TypeState{}
	}
	for name, state := range types {
		chunk[name] = state
		if s, ok := marshalChange(h.origin, account, chunk); ok && len(s) > notifyPayloadBudget && len(chunk) > 1 {
			delete(chunk, name)
			flush()
			chunk[name] = state
		}
	}
	flush()
	return out
}

// marshalChange encodes one change payload, reporting ok=false if it cannot be
// marshaled (which never happens for string maps, but keeps callers total).
func marshalChange(origin string, account jmap.Id, types jmap.TypeState) (string, bool) {
	b, err := json.Marshal(changePayload{Origin: origin, Account: account, Types: types})
	if err != nil {
		slog.Warn("postgres: could not marshal change hint", "err", err)
		return "", false
	}
	return string(b), true
}

// hintsWaker turns a lease release into a cross-instance wake hint and lets a
// waiter block for one.
type hintsWaker struct{ h *Hints }

// Wake publishes a lease-freed hint. It does not block: the notify is queued
// for the async publisher. Local waiters need no hint - they already contend on
// the store lease's process-local mutex.
func (w *hintsWaker) Wake(account jmap.Id) {
	b, err := json.Marshal(leasePayload{Origin: w.h.origin, Account: account})
	if err != nil {
		slog.Warn("postgres: could not marshal lease hint", "err", err)
		return
	}
	w.h.enqueue(chanLease, string(b))
}

// AwaitWake blocks until a lease-freed hint for account arrives, d elapses, or
// ctx is done. A spurious early return is allowed - the caller re-checks the
// store regardless.
func (w *hintsWaker) AwaitWake(ctx context.Context, account jmap.Id, d time.Duration) {
	ch := make(chan struct{})
	w.h.wmu.Lock()
	w.h.waiters[account] = append(w.h.waiters[account], ch)
	w.h.wmu.Unlock()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
	w.removeWaiter(account, ch)
}

// removeWaiter unregisters ch if it is still parked (the timer or ctx path). If
// signalWaiters already took and closed it, ch is simply absent here.
func (w *hintsWaker) removeWaiter(account jmap.Id, ch chan struct{}) {
	w.h.wmu.Lock()
	defer w.h.wmu.Unlock()
	chs := w.h.waiters[account]
	for i, c := range chs {
		if c == ch {
			w.h.waiters[account] = append(chs[:i], chs[i+1:]...)
			if len(w.h.waiters[account]) == 0 {
				delete(w.h.waiters, account)
			}
			return
		}
	}
}

// hintsRevoker implements auth.Revoker over the listener connection.
type hintsRevoker struct{ h *Hints }

// Revocations subscribes to the fleet's revocation stream. The channel
// is buffered (revokeSubQueueDepth) and is closed, after unregistering,
// when ctx ends.
func (r *hintsRevoker) Revocations(ctx context.Context) <-chan auth.Revocation {
	ch := make(chan auth.Revocation, revokeSubQueueDepth)
	r.h.rmu.Lock()
	r.h.revSubs[ch] = struct{}{}
	r.h.rmu.Unlock()
	go func() {
		<-ctx.Done()
		r.h.rmu.Lock()
		delete(r.h.revSubs, ch)
		r.h.rmu.Unlock()
		// Safe to close now: deliverRevocation only sends while ch is
		// registered, and both run under rmu.
		close(ch)
	}()
	return ch
}

// RevocationExecer is the slice of pgx this package needs to publish a
// revocation: pgxpool.Pool, pgx.Conn, and pgx.Tx all satisfy it.
type RevocationExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// PublishRevocation announces that username's credentials are revoked,
// so every process subscribed through Hints.Revoker closes that
// identity's live connections. Call it on the same transaction that
// commits the credential change: NOTIFY only fires when the
// transaction commits, so the revocation and the credential change
// become one atomic event - no window where one is visible without the
// other, and a rolled-back change never kills connections.
//
// The upsert into the revocations table is the durable half of the
// contract: NOTIFY reaches only sessions listening at commit time, and
// the row is what every process's poll re-asserts from afterwards
// (at-least-once delivery; see the file header). Re-revoking a username
// advances its row's at rather than adding a row.
func PublishRevocation(ctx context.Context, db RevocationExecer, username string) error {
	if username == "" {
		return errors.New("postgres: cannot publish a revocation for an empty username")
	}
	if !utf8.ValidString(username) {
		return errors.New("postgres: cannot publish a revocation for a non-UTF-8 username")
	}
	b, err := json.Marshal(revokePayload{Username: username})
	if err != nil {
		return err
	}
	if len(b) > notifyPayloadBudget {
		return errors.New("postgres: revocation username too long for a NOTIFY payload")
	}
	if _, err := db.Exec(ctx, "INSERT INTO revocations (username) VALUES ($1) ON CONFLICT (username) DO UPDATE SET at = now()", username); err != nil {
		return err
	}
	_, err = db.Exec(ctx, "SELECT pg_notify($1, $2)", chanRevoke, string(b))
	return err
}

// sleepCtx sleeps for d, returning true if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return false
	case <-ctx.Done():
		return true
	}
}

// nextBackoff doubles d up to the cap.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > backoffMax {
		return backoffMax
	}
	return d
}
