package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// DefaultCoreCapabilities returns the suggested minimum limits of
// RFC 8620 section 2, with the collations this runtime implements.
func DefaultCoreCapabilities() jmap.CoreCapabilities {
	return jmap.CoreCapabilities{
		MaxSizeUpload:         50_000_000,
		MaxConcurrentUpload:   4,
		MaxSizeRequest:        10_000_000,
		MaxConcurrentRequests: 4,
		MaxCallsInRequest:     16,
		MaxObjectsInGet:       500,
		MaxObjectsInSet:       500,
		CollationAlgorithms:   []string{"i;ascii-numeric", "i;ascii-casemap"},
	}
}

// Server is the HTTP face of the runtime: the session resource, the API
// endpoint, and (from M1) the binary and push endpoints. It assumes TLS
// is terminated by the embedder (RFC 8620 section 8.1 requires TLS on
// the wire; the library has no opinion on where).
type Server struct {
	authn auth.Authenticator
	proc  *Processor
	// baseURL is the external URL prefix used to build session URLs,
	// e.g. "https://jmap.example.com".
	baseURL string
	core    jmap.CoreCapabilities

	sessionCaps map[string]json.RawMessage
	accountCaps map[string]json.RawMessage
	capOrder    []string

	// apiSlots is the shared maxConcurrentRequests pool (section 2); it
	// bounds requests in flight across every transport, not per endpoint.
	// userMu/userSlots/perUserSlots add the per-user share of that pool
	// (see pipeline.go).
	apiSlots     chan struct{}
	userMu       sync.Mutex
	userSlots    map[string]*userSlots
	perUserSlots int
	// blobs is non-nil once EnableBlobs is called (section 6).
	blobs *blobSupport
	// push is non-nil once EnablePush is called (section 7).
	push *pushSupport

	// connMu/conns index live long-lived connections per username so a
	// credential revocation can close them (see revoke.go).
	connMu sync.Mutex
	conns  map[string]map[*connEntry]struct{}

	// routes holds capability-registered HTTP endpoints (Registration.Handle).
	// A key ending in "/" matches by prefix, otherwise exactly - the same
	// two shapes the built-in endpoints use. Written only during
	// registration, read without locking by ServeHTTP: all registration
	// must complete before the server starts serving.
	routes map[string]http.Handler
}

// NewServer wires an authenticator and processor into an http.Handler.
func NewServer(a auth.Authenticator, p *Processor, baseURL string, core jmap.CoreCapabilities) (*Server, error) {
	coreJSON, err := json.Marshal(core)
	if err != nil {
		return nil, err
	}
	if core.MaxConcurrentRequests < 1 || core.MaxCallsInRequest < 1 || core.MaxSizeRequest < 1 {
		return nil, errors.New("runtime: core limits must be positive (RFC 8620 section 8.5 requires enforced limits)")
	}
	for _, warning := range tuning.Validate() {
		slog.Warn("naust-jmap: tuning", "warning", warning)
	}
	// Per-user share of the request pool: the tuning override, or half
	// the pool (floored at one) so no single user can hold every slot.
	perUser := tuning.MaxConcurrentRequestsPerUser
	if perUser <= 0 {
		perUser = max(1, int(core.MaxConcurrentRequests)/2)
	}
	s := &Server{
		authn:        a,
		proc:         p,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		core:         core,
		sessionCaps:  map[string]json.RawMessage{jmap.CoreCapability: coreJSON},
		accountCaps:  map[string]json.RawMessage{},
		apiSlots:     make(chan struct{}, core.MaxConcurrentRequests),
		userSlots:    map[string]*userSlots{},
		perUserSlots: perUser,
		routes:       map[string]http.Handler{},
	}
	// A revocation-capable authenticator gets one subscriber for the
	// server's life: revoked identities' live connections are closed the
	// moment the event arrives (see revoke.go and auth.Revoker).
	if rv, ok := a.(auth.Revoker); ok {
		go s.watchRevocations(rv.Revocations(context.Background()))
	}
	return s, nil
}

// BaseURL returns the external URL prefix the server was constructed
// with (trailing slash trimmed), e.g. "https://jmap.example.com". It is
// the base from which the session object's endpoint URLs are built;
// registered capabilities can derive their own advertised URLs from it.
func (s *Server) BaseURL() string { return s.baseURL }

