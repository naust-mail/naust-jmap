package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"

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

	apiSlots chan struct{}
	// blobs is non-nil once EnableBlobs is called (section 6).
	blobs *blobSupport
	// push is non-nil once EnablePush is called (section 7).
	push *pushSupport
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
		log.Printf("naust-jmap: tuning: %s", warning)
	}
	return &Server{
		authn:       a,
		proc:        p,
		baseURL:     strings.TrimSuffix(baseURL, "/"),
		core:        core,
		sessionCaps: map[string]json.RawMessage{jmap.CoreCapability: coreJSON},
		accountCaps: map[string]json.RawMessage{},
		apiSlots:    make(chan struct{}, core.MaxConcurrentRequests),
	}, nil
}

// RegisterCapability advertises a non-core capability: sessionValue
// appears in the session capabilities object, accountValue in every
// account's accountCapabilities. The capability becomes valid in
// requests' "using" arrays.
func (s *Server) RegisterCapability(uri string, sessionValue, accountValue any) error {
	sv, err := json.Marshal(sessionValue)
	if err != nil {
		return err
	}
	av, err := json.Marshal(accountValue)
	if err != nil {
		return err
	}
	s.sessionCaps[uri] = sv
	s.accountCaps[uri] = av
	s.capOrder = append(s.capOrder, uri)
	s.proc.capabilities[uri] = true
	return nil
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
	select {
	case s.apiSlots <- struct{}{}:
		defer func() { <-s.apiSlots }()
	default:
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusTooManyRequests,
			Limit:  "maxConcurrentRequests",
			Detail: "too many concurrent API requests",
		})
		return
	}
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
	if err := jmap.CheckIJSON(body); err != nil {
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemNotJSON, Status: http.StatusBadRequest,
			Detail: err.Error(),
		})
		return
	}
	req, err := jmap.ParseRequest(body)
	if err != nil {
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemNotRequest, Status: http.StatusBadRequest,
			Detail: err.Error(),
		})
		return
	}
	if rerr := s.proc.CheckUsing(req); rerr != nil {
		writeProblem(w, *rerr)
		return
	}
	if int64(len(req.MethodCalls)) > s.core.MaxCallsInRequest {
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusBadRequest,
			Limit:  "maxCallsInRequest",
			Detail: fmt.Sprintf("%d method calls exceeds maxCallsInRequest", len(req.MethodCalls)),
		})
		return
	}
	resp := s.proc.Process(r.Context(), req, ident, s.session(ident).State)
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
