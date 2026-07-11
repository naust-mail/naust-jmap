<div align="center">

# naust-jmap

**A Go runtime for building JMAP servers.**

Not a mail server - a library that executes the JMAP protocol\
(RFC 8620 and friends) correctly, and leaves everything else to you.

[![License: Apache-2.0](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)

</div>

---

<!-- Badges to add once public: CI workflow, pkg.go.dev reference. -->

naust-jmap has no opinion about anything the RFCs are silent on. You supply
object types, storage, authentication, search, and delivery through small
interfaces; the runtime supplies protocol correctness: the session resource,
request dispatch, standard method semantics, change tracking, state strings,
and push.

- The runtime owns correctness. Plugins own meaning. Backends own persistence.
- The library module is `github.com/naust-mail/naust-jmap/core` and depends on
  the Go standard library only. Anything needing third-party code lives in a
  separate module; you import only what you use.

## Layout

One module per component. Drivers implement providers. Datatypes consume the
runtime.

| Directory         | What lives there                                                                                                               | You...                    |
|-------------------|--------------------------------------------------------------------------------------------------------------------------------|---------------------------|
| `core/`           | The runtime library, one Go module, stdlib-only forever                                                                        | import always             |
| `core/providers/` | The interfaces the runtime needs (storage, blobs, leases, notifications, auth), each with a built-in in-process implementation | pick or implement         |
| `drivers/`        | Provider implementations that need third-party dependencies (sqlite today), each its own module                                | import at most one or two |
| `datatypes/`      | JMAP datatypes served on top of the runtime (mail arrives here first), each its own module                                     | import what you serve     |
| `examples/`       | Runnable servers, starting with the quickstart below                                                                           | read                      |

## Quickstart

A complete runnable server lives in
[`examples/quickstart`](examples/quickstart/main.go). The whole idea in one
screen: describe a datatype as data (the Todo type from the RFC's own
examples), and the runtime derives its `/get`, `/changes`, `/set`, `/copy`,
`/query`, and `/queryChanges` methods with full RFC 8620 semantics.

```go
be := memory.New()
db := objectdb.New(be, lease.NewInProcess(be))

users := auth.NewStatic()
users.AddUser("demo@example.com", "demo", "Ademo")

todo := &descriptor.Type{
    Name:       "Todo",
    Capability: "urn:example:todo",
    Properties: map[string]descriptor.Property{
        "title": {Kind: descriptor.KindString, Indexed: true},
        "done":  {Kind: descriptor.KindBool, Indexed: true, Default: json.RawMessage(`false`)},
        "notes": {Kind: descriptor.KindString},
    },
}

proc := runtime.NewProcessor()
core := runtime.DefaultCoreCapabilities()
runtime.RegisterStandardType(proc, db, todo, core)

srv, _ := runtime.NewServer(users, proc, "http://localhost:8080", core)
srv.RegisterCapability("urn:example:todo", struct{}{}, struct{}{})
srv.EnableBlobs(db, kvstore.New(be))                     // uploads, downloads, Blob/copy
srv.EnablePush(db, notify.NewInProcess(), nil, nil)      // event-source push
http.ListenAndServe("localhost:8080", srv)
```

Run it and speak JMAP:

```sh
go run ./examples/quickstart

curl -su demo@example.com:demo http://localhost:8080/.well-known/jmap

curl -su demo@example.com:demo http://localhost:8080/api \
  -H 'Content-Type: application/json' -d '{
    "using": ["urn:ietf:params:jmap:core", "urn:example:todo"],
    "methodCalls": [
      ["Todo/set", {"accountId": "Ademo", "create":
        {"t1": {"title": "try JMAP"}}}, "0"],
      ["Todo/query", {"accountId": "Ademo", "filter":
        {"done": false}, "sort": [{"property": "title"}]}, "1"],
      ["Todo/get", {"accountId": "Ademo",
        "#ids": {"resultOf": "1", "name": "Todo/query", "path": "/ids"}}, "2"]
    ]
  }'
```

That request creates a record, queries for it, and fetches it via a
back-reference, in one round trip. Patches (`Todo/set` with `update`),
sync (`Todo/changes`), creation-id references (`"#t1"`), and query
windowing (`position`, `anchor`, `limit`) all work as the RFC specifies.

## JMAP support

The core protocol (RFC 8620) is implemented end to end:

- The core protocol: session resource, request envelope, back-references,
  capability gating, request limits, strict I-JSON.
- The six derived standard methods over any registered datatype: `/get`,
  `/changes`, `/set`, `/copy`, `/query`, `/queryChanges`.
- Binary data (`Server.EnableBlobs`): upload/download endpoints and
  `Blob/copy`, with reference tracking and unreferenced-blob sweeping.
- Push (`Server.EnablePush`): the event-source endpoint, plus verified
  `PushSubscription` webhooks with RFC 8291 encryption when given a
  subscription store and sender.

<details>
<summary>Full RFC 8620 support matrix</summary>

`Foo` below stands for any registered datatype; the methods are derived
from its descriptor, not written per type.

| Category   | Feature                                | Status   | Notes                                                                                         |
|------------|----------------------------------------|----------|-----------------------------------------------------------------------------------------------|
| Session    | Session resource (`/.well-known/jmap`) | Yes      | Accounts, capabilities, URLs, `sessionState` on every response                                |
| Session    | HTTP Basic authentication              | Yes      | Pluggable via the `providers/auth` interface                                                  |
| Core       | Capability negotiation (`using`)       | Yes      | Non-opted capabilities behave as absent                                                       |
| Core       | `Core/echo`                            | Yes      |                                                                                               |
| API        | Request envelope (`/api`)              | Yes      | Batched method calls, strict I-JSON, request limits                                           |
| API        | Request- and method-level errors       | Yes      | Full RFC 8620 error catalog                                                                   |
| References | Back-references (`#arg`)               | Yes      | JSON Pointer evaluation with `*` array flattening                                             |
| References | Creation-id references (`#creationId`) | Yes      | Request-wide `createdIds` map                                                                 |
| Methods    | `Foo/get`, `Foo/changes`, `Foo/set`    | Yes      | PatchObject validation, change coalescing, per-record atomicity                               |
| Methods    | `Foo/copy`                             | Yes      | Cross-account copy with `onSuccessDestroyOriginal`                                            |
| Methods    | `Foo/query`                            | Yes      | Indexed range scans, in-memory residual, anchors, windowing                                   |
| Methods    | `Foo/queryChanges`                     | Fallback | Always answers `cannotCalculateChanges` (permitted by section 5.6); clients refetch the query |
| State      | State strings and `ifInState`          | Yes      | Optimistic concurrency with `stateMismatch`                                                   |
| Blobs      | Upload/download endpoints, `Blob/copy` | Yes      | Reference tracking and unreferenced-blob sweeping                                             |
| Push       | EventSource (`/eventsource`)           | Yes      | `types`, `closeafter`, `ping` arguments                                                       |
| Push       | `PushSubscription` webhooks            | Yes      | Verification flow, RFC 8291 payload encryption                                                |

</details>

## Roadmap

naust-jmap is pre-release: no tagged versions yet. Coming next, in order:

- Mail (RFC 8621): `Email`, `Mailbox`, and `Thread` as the first datatype
  module, with LMTP and HTTP delivery adapters
- Mail submission: `EmailSubmission` and `Identity`
- A Postgres driver and multi-node cluster testing

## License

[Apache-2.0](LICENSE). See also [NOTICE](NOTICE).
