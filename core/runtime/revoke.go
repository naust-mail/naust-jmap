package runtime

// Long-lived connections (EventSource streams, and any streaming
// transport a capability mounts) authenticate once at establishment,
// so credential revocation needs a push path: the server keeps a
// per-username index of live connections, and when the authenticator
// implements auth.Revoker, a revocation closes every connection that
// identity holds and cancels its in-flight work. See auth.Revoker for
// the contract.

// connEntry is one registered live connection; close tears it down.
// A distinct heap object per registration so it can key the index.
type connEntry struct {
	close func()
}

// RegisterConnection indexes a live long-lived connection owned by
// username. If the username's credentials are revoked while the
// connection is registered, close is called (once, on the revocation
// goroutine - it must not block). The returned function removes the
// registration and is safe to call multiple times and concurrently
// with a revocation; every connection must unregister when it ends.
func (s *Server) RegisterConnection(username string, close func()) (unregister func()) {
	e := &connEntry{close: close}
	s.connMu.Lock()
	if s.conns == nil {
		s.conns = map[string]map[*connEntry]struct{}{}
	}
	set := s.conns[username]
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
	}
}

// watchRevocations consumes the authenticator's revocation stream for
// the life of the server; NewServer starts it when the authenticator
// implements auth.Revoker.
func (s *Server) watchRevocations(events <-chan string) {
	for username := range events {
		s.revokeConnections(username)
	}
}

// revokeConnections closes every registered connection of a username.
// Entries are detached from the index under the lock, then closed
// outside it, so a close callback may itself call its unregister.
func (s *Server) revokeConnections(username string) {
	s.connMu.Lock()
	set := s.conns[username]
	delete(s.conns, username)
	s.connMu.Unlock()
	for e := range set {
		e.close()
	}
}
