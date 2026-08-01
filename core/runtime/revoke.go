package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// Long-lived connections (EventSource streams, and any streaming
// transport a capability mounts) authenticate once at establishment,
// so credential revocation needs a push path: the server keeps a
// per-username index of live connections, and when the authenticator
// implements auth.Revoker, a revocation closes every connection that
// identity holds and cancels its in-flight work. See auth.Revoker for
// the contract.

// revocationClockSlack widens the "established before the revocation"
// window when deciding which connections a revocation kills. The
// revocation's At and a connection's registration time can come from
// different clocks (a database's vs this host's), so the comparison is
// biased toward closing: a connection registered up to this long AFTER
// At is still closed. Skew inside the slack costs only a bounded
// re-bounce of a freshly re-authenticated connection (At is fixed while
// reconnect times advance, so it stops matching); skew beyond it is the
// documented residual. Wall-clock time is a liveness bias here, never a
// safety mechanism - same doctrine as the store lease.
const revocationClockSlack = 2 * time.Minute

// connEntry is one registered live connection; close tears it down.
// A distinct heap object per registration so it can key the index.
type connEntry struct {
	close func()
	// at is when the connection registered, i.e. an upper bound on when
	// it authenticated; compared against Revocation.At.
	at time.Time
}

// ErrTooManyConnections reports that registering one more connection
// would put the username over tuning.MaxConnectionsPerUser.
var ErrTooManyConnections = errors.New("too many connections for this user")

// RegisterConnection indexes a live long-lived connection owned by
// username. If the username's credentials are revoked while the
// connection is registered, close is called (once, on the revocation
// goroutine - it must not block). The returned function removes the
// registration and is safe to call multiple times and concurrently
// with a revocation; every connection must unregister when it ends.
//
// Registration is also the admission point: it fails with
// ErrTooManyConnections when the username already holds
// tuning.MaxConnectionsPerUser registered connections in this process.
// Each long-lived connection occupies a socket, a goroutine, and a push
// subscription for its whole lifetime, so an uncounted transport would
// let one authenticated user exhaust the process (RFC 8620 section
// 8.5's denial-of-service guidance); counting here covers every
// transport with one check. On failure the caller must turn the
// connection away and close must never be called.
func (s *Server) RegisterConnection(username string, close func()) (unregister func(), err error) {
	e := &connEntry{close: close, at: time.Now()}
	s.connMu.Lock()
	if s.conns == nil {
		s.conns = map[string]map[*connEntry]struct{}{}
	}
	set := s.conns[username]
	if max := tuning.MaxConnectionsPerUser; max > 0 && len(set) >= max {
		s.connMu.Unlock()
		return nil, ErrTooManyConnections
	}
	if set == nil {
		set = map[*connEntry]struct{}{}
		s.conns[username] = set
	}
	set[e] = struct{}{}
	s.connMu.Unlock()
	return func() {
		s.connMu.Lock()
		defer s.connMu.Unlock()
		set := s.conns[username]
		delete(set, e)
		if len(set) == 0 {
			delete(s.conns, username)
		}
	}, nil
}

// watchRevocations consumes the authenticator's revocation stream until
// the stream closes or the server's lifetime ends; NewServer starts it
// when the authenticator implements auth.Revoker. The ctx arm keeps
// Close from depending on the implementation honoring auth.Revoker's
// close-the-channel-on-ctx-end contract.
func (s *Server) watchRevocations(ctx context.Context, events <-chan auth.Revocation) {
	defer close(s.watchDone)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.revokeConnections(ev.Username, ev.At)
		case <-ctx.Done():
			return
		}
	}
}

// revokeConnections closes the username's registered connections that
// predate the revocation (within revocationClockSlack). Connections
// established after that were authenticated against post-revocation
// credential state and survive - this is what makes redelivery of the
// same event safe (see auth.Revoker). Matching entries are detached
// from the index under the lock, then closed outside it, so a close
// callback may itself call its unregister.
func (s *Server) revokeConnections(username string, at time.Time) {
	cutoff := at.Add(revocationClockSlack)
	var hit []*connEntry
	s.connMu.Lock()
	set := s.conns[username]
	for e := range set {
		if e.at.Before(cutoff) {
			delete(set, e)
			hit = append(hit, e)
		}
	}
	if len(set) == 0 {
		delete(s.conns, username)
	}
	s.connMu.Unlock()
	for _, e := range hit {
		e.close()
	}
}
