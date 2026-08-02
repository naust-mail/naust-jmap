package websocket

import (
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
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
//	ws := websocket.NewHandler(srv, authn, websocket.Config{})
//	err := srv.Capability(websocket.CapabilityURI).
//		Advertise(websocket.SessionCapability(srv.BaseURL(), "/ws", false), struct{}{}).
//		Handle("/ws", ws).
//		Err()
type Handler struct {
	srv   *runtime.Server
	authn auth.Authenticator

	// origins and anyOrigin are the construction-time origin policy
	// (see Config.Origins). With none stated, selfOrigin (derived
	// from the server's BaseURL) is the whole allowlist: same-origin
	// by default.
	origins    map[string]bool
	anyOrigin  bool
	selfOrigin string

	// db and n are set by EnablePush (see push.go) and guarded by mu;
	// a nil notifier means push is not supported on this endpoint.
	db *objectdb.DB
	n  notify.Notifier

	// reauthEvery is non-zero when the authenticator implements no
	// auth.Revoker (the periodic re-authentication loop is then the only
	// way revoked credentials can reach an open connection, see
	// reauth.go), or when ReauthWithRevoker turns the loop on alongside
	// a Revoker. The interval is captured from the ReauthInterval knob
	// at construction.
	reauthEvery time.Duration

	mu     sync.Mutex
	conns  map[*conn]struct{}
	closed bool
}

// Config is the endpoint's construction-time configuration.
type Config struct {
	// Origins is the browser origin policy. Each entry is an exact
	// web origin (scheme://host, with the port when not the scheme
	// default): a handshake carrying any other Origin is refused
	// with 403 before authentication runs (RFC 6455 section 10.2).
	// The single entry "*" allows every origin, the same spelling
	// and semantics as CORS - correct exactly when no ambient
	// browser credential can authenticate a request, because the
	// browser WebSocket API cannot attach headers: an
	// Authorization-borne credential (a bearer token) is unforgeable
	// by a hostile page, while cookies and Basic realms travel
	// automatically. Nil defaults to same-origin with the server's
	// BaseURL. A handshake with no Origin header always passes -
	// only browsers send one, and a non-browser client controls its
	// own headers anyway.
	Origins []string
}

// NewHandler wires the endpoint to a runtime server and the same
// authenticator the rest of the server uses: the WebSocket handshake
// request authenticates exactly like any HTTP request (RFC 8887
// section 4.1), and the resulting identity holds for the connection's
// life (section 4.2). One HTTP assumption does NOT carry over: the
// handshake is exempt from the browser's same-origin policy, so an
// authenticator honoring ambient credentials (cookies, a Basic realm)
// is reachable by any web page unless origins are restricted (RFC 6455
// section 10.2). A nil Config.Origins therefore defaults to
// same-origin with the server's BaseURL - browser pages on any other
// origin are refused until Config.Origins states a wider policy.
// Non-browser clients send no Origin and always pass.
func NewHandler(srv *runtime.Server, authn auth.Authenticator, cfg Config) *Handler {
	h := &Handler{srv: srv, authn: authn, conns: map[*conn]struct{}{}}
	for _, o := range cfg.Origins {
		if o == "*" {
			h.anyOrigin = true
			continue
		}
		if h.origins == nil {
			h.origins = make(map[string]bool, len(cfg.Origins))
		}
		// Normalized the way the browser serializes the header
		// (default port omitted), so "https://app.example:443"
		// still matches what actually arrives.
		if n := webOrigin(o); n != "" {
			o = n
		}
		h.origins[strings.ToLower(o)] = true
	}
	if !h.anyOrigin && len(h.origins) == 0 {
		if srv != nil {
			h.selfOrigin = webOrigin(srv.BaseURL())
		}
		slog.Debug("websocket: no origin policy stated, defaulting to same-origin "+
			"(WebSocket bypasses CORS; see Config.Origins)",
			"origin", h.selfOrigin)
	}
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
	} else if ReauthWithRevoker {
		h.reauthEvery = ReauthInterval
	}
	return h
}