// Registration accumulates everything one capability contributes to the
// server: its entries in the session object (RFC 8620 section 2), its
// methods, and any HTTP endpoints of its own. Obtained from
// Server.Capability; calls chain, and the first error sticks - check
// Err once after the chain.
type Registration struct {
	s   *Server
	uri string
	err error
}

// Capability starts registering the capability identified by uri.
// Registration is not safe for use concurrently with serving: complete
// all registration before the server starts handling requests.
func (s *Server) Capability(uri string) *Registration {
	return &Registration{s: s, uri: uri}
}

// Advertise puts the capability in the session object: sessionValue
// under its URI in the capabilities object, accountValue in every
// account's accountCapabilities (RFC 8620 section 2). The capability
// also becomes valid in requests' "using" arrays.
func (r *Registration) Advertise(sessionValue, accountValue any) *Registration {
	if r.err != nil {
		return r
	}
	sv, err := json.Marshal(sessionValue)
	if err != nil {
		r.err = fmt.Errorf("runtime: capability %s: session value: %w", r.uri, err)
		return r
	}
	av, err := json.Marshal(accountValue)
	if err != nil {
		r.err = fmt.Errorf("runtime: capability %s: account value: %w", r.uri, err)
		return r
	}
	s := r.s
	s.sessionCaps[r.uri] = sv
	s.accountCaps[r.uri] = av
	s.capOrder = append(s.capOrder, r.uri)
	s.proc.capabilities[r.uri] = true
	return r
}

// Method registers a method under this capability: it becomes callable
// only in requests whose "using" array includes the capability's URI
// (RFC 8620 section 3.3).
func (r *Registration) Method(name string, h Handler) *Registration {
	if r.err != nil {
		return r
	}
	r.s.proc.Register(name, r.uri, h)
	return r
}

// Handle mounts an HTTP endpoint for this capability under the server's
// base URL. A path ending in "/" matches by prefix, otherwise exactly.
// Paths that are, or would shadow, the built-in JMAP endpoints
// (/.well-known/jmap, /api, /upload/, /download/, /eventsource) are
// rejected, as are duplicates.
func (r *Registration) Handle(path string, h http.Handler) *Registration {
	if r.err != nil {
		return r
	}
	if !strings.HasPrefix(path, "/") || path == "/" {
		r.err = fmt.Errorf("runtime: capability %s: path %q must start with / and not be the root", r.uri, path)
		return r
	}
	if err := checkReservedPath(path); err != nil {
		r.err = fmt.Errorf("runtime: capability %s: %w", r.uri, err)
		return r
	}
	if _, dup := r.s.routes[path]; dup {
		r.err = fmt.Errorf("runtime: capability %s: path %q already registered", r.uri, path)
		return r
	}
	r.s.routes[path] = h
	return r
}

// Err returns the first error the registration chain hit, or nil.
func (r *Registration) Err() error { return r.err }

// reservedExact and reservedPrefixes are the built-in JMAP endpoints
// (RFC 8620 sections 2, 3.4, 6.1, 6.2, 7.3); capability endpoints may
// not collide with them.
var (
	reservedExact    = []string{"/.well-known/jmap", "/api", "/eventsource"}
	reservedPrefixes = []string{"/upload/", "/download/"}
)

func checkReservedPath(path string) error {
	prefixPattern := strings.HasSuffix(path, "/")
	for _, res := range reservedExact {
		if path == res || (prefixPattern && strings.HasPrefix(res, path)) {
			return fmt.Errorf("path %q collides with built-in endpoint %s", path, res)
		}
	}
	for _, res := range reservedPrefixes {
		if strings.HasPrefix(path, res) || strings.HasPrefix(res, path) {
			return fmt.Errorf("path %q collides with built-in endpoint %s", path, res)
		}
	}
	return nil
}

// route finds a capability endpoint for an incoming path: an exact
// entry wins, else the longest matching prefix entry.
func (s *Server) route(path string) http.Handler {
	if h, ok := s.routes[path]; ok {
		return h
	}
	var best string
	var bestH http.Handler
	for pattern, h := range s.routes {
		if strings.HasSuffix(pattern, "/") && strings.HasPrefix(path, pattern) && len(pattern) > len(best) {
			best, bestH = pattern, h
		}
	}
	return bestH
}

