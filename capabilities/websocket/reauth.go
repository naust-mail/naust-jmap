package websocket

import (
	"context"
	"math/rand"
	"net/http"
	"time"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
)

// reauthLoop is the credential-expiry backstop for authenticators that
// implement no auth.Revoker: the handshake request's credentials are
// periodically re-verified, and a connection whose credentials no
// longer authenticate is closed with 1008 (RFC 8887 section 4.2 - the
// server may close the connection when the credentials used for the
// handshake expire). Intervals are jittered so a fleet's re-auth work
// never synchronizes into load spikes, and every check re-reads the
// authenticator's backing store, so it is fleet-correct: a credential
// killed on another box dies here within one interval.
func (c *conn) reauthLoop(r *http.Request) {
	// The stored request outlives its handler-scoped context; clone it
	// onto the connection's own so authenticators that consult the
	// request context see a live one.
	req := r.Clone(context.WithoutCancel(c.ctx))
	for {
		select {
		case <-time.After(jittered(c.h.reauthEvery)):
		case <-c.ctx.Done():
			return
		}
		if _, err := c.h.authn.Authenticate(req); err != nil {
			c.writeClose(frame.ClosePolicyViolation, "credentials expired")
			c.abort()
			return
		}
	}
}

// jittered spreads d over [0.75d, 1.25d).
func jittered(d time.Duration) time.Duration {
	return d*3/4 + time.Duration(rand.Int63n(int64(d)/2+1))
}
