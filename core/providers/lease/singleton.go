package lease

import (
	"context"
	mrand "math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// SingletonConfig tunes RunSingleton. Zero values take defaults.
type SingletonConfig struct {
	// TTL is how long a claim stays valid without a renewal, so a crashed
	// holder's role is takeable again after at most TTL. Default 30s.
	TTL time.Duration
	// Renew is how often the holder refreshes its claim's expiry. Default TTL/3.
	Renew time.Duration
	// Poll is how often a losing candidate retries the campaign. Default TTL/2.
	Poll time.Duration
}

// RunSingleton campaigns for the named singleton role across every process
// sharing be, and runs hold while this process owns it. Under normal operation
// exactly one holder exists fleet-wide at a time: the role is a claim record
// swapped atomically (the same compare-and-swap the writer lease uses) carrying
// an expiry the holder renews, so a crashed holder's role is taken over by
// another candidate once the claim expires.
//
// hold's ctx is cancelled when ownership is lost - a renewal that no longer
// swaps, or a persistent store error - or when ctx ends; RunSingleton waits for
// hold to return before campaigning again, and itself returns only when ctx
// ends. Unlike the writer lease this role is long-held and its failover is not
// latency-critical, so it polls and renews on timers with no wake hint: the
// role can sit vacant for up to TTL, acceptable for what it guards - for
// example electing the one instance that POSTs webpush (RFC 8620 section 7.2),
// where a duplicate or briefly-absent push is harmless.
//
// The claim is never deleted, even on a clean return: a holder that has lost
// the role must not disturb a successor's claim, so a relinquished role is
// simply left to expire, exactly as a crashed one is.
func RunSingleton(ctx context.Context, be backend.Backend, name string, cfg SingletonConfig, hold func(ctx context.Context)) {
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Second
	}
	if cfg.Renew <= 0 {
		cfg.Renew = cfg.TTL / 3
	}
	if cfg.Poll <= 0 {
		cfg.Poll = cfg.TTL / 2
	}
	s := &singleton{
		be:    be,
		key:   singletonKey(name),
		ttl:   cfg.TTL,
		renew: cfg.Renew,
		poll:  cfg.Poll,
		now:   time.Now,
		rand:  mrand.Float64,
		nonce: newNonce(),
	}
	s.run(ctx, hold)
}

// singletonKey holds the claim for a named singleton role. Its "le/" prefix
// (lease election) sits in the reserved lease namespace and never collides with
// account data or writer-lease claims.
func singletonKey(name string) []byte {
	return []byte("le/" + name)
}

// singleton is the campaign state, with the same now/rand test seams as
// StoreLease so tests drive election deterministically.
type singleton struct {
	be    backend.Backend
	key   []byte
	ttl   time.Duration
	renew time.Duration
	poll  time.Duration

	now   func() time.Time
	rand  func() float64
	nonce string
	seq   atomic.Uint64
}

// run is the campaign loop.
func (s *singleton) run(ctx context.Context, hold func(ctx context.Context)) {
	for {
		if ctx.Err() != nil {
			return
		}
		token, err := s.campaign(ctx)
		if err != nil || token == nil {
			// Store error, or the role is held by a live claimant: wait a
			// jittered interval and try again.
			if s.sleep(ctx, s.pollDelay()) {
				return
			}
			continue
		}
		s.serve(ctx, token, hold)
	}
}

// campaign attempts one takeover. It returns the token now held on success, a
// nil token (no error) if a live claimant holds the role, or an error on a
// store failure.
func (s *singleton) campaign(ctx context.Context) ([]byte, error) {
	cur, err := getClaim(ctx, s.be, s.key)
	if err != nil {
		return nil, err
	}
	// A well-formed, unexpired claim means another candidate holds it. A
	// malformed claim counts as expired; the swap below makes takeover
	// race-free regardless. There is no fence and no generation bump: a
	// singleton guards activity, not data writes, so there is nothing to fence.
	if exp, ok := parseClaimExpiry(cur); ok && s.now().Before(exp) {
		return nil, nil
	}
	token := s.mint()
	swapped, err := cas(ctx, s.be, s.key, cur, token)
	if err != nil {
		return nil, err
	}
	if !swapped {
		return nil, nil // another candidate won the race; retry as a loser
	}
	return token, nil
}

// serve runs hold while renewing the claim, and returns once the role is lost
// or ctx ends. hold always returns before serve does.
func (s *singleton) serve(ctx context.Context, token []byte, hold func(ctx context.Context)) {
	holdCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		hold(holdCtx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	for {
		if s.sleep(ctx, s.renew) {
			return // ctx ended
		}
		next := s.mint()
		swapped, err := cas(ctx, s.be, s.key, token, next)
		if err != nil || !swapped {
			return // renewal failed: the role was taken, so give it up
		}
		token = next
	}
}

// mint builds a claim token whose expiry is one TTL out.
func (s *singleton) mint() []byte { return mintToken(s.nonce, &s.seq, s.now().Add(s.ttl)) }

// pollDelay is one jittered candidate retry interval.
func (s *singleton) pollDelay() time.Duration {
	return time.Duration(float64(s.poll) * (0.5 + s.rand()))
}

// sleep waits for d, returning true if ctx ended first.
func (s *singleton) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-ctx.Done():
		return true
	}
}
