package lease

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	mrand "math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// Waker is an optional cross-process wake hint for StoreLease waiters. A
// waiter that would otherwise poll can instead block on AwaitWake until a
// holder on any instance signals Wake for the account, so a lease handed off
// on one machine is picked up on another without waiting out a poll interval.
//
// Implementations are lossy by contract: a dropped hint only delays a waiter
// until its next poll, never breaks correctness. Wake must not block.
type Waker interface {
	// Wake signals that account may have just been freed.
	Wake(account jmap.Id)
	// AwaitWake returns when a hint for account arrives, d elapses, or ctx
	// is done. Spurious early returns are allowed - the caller re-checks the
	// store either way.
	AwaitWake(ctx context.Context, account jmap.Id, d time.Duration)
}

// StoreLeaseConfig tunes StoreLease. Zero values take defaults.
type StoreLeaseConfig struct {
	// Expiry is how long a claim stays valid without being released. It is a
	// liveness knob only, never a safety one: a crashed holder's account is
	// takeable again after Expiry, but correctness never depends on the value
	// because every commit still fences on the exact claim token. Default 15s.
	Expiry time.Duration
	// Poll is the fallback cadence a waiter re-checks a held claim at (jittered
	// per wait). Default 25ms.
	Poll time.Duration
	// Waker, if set, lets waiters block for a cross-process hint instead of
	// polling. Nil means pure polling.
	Waker Waker
}

// StoreLease is the store-backed Manager: the per-account lease lives as a
// single claim record in the backend, so instances sharing one store exclude
// each other at Acquire, not just at commit. This is the difference from
// InProcess, whose mutex is instance-local: two InProcess instances over one
// store both hold at once and rely on fencing alone, while two StoreLease
// instances serialize.
//
// One key does both jobs. The claim value is a globally unique token, and that
// same token is the fence: Fence asserts the claim key still holds this lease's
// token, so once a newer holder has replaced it (a fresh acquire or an expiry
// takeover writes a new token), a superseded holder's late commit fails and
// changes nothing. There is no separate generation counter to read back, so
// there is no read-back race between two acquirers.
//
// A claim carries an expiry so a crashed holder's account frees itself. Expiry
// comparisons use wall clocks that may differ across machines, but skew is
// harmless: it can only make a takeover early or late, never unsafe, because
// the superseded holder's next commit fails its claim fence. Expiry is
// therefore never a safety mechanism, only a liveness one.
//
// Acquire and Release swap the claim atomically across instances: on a backend
// that implements backend.CompareAndSwapper they use it, otherwise an
// Assert-guarded batch, which is a true compare-and-swap only on a backend that
// serializes its writes. Never mix Manager types over one store: InProcess
// fences on its generation key and StoreLease fences on its claim key, so an
// InProcess box and a StoreLease box acting on one account would fence on
// different keys and neither Assert would catch the other - real corruption, not
// just lost efficiency. Run exactly one Manager type across a fleet.
type StoreLease struct {
	be     backend.Backend
	expiry time.Duration
	poll   time.Duration
	waker  Waker

	now   func() time.Time // test seam; time.Now outside tests
	rand  func() float64   // test seam; jitter source in [0,1)
	nonce string           // per-instance random claim-value base
	seq   atomic.Uint64    // per-acquire counter, keeps every claim unique

	mu    sync.Mutex
	locks map[jmap.Id]*accountLock
}

// NewStoreLease returns a store-backed Manager over be.
func NewStoreLease(be backend.Backend, cfg StoreLeaseConfig) *StoreLease {
	if cfg.Expiry <= 0 {
		cfg.Expiry = 15 * time.Second
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 25 * time.Millisecond
	}
	return &StoreLease{
		be:     be,
		expiry: cfg.Expiry,
		poll:   cfg.Poll,
		waker:  cfg.Waker,
		now:    time.Now,
		rand:   mrand.Float64,
		nonce:  newNonce(),
		locks:  make(map[jmap.Id]*accountLock),
	}
}

// storeClaimKey holds an account's single claim record, whose unique value is
// both the exclusion token and the fence token.
func storeClaimKey(account jmap.Id) []byte {
	return []byte("lc/" + string(account))
}

// Acquire implements Manager.
func (m *StoreLease) Acquire(ctx context.Context, account jmap.Id) (Lease, error) {
	al := m.accountLock(account)

	// Take the process-local mutex in a way that honors ctx, mirroring the
	// in-process manager: block for it in a goroutine and abandon that
	// goroutine on cancellation (it unlocks once the lock finally arrives).
	// Past this point at most one goroutine per account per process talks to
	// the store, so the store only ever arbitrates between separate instances.
	locked := make(chan struct{})
	go func() {
		al.mu.Lock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-ctx.Done():
		go func() {
			<-locked
			al.mu.Unlock()
		}()
		return nil, ctx.Err()
	}

	claimKey := storeClaimKey(account)
	for {
		old, err := getClaim(ctx, m.be, claimKey)
		if err != nil {
			al.mu.Unlock()
			return nil, err
		}
		// A well-formed, unexpired claim means another instance holds it;
		// wait and retry. A malformed claim counts as expired: the atomic swap
		// below makes taking it over race-free regardless.
		if exp, ok := parseClaimExpiry(old); ok && m.now().Before(exp) {
			if err := m.wait(ctx, account, m.waitDuration(exp)); err != nil {
				al.mu.Unlock()
				return nil, err
			}
			continue
		}

		claim := m.newClaimValue()
		switch swapped, err := cas(ctx, m.be, claimKey, old, claim); {
		case err != nil:
			al.mu.Unlock()
			return nil, err
		case !swapped:
			// Another instance changed the claim first; re-read and retry.
			continue
		}
		return &storeLease{m: m, al: al, account: account, claimKey: claimKey, claim: claim}, nil
	}
}

