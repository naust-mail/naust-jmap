package runtime

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

// The request pipeline is the transport-agnostic half of the API
// endpoint: everything RFC 8620 section 3.1/3.2 requires of a request
// after the bytes and the authenticated identity are in hand, plus the
// concurrency limits of section 2 that apply to the server as a whole,
// not to one transport. Any transport (the built-in HTTP endpoint, a
// WebSocket connection, ...) first acquires a RequestSlot - one unit of
// the shared maxConcurrentRequests pool - and then runs bodies through
// Process while holding it. Slots are handed out with a per-user bound
// as well as the pool bound, so one user cannot occupy the entire pool
// and starve every other user of the server (section 8.5).

// RequestSlot is one held execution slot: possession is the right to
// run one request. Obtained from AcquireSlot or TryAcquireSlot; must be
// Released exactly once when the request finishes (Release is
// idempotent, so releasing on every exit path is safe).
type RequestSlot struct {
	s       *Server
	ident   *auth.Identity
	userSem chan struct{}
	release sync.Once
}

// userSlots is one username's semaphore plus a count of goroutines
// currently holding or waiting on it, so the map entry can be dropped
// the moment the last one leaves.
type userSlots struct {
	ch   chan struct{}
	refs int
}

// userSem returns the semaphore for a username, creating it on first
// use and counting the caller as a user of it until userDone.
func (s *Server) userSem(username string) chan struct{} {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	e := s.userSlots[username]
	if e == nil {
		e = &userSlots{ch: make(chan struct{}, s.perUserSlots)}
		s.userSlots[username] = e
	}
	e.refs++
	return e.ch
}

func (s *Server) userDone(username string) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	e := s.userSlots[username]
	e.refs--
	if e.refs == 0 {
		delete(s.userSlots, username)
	}
}

// AcquireSlot blocks until the identity may start a request - a slot is
// free in both its user's share and the shared pool - or ctx is done.
// This is the mode for transports with their own flow control: a
// connection that cannot start a request simply does not read the next
// one, and the peer experiences backpressure instead of an error.
func (s *Server) AcquireSlot(ctx context.Context, ident *auth.Identity) (*RequestSlot, error) {
	// An already-done context never acquires: without this check a free
	// slot and a done context are both ready cases and the select below
	// could pick either.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sem := s.userSem(ident.Username)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		s.userDone(ident.Username)
		return nil, ctx.Err()
	}
	select {
	case s.apiSlots <- struct{}{}:
	case <-ctx.Done():
		<-sem
		s.userDone(ident.Username)
		return nil, ctx.Err()
	}
	// Both a free slot and a done context can be ready at once and the
	// selects pick randomly; a caller with a dead context must not walk
	// away holding a slot.
	if err := ctx.Err(); err != nil {
		<-s.apiSlots
		<-sem
		s.userDone(ident.Username)
		return nil, err
	}
	return &RequestSlot{s: s, ident: ident, userSem: sem}, nil
}

// TryAcquireSlot is AcquireSlot without waiting: nil means the user's
// share or the pool is exhausted right now. This is the mode for plain
// HTTP, where the client gets an immediate 429 rather than a stalled
// connection.
func (s *Server) TryAcquireSlot(ident *auth.Identity) *RequestSlot {
	sem := s.userSem(ident.Username)
	select {
	case sem <- struct{}{}:
	default:
		s.userDone(ident.Username)
		return nil
	}
	select {
	case s.apiSlots <- struct{}{}:
	default:
		<-sem
		s.userDone(ident.Username)
		return nil
	}
	return &RequestSlot{s: s, ident: ident, userSem: sem}
}

// Release returns the slot to the pool and the user's share. Safe to
// call more than once; only the first call releases.
func (r *RequestSlot) Release() {
	r.release.Do(func() {
		<-r.s.apiSlots
		<-r.userSem
		r.s.userDone(r.ident.Username)
	})
}

// Process runs one request body through the full RFC 8620 pipeline for
// the slot's identity: size and I-JSON checks (section 3.1's
// application/json object), request parsing, "using" validation
// (section 3.2), the maxCallsInRequest limit (section 2), and method
// execution stamped with the identity's current sessionState (section
// 3.4). A non-nil *jmap.RequestError is a request-level error per
// section 3.6.1; its Status carries the matching HTTP status code for
// transports that have one.
func (r *RequestSlot) Process(ctx context.Context, body []byte) (*jmap.Response, *jmap.RequestError) {
	s := r.s
	if int64(len(body)) > s.core.MaxSizeRequest {
		return nil, &jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusBadRequest,
			Limit:  "maxSizeRequest",
			Detail: "request body exceeds maxSizeRequest",
		}
	}
	if err := jmap.CheckIJSON(body); err != nil {
		return nil, &jmap.RequestError{
			Type: jmap.ProblemNotJSON, Status: http.StatusBadRequest,
			Detail: err.Error(),
		}
	}
	req, err := jmap.ParseRequest(body)
	if err != nil {
		return nil, &jmap.RequestError{
			Type: jmap.ProblemNotRequest, Status: http.StatusBadRequest,
			Detail: err.Error(),
		}
	}
	if rerr := s.proc.CheckUsing(req); rerr != nil {
		return nil, rerr
	}
	if int64(len(req.MethodCalls)) > s.core.MaxCallsInRequest {
		return nil, &jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusBadRequest,
			Limit:  "maxCallsInRequest",
			Detail: fmt.Sprintf("%d method calls exceeds maxCallsInRequest", len(req.MethodCalls)),
		}
	}
	return s.proc.Process(ctx, req, r.ident, s.session(r.ident).State), nil
}
