// Package demoauth is a small reference auth.Authenticator for the
// examples: it verifies HTTP Basic credentials against argon2id password
// hashes, so copying it does not teach a footgun.
//
// Two things a real deployment does differently, and that this omits on
// purpose to stay small:
//
//  1. Users come from a real credential store, not AddUser calls.
//  2. The runtime authenticates EVERY request. Verifying a password (and
//     thus running argon2id) per request is far too expensive at scale,
//     so production verifies a cheap bearer TOKEN here and runs the KDF
//     only at a separate login endpoint that mints the token. This demo
//     hashes per request, which is fine only because it is a demo.
//
// To keep per-request cost sane it carries a verdict cache, built on
// one rule: skip the KDF, never skip the record read. Every request
// still reads the stored credential; the cache only replaces the KDF
// when this exact password already verified against this exact stored
// hash. Because the verdict is pinned to the stored hash it validated
// against, a password change - even one made by another path - forces
// the full KDF on the next request; there is no window where a stale
// cache accepts a dead password.
package demoauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"golang.org/x/crypto/argon2"
)

// Params are the argon2id cost parameters. Higher is more resistant to
// offline cracking but slower per verification.
type Params struct {
	Time    uint32 // number of passes
	Memory  uint32 // memory in KiB
	Threads uint8  // parallelism
}

// Default follows the OWASP argon2id guidance (~19 MiB, 2 passes). Use it
// for anything holding real credentials.
func Default() Params { return Params{Time: 2, Memory: 19 * 1024, Threads: 1} }

// Fast is a deliberately cheap cost for benchmarks and tests, where the
// KDF is not what is being measured. Never use it for real credentials.
func Fast() Params { return Params{Time: 1, Memory: 8 * 1024, Threads: 1} }

// dummySalt lets an unknown user cost the same argon2id work as a known
// one, so user existence does not leak through response timing.
var dummySalt = []byte("naust-demo-salt.")

type user struct {
	salt     []byte
	hash     []byte
	identity auth.Identity
}

// verdict is one cached authentication outcome. quick is an HMAC of the
// presented password keyed by the user's salt: enough to recognize "the
// same password again" with one cheap hash, held only in memory, and
// never a substitute for the stored argon2id hash. storedHash pins the
// credential version the verdict validated against.
type verdict struct {
	storedHash []byte
	quick      []byte
}

// Authenticator verifies HTTP Basic credentials against argon2id hashes.
type Authenticator struct {
	params Params

	// mu guards users and cache: SetPassword mutates at runtime.
	mu    sync.RWMutex
	users map[string]user
	cache map[string]verdict

	// kdfCalls counts hash invocations; a test seam for proving cache
	// hits skip the KDF.
	kdfCalls atomic.Int64
}

// New returns an empty Authenticator using the given cost parameters.
func New(p Params) *Authenticator {
	return &Authenticator{
		params: p,
		users:  make(map[string]user),
		cache:  make(map[string]verdict),
	}
}

// AddUser registers username/password with a single personal account. The
// password is hashed now; the plain text is not retained.
func (a *Authenticator) AddUser(username, password string, accountID jmap.Id) {
	salt := make([]byte, 16)
	rand.Read(salt) // crypto/rand.Read never returns an error
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users[username] = user{
		salt: salt,
		hash: a.hash(password, salt),
		identity: auth.Identity{
			Username: username,
			Accounts: map[jmap.Id]auth.Access{accountID: {Name: username, Personal: true}},
			Primary:  accountID,
		},
	}
	delete(a.cache, username)
}

// SetPassword replaces an existing user's password. The stored hash
// changes, so any cached verdict for the user stops matching and the
// old password dies on its next use; the entry is also dropped
// outright. Unknown usernames are ignored.
func (a *Authenticator) SetPassword(username, password string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[username]
	if !ok {
		return
	}
	salt := make([]byte, 16)
	rand.Read(salt) // crypto/rand.Read never returns an error
	u.salt = salt
	u.hash = a.hash(password, salt)
	a.users[username] = u
	delete(a.cache, username)
}

// hash runs the KDF. It takes no lock: the KDF is the expensive part,
// and holding mu across it would serialize every authentication.
func (a *Authenticator) hash(password string, salt []byte) []byte {
	a.kdfCalls.Add(1)
	return argon2.IDKey([]byte(password), salt, a.params.Time, a.params.Memory, a.params.Threads, 32)
}

// quickCheck is the cheap cache probe: an HMAC-SHA256 of the presented
// password keyed by the user's random salt.
func quickCheck(password string, salt []byte) []byte {
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(password))
	return m.Sum(nil)
}

// Authenticate implements auth.Authenticator. The stored credential is
// read on every call; only the KDF is skippable, and only when the
// cached verdict still pins the exact stored hash on record and the
// presented password matches the verdict's quick check.
func (a *Authenticator) Authenticate(r *http.Request) (*auth.Identity, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	a.mu.RLock()
	u, found := a.users[username]
	v, cached := a.cache[username]
	a.mu.RUnlock()

	if found && cached && subtle.ConstantTimeCompare(v.storedHash, u.hash) == 1 &&
		subtle.ConstantTimeCompare(quickCheck(password, u.salt), v.quick) == 1 {
		id := u.identity
		return &id, nil
	}

	// Full path: run the KDF, against a throwaway salt for unknown users,
	// so a miss costs the same as a hit and existence stays hidden.
	salt := u.salt
	if !found {
		salt = dummySalt
	}
	got := a.hash(password, salt)
	if !found || subtle.ConstantTimeCompare(got, u.hash) != 1 {
		return nil, auth.ErrUnauthenticated
	}
	a.mu.Lock()
	// Re-read the record: the password may have changed while the KDF
	// ran, and a verdict must only ever pin the hash it validated.
	if cur, still := a.users[username]; still && subtle.ConstantTimeCompare(cur.hash, u.hash) == 1 {
		a.cache[username] = verdict{storedHash: u.hash, quick: quickCheck(password, u.salt)}
	}
	a.mu.Unlock()
	id := u.identity
	return &id, nil
}
