// Package auth defines the authentication provider. The runtime forces
// authentication to exist (RFC 8620 requires it) but not its nature:
// embedders implement Authenticator however they like; the runtime only
// ever asks "whose request is this, and which accounts may it touch?".
package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// Access describes one account an identity can use.
type Access struct {
	// Name is the user-facing account name (e.g. an email address).
	Name string
	// Personal is true if the account belongs to the identity itself.
	Personal bool
	// ReadOnly is true if the identity may not modify the account.
	ReadOnly bool
}

// Identity is an authenticated caller and its account access. Identity
// and account are deliberately distinct concepts: an identity may reach
// several accounts, and an account may be reachable by several
// identities.
type Identity struct {
	// Username is reported in the session resource; may be empty.
	Username string
	// Credential identifies the specific credential that authenticated,
	// when that is finer-grained than the identity (e.g. one API token
	// among several). Push subscriptions are tied to it (RFC 8620
	// section 7.2): only the creating credential sees or updates them.
	// Empty means the credential IS the identity and Username is used.
	Credential string
	// Accounts maps each reachable account to the identity's access.
	Accounts map[jmap.Id]Access
	// Primary is the identity's default account; it should be a key of
	// Accounts. Capability specs use it for primaryAccounts entries.
	Primary jmap.Id
}

// CredentialKey returns the key push subscriptions are tied to:
// Credential when set, Username otherwise.
func (i *Identity) CredentialKey() string {
	if i.Credential != "" {
		return i.Credential
	}
	return i.Username
}

// ErrUnauthenticated is returned when the request carries no valid
// credentials; the server answers 401.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Authenticator is the provider interface: given an HTTP request, identify the
// caller or reject with ErrUnauthenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (*Identity, error)
}

// Revoker is an optional Authenticator extension for credential
// revocation. Authenticate is per-request, so a revoked credential
// simply fails the next request - but a long-lived connection (an
// EventSource stream, a WebSocket) authenticates once and then holds
// open, and without a push signal it would keep serving a credential
// its owner already killed. An Authenticator that implements Revoker
// tells the runtime the moment an identity's credentials are revoked,
// and the runtime closes that identity's live connections and cancels
// their in-flight work.
//
// In a multi-process deployment every process's subscription must see
// every revocation, wherever it originated - fan-out across the fleet
// is the implementation's responsibility, not the runtime's.
//
// Delivery is at-least-once: an implementation may deliver the same
// revocation more than once (for example, a durable transport that
// re-asserts recent revocations after an outage). Consumers make the
// stream safe to replay by applying events idempotently - comparing
// the event's At against when each connection was established and
// closing only connections that predate the revocation. To keep that
// comparison meaningful, an implementation that relays or re-emits an
// event MUST preserve its original At, never re-stamp it.
type Revoker interface {
	// Revocations returns a stream of revocation events. The
	// implementation closes the channel when ctx is done. The runtime
	// consumes promptly and ignores events for usernames with no live
	// connection in this process.
	Revocations(ctx context.Context) <-chan Revocation
}

// Revocation is one credential-revocation event on a Revoker stream.
type Revocation struct {
	// Username is the Identity.Username whose credentials were revoked.
	Username string
	// At is the instant the revocation took effect. Connections
	// established after it were authenticated against the post-revocation
	// credential state and survive redelivery of this event; anything
	// relaying the event must carry At through unchanged.
	At time.Time
}

// Challenger is an optional Authenticator extension that names the scheme
// a 401 response should challenge for (RFC 7235 section 4.1's
// WWW-Authenticate). Authenticators that don't implement it get the
// runtime's "Basic" default, so existing embedders are unaffected.
type Challenger interface {
	// Challenge returns the WWW-Authenticate header value to send on a
	// failed Authenticate, e.g. `Bearer realm="jmap"`.
	Challenge() string
}
