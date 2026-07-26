// Package websocket implements JMAP over WebSocket (RFC 8887) as an
// optional capability for the naust-jmap runtime: an HTTP endpoint
// that upgrades to the "jmap" subprotocol (RFC 6455 handshake), runs
// Request/Response exchanges over the runtime's request pipeline, and
// optionally pushes StateChange objects.
//
// TLS is the embedder's responsibility, exactly as for the rest of the
// server: RFC 8887 section 3 requires the advertised URL be wss, so
// terminate TLS in front of this handler. Only HTTP/1.1 Upgrade is
// supported; the RFC 8441 extended CONNECT handshake for HTTP/2 is out
// of scope.
package websocket