// ServeHTTP routes the JMAP endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/jmap":
		s.handleSession(w, r)
	case r.URL.Path == "/api":
		s.handleAPI(w, r)
	case strings.HasPrefix(r.URL.Path, "/upload/"):
		s.handleUpload(w, r)
	case strings.HasPrefix(r.URL.Path, "/download/"):
		s.handleDownload(w, r)
	case r.URL.Path == "/eventsource":
		s.handleEventSource(w, r)
	default:
		if h := s.route(r.URL.Path); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) *auth.Identity {
	ident, err := s.authn.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", s.challenge())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	return ident
}

// challenge returns the WWW-Authenticate value for a failed authentication,
// per the authenticator's auth.Challenger if it implements one, else the
// "Basic" default (RFC 7235 section 4.1).
func (s *Server) challenge() string {
	if c, ok := s.authn.(auth.Challenger); ok {
		return c.Challenge()
	}
	return `Basic realm="jmap"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ident := s.authenticate(w, r)
	if ident == nil {
		return
	}
	session := s.session(ident)
	// Session caching is done via sessionState comparison, not HTTP
	// caches (section 2).
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(session); err != nil {
		http.Error(w, "encoding failed", http.StatusInternalServerError)
	}
}

// session builds the RFC 8620 section 2 Session object for an identity.
func (s *Server) session(ident *auth.Identity) *jmap.Session {
	accounts := make(map[jmap.Id]jmap.Account, len(ident.Accounts))
	for id, acc := range ident.Accounts {
		accounts[id] = jmap.Account{
			Name:                acc.Name,
			IsPersonal:          acc.Personal,
			IsReadOnly:          acc.ReadOnly,
			AccountCapabilities: s.accountCaps,
		}
	}
	// Core SHOULD NOT appear in primaryAccounts (section 2).
	primary := make(map[string]jmap.Id, len(s.capOrder))
	if ident.Primary != "" {
		for _, uri := range s.capOrder {
			primary[uri] = ident.Primary
		}
	}
	session := &jmap.Session{
		Capabilities:    s.sessionCaps,
		Accounts:        accounts,
		PrimaryAccounts: primary,
		Username:        ident.Username,
		APIURL:          s.baseURL + "/api",
		DownloadURL:     s.baseURL + "/download/{accountId}/{blobId}/{name}?accept={type}",
		UploadURL:       s.baseURL + "/upload/{accountId}/",
		EventSourceURL:  s.baseURL + "/eventsource?types={types}&closeafter={closeafter}&ping={ping}",
	}
	session.State = sessionStateOf(session)
	return session
}

// sessionStateOf derives the session state string: it must change
// whenever any other session property changes (section 2).
func sessionStateOf(session *jmap.Session) string {
	withoutState := *session
	withoutState.State = ""
	blob, err := json.Marshal(&withoutState)
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:6])
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ident := s.authenticate(w, r)
	if ident == nil {
		return
	}
	slot := s.TryAcquireSlot(ident)
	if slot == nil {
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusTooManyRequests,
			Limit:  "maxConcurrentRequests",
			Detail: "too many concurrent API requests",
		})
		return
	}
	defer slot.Release()
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if media, _, err := mime.ParseMediaType(ct); err != nil || media != "application/json" {
			writeProblem(w, jmap.RequestError{
				Type: jmap.ProblemNotJSON, Status: http.StatusBadRequest,
				Detail: "content type must be application/json",
			})
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.core.MaxSizeRequest))
	if err != nil {
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusBadRequest,
			Limit:  "maxSizeRequest",
			Detail: "request body exceeds maxSizeRequest",
		})
		return
	}
	resp, rerr := slot.Process(r.Context(), body)
	if rerr != nil {
		writeProblem(w, *rerr)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// WriteJSON, not json.NewEncoder(w).Encode(resp): the response body is
	// almost entirely already-compact JSON assembled by reply()/
	// ErrorInvocation (see their comments), so re-marshaling through
	// reflection would re-validate and re-compact content that has already
	// passed through encoding/json once. See core/jmap/compact.go.
	if err := resp.WriteJSON(w); err != nil {
		http.Error(w, "encoding failed", http.StatusInternalServerError)
	}
}

func writeProblem(w http.ResponseWriter, p jmap.RequestError) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
