package websocket

import "time"

// Tunable defaults for the WebSocket transport, following the same
// convention as the core module's tuning package: compiled-in knobs an
// operator can see in one place, package variables only so tests can
// drive boundaries.

// LaneCap is how many labeled requests (requests carrying the optional
// "id" of RFC 8887 section 4.3.2) one connection may run concurrently.
// The RFC allows out-of-order processing; this bounds how much of the
// server's shared request pool a single connection can occupy at once.
// 1 degenerates to strictly serial processing. Requests WITHOUT an id
// always run one at a time, in order, on a separate serial lane - with
// no id, the client cannot correlate out-of-order responses.
var LaneCap = 2

// WriteDeadline bounds every frame write. A peer that stops reading
// while the server has data to flush would otherwise park a goroutine
// on a dead TCP buffer forever; hitting the deadline abandons the
// connection. 0 disables.
var WriteDeadline = 30 * time.Second

// IdleTimeout closes a connection that has been truly idle - nothing
// in flight and nothing arriving - for this long. Time spent waiting
// for a request slot or executing requests never counts. 0 disables.
var IdleTimeout = 10 * time.Minute

// MessageDeadline bounds one message from its first frame header to
// its last byte: a peer that starts sending must finish the message
// within this window or the connection is failed with 1008 (RFC 6455
// section 10.4 resource limits). The clock starts only when a header
// arrives, so a quiet connection - a push subscriber sitting and
// listening - is never touched by it. 0 disables.
var MessageDeadline = 2 * time.Minute

// MaxMessageSize caps one coalesced text message (RFC 8887 section 4.3
// requires fragments be coalesced before parsing; this bounds what
// that coalescing will buffer). It mirrors the default JMAP
// maxSizeRequest - the request pipeline enforces the session's real
// limit after coalescing, so this knob only needs to be at least that
// large.
var MaxMessageSize int64 = 10_000_000

// MaxFragments caps how many frames one message may span, so a stream
// of one-byte fragments cannot buy unbounded per-frame overhead on the
// way to MaxMessageSize.
var MaxFragments = 1024

// MaxRequestIDLength caps the "id" property of an incoming Request
// (RFC 8887 section 4.3.2). The value is echoed back verbatim in
// responses and errors, so it is attacker-chosen reflected content;
// nothing legitimate needs more than a short correlation token.
var MaxRequestIDLength = 256

// ReauthInterval is how often the credential-expiry backstop re-runs
// Authenticate on a connection's stored handshake request (jittered;
// see reauth.go). The backstop only runs when the authenticator
// implements no auth.Revoker - a revocation stream makes polling
// pointless - unless ReauthWithRevoker turns it on regardless. 0
// disables it entirely, which with no Revoker means open connections
// outlive their credentials until they close themselves.
var ReauthInterval = 10 * time.Minute

// ReauthWithRevoker runs the re-authentication backstop even when the
// authenticator implements auth.Revoker. A revocation stream normally
// makes polling pointless, but a deployment can opt in as a floor that
// holds even if its Revoker never learns of a credential change (for
// example, credentials mutated behind the Revoker's back). It re-runs
// Authenticate per connection per interval - for a KDF-backed
// authenticator that is a real per-connection burn, so weigh the cost
// before enabling.
var ReauthWithRevoker = false

// DrainDeadline bounds how long a graceful shutdown waits for in-
// flight requests to finish and flush before the connection is torn
// down anyway.
var DrainDeadline = 10 * time.Second

// CloseReplyDeadline bounds how long the server waits for the peer's
// answering Close frame after sending its own (RFC 6455 section
// 5.5.1) before closing the TCP connection regardless.
var CloseReplyDeadline = 5 * time.Second