// originAllowed applies the construction-time origin policy. A request
// with no Origin header always passes - only browsers send one. One
// carrying it passes under "*", on the Config.Origins list, or (with
// no policy stated) equal to the server's own origin; comparison is
// case-insensitive (RFC 6454 section 4 serializes lowercase).
func (h *Handler) originAllowed(r *http.Request) bool {
	if h.anyOrigin {
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	if len(h.origins) != 0 {
		return h.origins[strings.ToLower(o)]
	}
	return h.selfOrigin != "" && strings.EqualFold(o, h.selfOrigin)
}

// allowedOrigins renders the origin policy in effect, for the refusal
// log: the Config.Origins list when one was stated, else the server's
// own origin. Sorted, so the line is stable across refusals.
func (h *Handler) allowedOrigins() string {
	if len(h.origins) == 0 {
		return h.selfOrigin
	}
	list := make([]string, 0, len(h.origins))
	for o := range h.origins {
		list = append(list, o)
	}
	sort.Strings(list)
	return strings.Join(list, " ")
}

// webOrigin reduces a URL to its web origin as a browser would
// serialize it (RFC 6454 section 6.1: lowercase, the port omitted when
// it is the scheme's default); empty when the URL does not parse.
func webOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	host := u.Host
	if p := u.Port(); (u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80") {
		host = strings.TrimSuffix(host, ":"+p)
	}
	return strings.ToLower(u.Scheme + "://" + host)
}

// ServeHTTP authenticates, upgrades, and runs one connection; it
// returns when the connection is finished.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The origin policy runs before authentication: a disallowed page
	// never exercises the authenticator at all (RFC 6455 section 10.2).
	if !h.originAllowed(r) {
		// The refused Origin is not logged: it is unauthenticated input,
		// and it is the policy - not the value that failed it - that a
		// first-party client blocked by a misconfigured origin needs to
		// see.
		slog.Debug("websocket: origin policy refused a handshake", "allowed", h.allowedOrigins())
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
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
	// work through the connection context; the closing write runs off
	// the revocation dispatcher's goroutine (see conn.revoked).
	unregister, err := h.srv.RegisterConnection(ident.Username, c.revoked)
	if err != nil {
		// Per-user connection admission (RFC 8620 section 8.5) happens
		// at registration, after the upgrade: the handshake is already
		// committed, so the refusal is a Close frame the client can
		// read, with 1013 saying a later retry may be admitted.
		c.writeClose(frame.CloseTryAgainLater, "too many open connections for this user")
		c.abort()
		return
	}
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

// track admits a connection into h.conns. Membership is a lifetime
// claim, not just a close list: a connection is in the set exactly
// while its serving goroutine is alive (untrack is the goroutine's
// final action), which is what lets Shutdown wait tracked connections
// all the way out rather than merely closing their sockets.
func (h *Handler) track(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

// untrack ends a tracked connection's membership and marks its
// goroutine finished. It must be the last thing the serving goroutine
// does - the deferred calls that drain request workers and unregister
// from the runtime all run before it.
func (h *Handler) untrack(c *conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
	close(c.done)
}

// Shutdown closes every live connection gracefully: 1001 Going Away
// after in-flight requests drain under DrainDeadline (RFC 6455
// section 7.1.1). New connections are refused once it begins, and it
// returns only after every tracked connection's serving goroutine has
// exited - afterwards no goroutine of the handler's remains.
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
	// The close handshakes above are bounded by DrainDeadline and
	// CloseReplyDeadline; once each socket is down, its goroutine's
	// remaining teardown is quick, so this wait is short and unbounded
	// on purpose - a deadline here would just turn a teardown bug into
	// a silent leak.
	for _, c := range conns {
		<-c.done
	}
}
