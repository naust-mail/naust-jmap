package websocket

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// CapabilityURI identifies JMAP over WebSocket (RFC 8887 section 3).
const CapabilityURI = "urn:ietf:params:jmap:websocket"

// Handler is the WebSocket endpoint: an http.Handler that upgrades
// authenticated requests to the jmap subprotocol and runs the
// connection. Mount it through the server's capability registrar:
//
//	ws := websocket.NewHandler(srv, authn)
//	err := srv.Capability(websocket.CapabilityURI).
//		Advertise(websocket.SessionCapability(srv.BaseURL(), "/ws", false), struct{}{}).
//		Handle("/ws", ws).
//		Err()
type Handler struct {
	srv   *runtime.Server
	authn auth.Authenticator

	// db and n are set by EnablePush (see push.go); nil means push is
	// not supported on this endpoint.
	db *objectdb.DB
	n  notify.Notifier

	// reauthEvery is non-zero when the authenticator implements no
	// auth.Revoker: the periodic re-authentication loop is then the only
	// way revoked credentials can reach an open connection (see
	// reauth.go). The interval is captured from the ReauthInterval knob
	// at construction.
	reauthEvery time.Duration

	mu     sync.Mutex
	conns  map[*conn]struct{}
	closed bool
}

// NewHandler wires the endpoint to a runtime server and the same
// authenticator the rest of the server uses: the WebSocket handshake
// request authenticates exactly like any HTTP request (RFC 8887
// section 4.1), and the resulting identity holds for the connection's
// life (section 4.2).
func NewHandler(srv *runtime.Server, authn auth.Authenticator) *Handler {
	h := &Handler{srv: srv, authn: authn, conns: map[*conn]struct{}{}}
	if _, ok := authn.(auth.Revoker); !ok {
		// Without a revocation stream, an open connection would outlive
		// its credentials indefinitely; fall back to periodically
		// re-running Authenticate on the stored handshake request. This
		// re-runs whatever Authenticate costs - for a KDF-per-request
		// authenticator that is a real per-connection burn - so
		// implementing auth.Revoker is the better path.
		h.reauthEvery = ReauthInterval
		slog.Warn("websocket: authenticator implements no auth.Revoker; " +
			"falling back to periodic re-authentication of open connections")
	}
	return h
}

// ServeHTTP authenticates, upgrades, and runs one connection; it
// returns when the connection is finished.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ident, err := h.authn.Authenticate(r)
	if err != nil {
		// Failed authentication follows normal HTTP semantics: 401 with
		// the authenticator's challenge (RFC 6455 section 4.2.2 step 2).
		w.Header().Set("WWW-Authenticate", h.challenge())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	nc, brw := accept(w, r)
	if nc == nil {
		return
	}
	rd := frame.NewReader(brw.Reader, MaxMessageSize, MaxFragments)
	c := newConn(h, ident, nc, rd)
	if !h.track(c) {
		// The handler is already shut down; go away immediately.
		c.writeClose(frame.CloseGoingAway, "server shutting down")
		c.abort()
		return
	}
	defer h.untrack(c)

	// A revoked identity kills the connection with 1008 (RFC 8887
	// section 4.2's credential-expiry policy) and cancels its in-flight
	// work through the connection context.
	unregister := h.srv.RegisterConnection(ident.Username, func() {
		c.writeClose(frame.ClosePolicyViolation, "credentials revoked")
		c.abort()
	})
	defer unregister()

	if h.reauthEvery > 0 {
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.reauthLoop(r)
		}()
		// The loop exits when the connection context ends; waiting here
		// keeps every goroutine inside the handler's lifetime.
		defer func() { <-done }()
	}
	c.run()
}

// challenge mirrors the runtime's 401 challenge selection: the
// authenticator's auth.Challenger if implemented, else the Basic
// default (RFC 7235 section 4.1).
func (h *Handler) challenge() string {
	if ch, ok := h.authn.(auth.Challenger); ok {
		return ch.Challenge()
	}
	return `Basic realm="jmap"`
}

func (h *Handler) track(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

func (h *Handler) untrack(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
}

// Shutdown closes every live connection gracefully: 1001 Going Away
// after in-flight requests drain under DrainDeadline (RFC 6455
// section 7.1.1). New connections are refused once it begins.
func (h *Handler) Shutdown() {
	h.mu.Lock()
	h.closed = true
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *conn) {
			defer wg.Done()
			c.shutdown()
		}(c)
	}
	wg.Wait()
}
