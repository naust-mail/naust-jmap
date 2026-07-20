package notify

import (
	"context"
	"sync"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// InProcess is the single-node Notifier: direct fan-out to in-memory
// subscriptions, no persistence. Publish never blocks on subscribers -
// pending changes merge under a mutex and Wait picks them up.
type InProcess struct {
	mu   sync.Mutex
	subs map[*inProcessSub]struct{}
}

// NewInProcess returns an empty in-process Notifier.
func NewInProcess() *InProcess {
	return &InProcess{subs: make(map[*inProcessSub]struct{})}
}

// Publish implements Notifier.
func (n *InProcess) Publish(_ context.Context, account jmap.Id, types jmap.TypeState) {
	if len(types) == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for sub := range n.subs {
		sub.add(account, types)
	}
}

// Subscribe implements Notifier. An empty or nil accounts set matches
// nothing, per the interface contract.
func (n *InProcess) Subscribe(_ context.Context, accounts []jmap.Id) (Subscription, error) {
	sub := &inProcessSub{
		parent:   n,
		accounts: make(map[jmap.Id]bool, len(accounts)),
		pending:  make(Changes),
		wake:     make(chan struct{}, 1),
	}
	for _, a := range accounts {
		sub.accounts[a] = true
	}
	n.mu.Lock()
	n.subs[sub] = struct{}{}
	n.mu.Unlock()
	return sub, nil
}

// SubscribeAll implements Notifier: the explicit firehose.
func (n *InProcess) SubscribeAll(_ context.Context) (Subscription, error) {
	sub := &inProcessSub{
		parent:  n,
		pending: make(Changes),
		wake:    make(chan struct{}, 1),
	}
	n.mu.Lock()
	n.subs[sub] = struct{}{}
	n.mu.Unlock()
	return sub, nil
}

type inProcessSub struct {
	parent *InProcess
	// accounts filters add; nil means every account (only SubscribeAll
	// constructs that).
	accounts map[jmap.Id]bool

	mu      sync.Mutex
	closed  bool
	pending Changes
	// wake has capacity 1: a signal raised while the subscriber is
	// between Wait calls is retained, never lost.
	wake chan struct{}
}

// add merges one commit's changes into the pending set. States are
// monotonic, so for a type already pending the newest state wins.
func (s *inProcessSub) add(account jmap.Id, types jmap.TypeState) {
	if s.accounts != nil && !s.accounts[account] {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	ts := s.pending[account]
	if ts == nil {
		ts = make(jmap.TypeState, len(types))
		s.pending[account] = ts
	}
	for name, state := range types {
		ts[name] = state
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Wait implements Subscription.
func (s *inProcessSub) Wait(ctx context.Context) (Changes, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		if len(s.pending) > 0 {
			got := s.pending
			s.pending = make(Changes)
			s.mu.Unlock()
			return got, nil
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Close implements Subscription. Idempotent.
func (s *inProcessSub) Close() {
	s.parent.mu.Lock()
	delete(s.parent.subs, s)
	s.parent.mu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
