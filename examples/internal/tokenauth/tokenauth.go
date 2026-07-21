// Package tokenauth is the production-shaped reference auth.Authenticator
// for the mailserver example. It models the split every serious deployment
// makes between two genuinely different operations:
//
//   - Authenticate a credential: expensive, rare. Runs the password KDF
//     (argon2id). Happens ONCE, at the LoginHandler endpoint.
//   - Authorize a request: cheap, per request. Verifies that the caller
//     holds a token. This is all the runtime's per-request path does.
//
// The runtime calls Authenticate on EVERY request (each /api batch, each
// blob upload, each event-source reconnect). Running a password KDF there
// would multiply its cost across all of them. A bearer token is the right
// data structure: a cheap-to-verify capability minted once from an
// expensive-to-verify credential. Because the token is 256 bits of
// randomness, a single fast hash plus a map lookup is a cryptographically
// sufficient check on the hot path - there is nothing low-entropy left to
// brute-force, so no KDF belongs here.
//
// What a real deployment adds and this omits to stay small: token expiry,
// rotation and revocation; a persistent token store (these live in memory,
// so a restart logs everyone out); and rate limiting on the login endpoint.
// A deployment may instead skip minting entirely and verify tokens issued
// by an external identity provider here.
package tokenauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"golang.org/x/crypto/argon2"
)

// argon2id cost, following the OWASP guidance (~19 MiB, 2 passes). This runs
// only at login, so it can afford proper cost without touching per-request
// throughput.
const (
	kdfTime    = 2
	kdfMemory  = 19 * 1024
	kdfThreads = 1
	kdfKeyLen  = 32
)

// dummySalt lets a login for an unknown user cost the same argon2id work as a
// known one, so user existence does not leak through login timing.
var dummySalt = []byte("naust-token-slt.")

// credential is the password side: an argon2id hash and the identity the
// password proves.
type credential struct {
	salt     []byte
	hash     []byte
	identity auth.Identity
}

// Authenticator verifies bearer tokens on the request path and mints them
// from passwords at LoginHandler.
type Authenticator struct {
	// users is configured at setup via AddUser and not mutated once serving
	// has begun, so login reads it without locking.
	users map[string]credential

	// tokens maps sha256(token) to the identity it grants. It is mutated at
	// runtime as logins mint tokens, so it is guarded.
	mu     sync.RWMutex
	tokens map[string]auth.Identity
}

// New returns an empty Authenticator.
func New() *Authenticator {
	return &Authenticator{
		users:  make(map[string]credential),
		tokens: make(map[string]auth.Identity),
	}
}

// AddUser registers username/password with a single personal account. The
// password is hashed now; the plain text is not retained.
func (a *Authenticator) AddUser(username, password string, accountID jmap.Id) {
	salt := make([]byte, 16)
	rand.Read(salt) // crypto/rand.Read never returns an error
	a.users[username] = credential{
		salt: salt,
		hash: hash(password, salt),
		identity: auth.Identity{
			Username: username,
			Accounts: map[jmap.Id]auth.Access{accountID: {Name: username, Personal: true}},
			Primary:  accountID,
		},
	}
}

func hash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, kdfTime, kdfMemory, kdfThreads, kdfKeyLen)
}

// LoginHandler is the mint endpoint: POST with HTTP Basic credentials, and on
// success it returns a fresh bearer token as the plain-text body. This is the
// only place the KDF runs. A real API would return JSON with an expiry; the
// plain body keeps the example's walkthrough dependency-free.
func (a *Authenticator) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="login"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, ok := a.verifyPassword(username, password)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, a.mint(id))
	})
}

// verifyPassword runs the KDF and constant-time compare. Unknown users cost
// the same work against a throwaway salt, so existence stays hidden.
func (a *Authenticator) verifyPassword(username, password string) (auth.Identity, bool) {
	u, found := a.users[username]
	salt := u.salt
	if !found {
		salt = dummySalt
	}
	got := hash(password, salt)
	if !found || subtle.ConstantTimeCompare(got, u.hash) != 1 {
		return auth.Identity{}, false
	}
	return u.identity, true
}

// mint creates a 256-bit token, stores the hash of it against the identity,
// and returns the token itself. Only the hash is retained, so a leak of the
// token store does not hand out usable tokens.
func (a *Authenticator) mint(id auth.Identity) string {
	raw := make([]byte, 32)
	rand.Read(raw) // crypto/rand.Read never returns an error
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	a.mu.Lock()
	a.tokens[string(sum[:])] = id
	a.mu.Unlock()
	return token
}

// Challenge implements auth.Challenger: requests to /api, /eventsource, etc.
// carry a bearer token, not Basic credentials, so the 401 challenge must say
// so.
func (a *Authenticator) Challenge() string {
	return `Bearer realm="jmap"`
}

// Authenticate implements auth.Authenticator: it reads the bearer token and
// looks up its hash. No KDF, because the token is high-entropy.
func (a *Authenticator) Authenticate(r *http.Request) (*auth.Identity, error) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return nil, auth.ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(h[len(prefix):]))
	a.mu.RLock()
	id, ok := a.tokens[string(sum[:])]
	a.mu.RUnlock()
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	return &id, nil
}
