package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// This file adds the cluster hint transport: a best-effort accelerator that
// lets change notifications and lease-wake hints cross process boundaries over
// Postgres LISTEN/NOTIFY. It is strictly optional. Correctness never depends on
// a hint arriving: change delivery is reconciled by clients on state strings
// (RFC 8620 section 7), and lease safety is the store's generation fence, not a
// hint. A dropped or duplicated hint costs only latency.
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

	// notifyPayloadBudget keeps a single NOTIFY payload well under Postgres's
	// ~8000-byte cap; a change carrying more type states than fits is split
	// across several notifications.
	notifyPayloadBudget = 7000

	// publishQueueDepth bounds outstanding async publishes. A full queue drops
	// hints (lossy by contract) rather than blocking a committer or releaser.
	publishQueueDepth = 1024

	backoffInitial = 1 * time.Second
	backoffMax     = 30 * time.Second

	// stableConnThreshold is how long a listener connection must stay up to be
	// counted healthy. A connection that drops sooner is treated as flapping and
	// escalates the reconnect backoff, so a session that keeps dying right after
	// LISTEN - an aggressive pooler, a terminate loop, a flaky path - cannot
	// become an unthrottled reconnect storm.
	stableConnThreshold = 10 * time.Second
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

	notifier *hintsNotifier
	waker    *hintsWaker

	cancel      context.CancelFunc
	listenDone  chan struct{}
	publishDone chan struct{}
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
		listenDone:  make(chan struct{}),
		publishDone: make(chan struct{}),
	}
	h.notifier = &hintsNotifier{h: h}
	h.waker = &hintsWaker{h: h}

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
	return h, nil
}

// Notifier returns the cross-instance Notifier backed by this transport.
func (h *Hints) Notifier() notify.Notifier { return h.notifier }

// Waker returns the cross-instance lease Waker backed by this transport.
func (h *Hints) Waker() lease.Waker { return h.waker }

// Close stops the listener and publisher loops and closes the listener
// connection. Local subscriptions are owned by their callers and are not force
// closed here.
func (h *Hints) Close() error {
	h.cancel()
	<-h.listenDone
	<-h.publishDone
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
	for _, ch := range []string{chanChanges, chanLease} {
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
// malformed or self-originated.
func (h *Hints) dispatch(channel, payload string) {
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
	}
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
