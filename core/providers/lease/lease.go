// Package lease defines the per-account writer lease: the concurrency
// half of the runtime's consistency contract. Every write to an account
// happens under that account's lease, and every commit batch carries a
// fencing assertion, so backends need atomic batches but no isolation
// (see package backend).
//
// Manager is a socket: the in-process implementation here is the
// single-node default; cluster deployments plug a store-backed lease
// (e.g. Postgres) with the same interface and fencing semantics.
package lease

import (
	"context"
	"sync"

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
	// applies only if this lease is still the current generation. This
	// is what makes a stalled holder's late writes fail instead of
	// corrupting (the batch model is safe ONLY with this in place).
	Fence(b *backend.Batch)
	// Release gives the lease up. The Lease is unusable afterwards.
	Release()
}

// leaseKey is the backend key holding an account's lease generation.
func leaseKey(account jmap.Id) []byte {
	return []byte("l/" + string(account))
}

// InProcess is the single-node Manager: a mutex per account, with the
// generation counter persisted through the backend so fencing behaves
// identically to cluster implementations.
type InProcess struct {
	be backend.Backend

	mu    sync.Mutex
	locks map[jmap.Id]*accountLock
}

type accountLock struct {
	mu sync.Mutex
}

// NewInProcess returns a Manager for a single process owning be.
func NewInProcess(be backend.Backend) *InProcess {
	return &InProcess{be: be, locks: make(map[jmap.Id]*accountLock)}
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

	// Bump the generation under the mutex; the stored value is what
	// commit batches fence against.
	key := leaseKey(account)
	var bump backend.Batch
	bump.Add(key, 1)
	if err := m.be.WriteBatch(ctx, &bump); err != nil {
		al.mu.Unlock()
		return nil, err
	}
	gen, err := m.be.Get(ctx, key)
	if err != nil {
		al.mu.Unlock()
		return nil, err
	}
	return &inProcessLease{lock: al, key: key, gen: gen}, nil
}

type inProcessLease struct {
	lock *accountLock
	key  []byte
	gen  []byte
	done bool
}

// Fence implements Lease.
func (l *inProcessLease) Fence(b *backend.Batch) { b.Assert(l.key, l.gen) }

// Release implements Lease.
func (l *inProcessLease) Release() {
	if l.done {
		return
	}
	l.done = true
	l.lock.mu.Unlock()
}