// cas atomically swaps key from expected (nil means the key must be absent) to
// newVal, reporting whether this caller won. It uses the backend's native
// compare-and-swap when the backend provides one, and otherwise an
// Assert-guarded batch, which is a true compare-and-swap only on a backend that
// serializes its writes. The store lease and the singleton election both claim
// through it, so their exclusion is atomic on every backend that can host them.
func cas(ctx context.Context, be backend.Backend, key, expected, newVal []byte) (bool, error) {
	if c, ok := be.(backend.CompareAndSwapper); ok {
		return c.CompareAndSwap(ctx, key, expected, newVal)
	}
	var b backend.Batch
	b.Assert(key, expected)
	b.Set(key, newVal)
	switch err := be.WriteBatch(ctx, &b); {
	case err == nil:
		return true, nil
	case errors.Is(err, backend.ErrAssertFailed):
		return false, nil
	default:
		return false, err
	}
}

// getClaim reads key, mapping an absent key to a nil value so callers can pass
// it straight to cas as the "must be absent" expected value.
func getClaim(ctx context.Context, be backend.Backend, key []byte) ([]byte, error) {
	v, err := be.Get(ctx, key)
	if errors.Is(err, backend.ErrNotFound) {
		return nil, nil
	}
	return v, err
}

// newNonce returns 8 bytes of hex, the per-instance base that makes a minted
// token unique across instances.
func newNonce() string {
	var nb [8]byte
	// crypto/rand.Read is documented never to fail on supported platforms.
	_, _ = rand.Read(nb[:])
	return hex.EncodeToString(nb[:])
}

// mintToken builds a globally-unique claim token: an expiry deadline followed
// by a per-instance nonce and a per-mint counter, so every token is distinct
// and a holder can compare-and-swap against its own exact token.
func mintToken(nonce string, seq *atomic.Uint64, exp time.Time) []byte {
	return []byte(exp.UTC().Format(time.RFC3339Nano) + "|" + nonce + "-" + strconv.FormatUint(seq.Add(1), 36))
}

// accountLock returns the process-local mutex holder for account, creating it
// on first use.
func (m *StoreLease) accountLock(account jmap.Id) *accountLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	al, ok := m.locks[account]
	if !ok {
		al = &accountLock{}
		m.locks[account] = al
	}
	return al
}

// newClaimValue mints a token that is live for the configured expiry.
func (m *StoreLease) newClaimValue() []byte {
	return mintToken(m.nonce, &m.seq, m.now().Add(m.expiry))
}

// newExpiredToken mints a token whose deadline is already in the past, written
// on Release so the next waiter frees the claim at once via the same takeover
// path a naturally-expired claim uses.
func (m *StoreLease) newExpiredToken() []byte {
	return mintToken(m.nonce, &m.seq, m.now().Add(-time.Second))
}

// parseClaimExpiry extracts the expiry deadline from a claim value. A nil or
// malformed claim returns ok=false and is treated as expired by the caller.
func parseClaimExpiry(claim []byte) (time.Time, bool) {
	i := bytes.IndexByte(claim, '|')
	if i < 0 {
		return time.Time{}, false
	}
	exp, err := time.Parse(time.RFC3339Nano, string(claim[:i]))
	if err != nil {
		return time.Time{}, false
	}
	return exp, true
}

// waitDuration is one jittered poll interval, never overshooting the claim's
// expiry (so an expired claim is retried promptly) and never below 1ms.
func (m *StoreLease) waitDuration(exp time.Time) time.Duration {
	d := time.Duration(float64(m.poll) * (0.5 + m.rand()))
	if untilExp := exp.Sub(m.now()); untilExp < d {
		d = untilExp
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

// wait blocks for one interval before the next retry. It returns ctx.Err() if
// ctx is done (the caller then gives up), or nil to retry. With a Waker it
// blocks on the cross-process hint; otherwise it sleeps on a timer.
func (m *StoreLease) wait(ctx context.Context, account jmap.Id, d time.Duration) error {
	if m.waker != nil {
		m.waker.AwaitWake(ctx, account, d)
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type storeLease struct {
	m        *StoreLease
	al       *accountLock
	account  jmap.Id
	claimKey []byte
	claim    []byte
	done     bool
}

// Fence implements Lease. The claim token is the fence: a commit applies only
// while this lease's exact token is still the current claim.
func (l *storeLease) Fence(b *backend.Batch) { b.Assert(l.claimKey, l.claim) }

// Release implements Lease. Idempotent.
func (l *storeLease) Release() {
	if l.done {
		return
	}
	l.done = true

	// Rewrite the claim as an already-expired token, but only if it is still
	// ours: if it expired and another instance took over, the compare fails and
	// we leave their claim alone. A released claim then takes the identical
	// takeover path a naturally-expired one does. Every error is swallowed - a
	// failed swap just lets the claim age out via its own expiry, and
	// correctness is the claim fence's job, not this swap's. Release has no way
	// to report an error and must not block a caller on one.
	_, _ = cas(context.Background(), l.m.be, l.claimKey, l.claim, l.m.newExpiredToken())

	if l.m.waker != nil {
		l.m.waker.Wake(l.account)
	}
	l.al.mu.Unlock()
}
