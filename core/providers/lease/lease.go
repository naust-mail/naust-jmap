// Package lease defines the per-account writer lease: the concurrency
// half of the runtime's consistency contract. Every write to an account
// happens under that account's lease, and every commit batch carries a
// fencing assertion, so backends need atomic batches but no isolation
// (see package backend).
//
// Manager is a provider interface: the in-process implementation here is the
// single-node default; cluster deployments plug a store-backed lease
// (e.g. Postgres) with the same interface and fencing semantics.
//
// An implementation is correct when it passes leasetest.Run, the shared
// contract suite both the in-process and store-backed managers satisfy.
package lease

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// Manager grants exclusive per-account write leases.
type Manager interface {
	// Acquire blocks until the caller holds the account's lease or ctx
	// is done. Leases are held per mutating method call, not per HTTP
	// request (RFC 8620 section 3.10 lets concurrent requests interleave
	// between method calls).
	Acquire(ctx context.Context, account jmap.Id) (Lease, error)
}

// Lease is a held per-account write lease.
type Lease interface {
	// Fence appends the fencing assertion to a commit batch: the batch
	// applies only if this lease's claim token is still current. This
	// is what makes a stalled holder's late writes fail instead of
	// corrupting (the batch model is safe ONLY with this in place).
	Fence(b *backend.Batch)
	// Release gives the lease up. The Lease is unusable afterwards.
	Release()
}

// inProcessExpiry is the deadline written into claims minted by InProcess. It
// never gates InProcess itself (the sole-process assumption makes every
// existing claim immediately stealable); it only shapes how long a store-lease
// instance would wait before taking over an account this manager abandoned.
const inProcessExpiry = 15 * time.Second

// InProcess is the single-node Manager: a mutex per account serializes local
// callers, and the account's claim record in the backend - the same key,
// token format, and fence StoreLease uses - carries the fence every commit
// asserts.
//
// The two managers differ only in policy. InProcess assumes it is the sole
// process writing this store, so it takes over any existing claim immediately
// instead of waiting out an expiry: a restart after a crash reclaims every
// account at once. Because both managers speak one claim mechanism, that
// assumption failing cannot corrupt anything - a store-lease instance's
// takeover replaces this manager's token and its next fence fails cleanly,
// and vice versa. It does surface: a claim swapped out from under this
// manager, or a foreign token reappearing on an account it already reclaimed,
// is proof of a live concurrent writer (a dead predecessor cannot mint new
// claims), and InProcess logs it loudly - the deliberate sole log site in
// core, because a silent misconfiguration detector detects nothing.
type InProcess struct {
	be backend.Backend

	now   func() time.Time // test seam; time.Now outside tests
	nonce string           // per-instance random claim-value base
	seq   atomic.Uint64    // per-mint counter, keeps every claim unique
	// warnForeign reports a proven live concurrent writer; a test seam over
	// the slog default.
	warnForeign func(account jmap.Id, evidence string)

	mu    sync.Mutex
	locks map[jmap.Id]*accountLock
}

type accountLock struct {
	mu sync.Mutex
	// token is the claim this InProcess manager last wrote for the account,
	// still in the store (Release leaves it in place; there is no
	// cross-process waiter to free). It makes the steady-state acquire a
	// single compare-and-swap against a known value, and any failure of that
	// swap is proof of a live concurrent writer - the sole process's claims
	// cannot otherwise change. Guarded by mu. Unused by StoreLease, whose
	// claims other instances may legitimately replace.
	token []byte
}

// NewInProcess returns a Manager for a single process owning be.
func NewInProcess(be backend.Backend) *InProcess {
	return &InProcess{
		be:    be,
		now:   time.Now,
		nonce: newNonce(),
		warnForeign: func(account jmap.Id, evidence string) {
			slog.Warn("lease: another process is writing this store while InProcess assumes exclusivity; run one manager type per store",
				"account", string(account), "evidence", evidence)
		},
		locks: make(map[jmap.Id]*accountLock),
	}
}

// Acquire implements Manager.
func (m *InProcess) Acquire(ctx context.Context, account jmap.Id) (Lease, error) {
	m.mu.Lock()
	al, ok := m.locks[account]
	if !ok {
		al = &accountLock{}
		m.locks[account] = al
	}
	m.mu.Unlock()

	locked := make(chan struct{})
	go func() {
		al.mu.Lock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-ctx.Done():
		// The goroutine will still take the mutex; hand it straight back.
		go func() {
			<-locked
			al.mu.Unlock()
		}()
		return nil, ctx.Err()
	}

	key := storeClaimKey(account)
	claim := mintToken(m.nonce, &m.seq, m.now().Add(inProcessExpiry))

	// Steady state: swap directly against the claim this manager last wrote -
	// one store round trip. The sole-process assumption means nothing else can
	// have changed it, so a failed swap is itself the evidence of a live
	// foreign writer and falls through to the full read path.
	if al.token != nil {
		switch swapped, err := cas(ctx, m.be, key, al.token, claim); {
		case err != nil:
			al.mu.Unlock()
			return nil, err
		case swapped:
			al.token = claim
			return &inProcessLease{al: al, key: key, claim: claim}, nil
		}
		m.warnForeign(account, "cached claim was replaced by another writer")
	}

	// First contact with this account, or the fast path just lost its swap:
	// read whatever claim is present and take it over immediately, regardless
	// of expiry or owner - a predecessor's leftover is dead by assumption, and
	// a live owner's next fence will fail cleanly rather than corrupt.
	for {
		old, err := getClaim(ctx, m.be, key)
		if err != nil {
			al.mu.Unlock()
			return nil, err
		}
		swapped, err := cas(ctx, m.be, key, old, claim)
		if err != nil {
			al.mu.Unlock()
			return nil, err
		}
		if swapped {
			al.token = claim
			return &inProcessLease{al: al, key: key, claim: claim}, nil
		}
		// A dead predecessor cannot swap a claim; losing this race proves a
		// live concurrent writer.
		m.warnForeign(account, "claim changed between read and takeover")
	}
}

type inProcessLease struct {
	al    *accountLock
	key   []byte
	claim []byte
	done  bool
}

// Fence implements Lease. The claim token is the fence, exactly as in
// StoreLease: the commit applies only while this token is still the current
// claim.
func (l *inProcessLease) Fence(b *backend.Batch) { b.Assert(l.key, l.claim) }

// Release implements Lease. It frees only the local mutex: the claim stays in
// the store for the next acquire's single-swap fast path, and by assumption no
// other process is waiting on it.
func (l *inProcessLease) Release() {
	if l.done {
		return
	}
	l.done = true
	l.al.mu.Unlock()
}
