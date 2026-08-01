# websocket

[![Go Reference](https://pkg.go.dev/badge/github.com/naust-mail/naust-jmap/capabilities/websocket.svg)](https://pkg.go.dev/github.com/naust-mail/naust-jmap/capabilities/websocket)

JMAP over WebSocket (RFC 8887) as an optional capability: an HTTP endpoint that
upgrades to the `jmap` subprotocol, runs Request/Response exchanges over the
runtime's own request pipeline, and optionally pushes `StateChange` objects over
the same socket.

Standard library only - the RFC 6455 framing is implemented here, not imported.

```sh
go get github.com/naust-mail/naust-jmap/capabilities/websocket
```

## Overview

One package. It sits beside the HTTP `/api` and `/eventsource` endpoints rather
than replacing them: a client that speaks WebSocket gets requests and push on
one connection, and a client that does not is unaffected.

Because it runs through the runtime's pipeline, it shares
`maxConcurrentRequests` with the HTTP endpoint instead of opening a second,
unaccounted path into the server.

## When to use this module

Import it when clients benefit from one connection instead of two - typically
browser and mobile clients that would otherwise hold an EventSource stream open
alongside their API requests.

Skip it if your clients only speak HTTP. Unimported, it is absent from the
binary and from the dependency graph.

Import cost: none beyond the standard library.

```go
ws := websocket.NewHandler(srv, users)
ws.EnablePush(db, notifier)          // optional: StateChange over the socket
defer ws.Shutdown()

srv.Capability(websocket.CapabilityURI).
    Advertise(websocket.SessionCapability(srv.BaseURL(), "/ws", ws.SupportsPush()), struct{}{})
mux.Handle("/ws", ws)
```

The advertised path and the mounted path must match; the session resource is
what tells a client where to connect.

## Public API

| Symbol                                                                  | Kind   | What it is                                                                   |
|-------------------------------------------------------------------------|--------|------------------------------------------------------------------------------|
| `NewHandler(srv *runtime.Server, authn auth.Authenticator) *Handler`    | func   | The upgrade endpoint. Mount it on your mux                                   |
| `Handler`                                                               | type   | An `http.Handler` performing the RFC 6455 handshake and running the socket   |
| `Handler.EnablePush(db, notifier)`                                      | method | Turn on StateChange push over the socket. Without it `SupportsPush` is false |
| `Handler.SupportsPush()`                                                | method | Whether push is available, for the advertised capability object              |
| `Handler.ServeHTTP`                                                     | method | It is an `http.Handler`; mount it at the path you advertise                  |
| `Handler.Shutdown()`                                                    | method | Drain and close live sockets, bounded by `DrainDeadline`; returns with no connection goroutine left running |
| `SessionCapability(baseURL, path string, supportsPush bool) Capability` | func   | The capability object to advertise in the session resource                   |
| `Capability`                                                            | type   | The advertised object: URL and push support                                  |
| `CapabilityURI`                                                         | const  | `urn:ietf:params:jmap:websocket`                                             |
| `IdleTimeout`, `WriteDeadline`, `DrainDeadline`, `CloseReplyDeadline`   | var    | Connection timing                                                            |
| `MaxMessageSize`, `MaxFragments`, `MaxRequestIDLength`, `LaneCap`       | var    | Per-connection limits                                                        |
| `ReauthInterval`                                                        | var    | How often a live socket re-checks its credential                             |
| `ReauthWithRevoker`                                                     | var    | Run the re-auth backstop even when the authenticator has a revocation stream |

The package-level vars are the tuning surface; set them before serving.

## Concepts

**The socket is a transport, not a second server.** Requests arriving over it go
through the same pipeline, the same capability gating and the same concurrency
slots as `/api`.

**Requests are correlated by `id`.** A client may have several in flight; each
response carries back the request's `id`.

**Push is a snapshot, not a log.** `StateChange` objects are pushed as they
occur, and there is no `pushState`: on reconnection the client resyncs from the
state strings it holds, which the snapshot covers.

**Long-lived connections re-check authentication.** A credential revoked
mid-session does not stay valid until the socket closes; `ReauthInterval` bounds
the window, and the runtime's revocation path closes affected connections. With
an `auth.Revoker` present the periodic re-check is normally off (the stream
carries revocations); `ReauthWithRevoker` opts it back on as an independent
floor, at the cost of one `Authenticate` per connection per interval.

## Examples

`examples/mailserver` exercises this module in
[`websocket_test.go`](../../examples/mailserver/websocket_test.go).

## Status and compatibility

Pre-release, tagged v0.1.2. Requires `core` v0.3.0 or later.

Deviation from RFC 8887 worth knowing: no `pushState` is emitted or accepted.

## Related modules

| Module                                   | Relationship                                                                   |
|------------------------------------------|--------------------------------------------------------------------------------|
| [`core`](../../core)                     | Provides the request pipeline, concurrency slots and push this module rides on |
| [`datatypes/mail`](../../datatypes/mail) | Independent; the socket carries any registered datatype                        |

For enabling and tuning it, see
[naust.email/naust-jmap/websocket](https://naust.email/naust-jmap/websocket).
