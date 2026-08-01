# core

[![Go Reference](https://pkg.go.dev/badge/github.com/naust-mail/naust-jmap/core.svg)](https://pkg.go.dev/github.com/naust-mail/naust-jmap/core)

The runtime: the module that executes the JMAP protocol (RFC 8620). It owns the
session resource, request dispatch, the six derived standard methods, change
tracking, state strings, blobs and push. It owns nothing about what your objects
mean or where they are stored - those arrive through the provider interfaces
below.

This module depends on the Go standard library only, and always will.

```sh
go get github.com/naust-mail/naust-jmap/core
```

## Contents

- [Overview](#overview)
- [When to use this module](#when-to-use-this-module)
- [Building a server](#building-a-server)
- [Public API](#public-api)
- [Concepts](#concepts)
- [Extension points](#extension-points)
  - [Custom methods](#custom-methods)
- [Choosing a blob store](#choosing-a-blob-store)
- [Protocol support](#protocol-support)
- [Examples](#examples)
- [Status and compatibility](#status-and-compatibility)
- [Related modules](#related-modules)

## Overview

| Package             | What it holds                                                                                                                                   |
|---------------------|-------------------------------------------------------------------------------------------------------------------------------------------------|
| `runtime`           | Request dispatch, back-reference resolution, capability enforcement, the session endpoint, standard method derivation                           |
| `descriptor`        | The schema vocabulary datatype plugins register instead of implementing methods                                                                 |
| `objectdb`          | Collections of typed records with in-commit index maintenance, a per-account change log, and per-type state strings, over any `backend.Backend` |
| `objectdb/maintain` | Scheduled storage reclamation: change-log trimming and unreferenced-blob collection                                                             |
| `jmap`              | The protocol wire types: request/response envelope, session resource, identifiers, error taxonomy                                               |
| `tuning`            | The tunable defaults, in one place: piece sizes, retention windows, request caps, the object-id scheme                                          |
| `pushsub`           | `PushSubscription` record persistence (RFC 8620 section 7.2)                                                                                    |
| `webpush`           | RFC 8291 payload encryption and RFC 8030 delivery, with the SSRF protection RFC 8620 section 8.6 requires                                       |
| `providers/...`     | The interfaces the runtime needs, each with a built-in in-process implementation. See [Extension points](#extension-points)                     |

## When to use this module

Always. Every naust-jmap server imports it, and nothing else in the repository
works without it.

You do not use it to get a mail server. It has no notion of a Mailbox or an
Email; those live in [`datatypes/mail`](../datatypes/mail). You use it when you
want the JMAP protocol executed correctly over object types you define yourself,
storage you choose, and authentication you already have.

Import cost: none beyond the standard library. The `core/internal/depsguard`
test enforces that.

## Building a server

The shape of every server built on this module:

```go
be := memory.New()                                  // or a driver module
db := objectdb.New(be, lease.NewInProcess(be))

proc := runtime.NewProcessor()
core := runtime.DefaultCoreCapabilities()
runtime.RegisterStandardType(proc, db, myType, core) // the six methods, derived

srv, err := runtime.NewServer(users, proc, "https://jmap.example.com", core)
srv.EnableBlobs(db, kvstore.New(be))                 // optional: binary data
srv.EnablePush(db, notify.NewInProcess(), nil, nil)  // optional: change notification
http.ListenAndServe(":8080", srv)
```

Both `Enable` calls are optional, and a server without them still speaks the
core protocol. Call either one before serving.

**`EnableBlobs`** turns on the upload and download endpoints and `Blob/copy`
(RFC 8620 section 6). Blobs are the immutable binary data a record refers to by
`blobId` - an email's raw message and its attachments, for instance. Without it
the server cannot accept or serve binary content at all. Pass it whichever
`blob.Store` suits your deployment; see [Choosing a blob
store](#choosing-a-blob-store).

**`EnablePush`** turns on change notification (section 7), so clients learn that
a type changed instead of polling for it. Commits publish their touched types to
the `notify.Notifier`, and the `/eventsource` endpoint streams `StateChange`
objects to connected clients. The last two arguments - a `pushsub.Store` and a
`webpush.Sender` - add `PushSubscription` webhooks with RFC 8291 encryption, for
clients that cannot hold a connection open.

> Passing `nil` for both, as above, enables the event source only. That is **not
> RFC 8620 conformant** - section 7.2 has no opt-out - and is intended for
> development. A production server supplies both.

Event-source streams authenticate once and then hold open, so implement
`auth.Revoker` on your authenticator to close a revoked identity's streams
promptly; without one, streams are capped at `tuning.EventSourceMaxLifetime`
and re-authenticate on reconnect.

## Public API

godoc is the reference. This table is the map: the entry points a consumer
actually calls.

| Symbol                                                             | Kind         | What it is                                                                                                   |
|--------------------------------------------------------------------|--------------|--------------------------------------------------------------------------------------------------------------|
| `runtime.NewServer`                                                | func         | Builds the HTTP face of the runtime: session resource plus API endpoint. Assumes the embedder terminates TLS |
| `runtime.NewProcessor`                                             | func         | The method dispatcher every datatype registers into                                                          |
| `runtime.RegisterStandardType`                                     | func         | Derives `/get`, `/changes`, `/set`, `/copy`, `/query`, `/queryChanges` for a descriptor type                 |
| `runtime.RegisterStandardTypeExt`                                  | func         | The same, with method extensions (hooks, computed properties, custom filter semantics)                       |
| `runtime.DefaultCoreCapabilities`                                  | func         | The advertised RFC 8620 section 2 limits, at their defaults                                                  |
| `Server.Capability(uri).Advertise(...)`                            | method       | Advertises a capability URI and its session/account objects                                                  |
| `Server.EnableBlobs`                                               | method       | Turns on upload/download and `Blob/copy` against a `blob.Store`                                              |
| `Server.EnablePush`                                                | method       | Turns on the event-source endpoint, and webhooks when given a subscription store and sender                  |
| `Server.ServeHTTP`                                                 | method       | Standard `http.Handler`; mount it where you like                                                             |
| `Server.Close`                                                     | method       | Ends the server's lifetime: stops and joins its background goroutines. Shut transports down first            |
| `Server.AcquireSlot` / `TryAcquireSlot`                            | method       | The concurrency gate, shared with out-of-band transports such as WebSocket                                   |
| `descriptor.Type`, `descriptor.Property`, `descriptor.Kind`        | type         | A datatype described as data: properties, kinds, which are indexed, defaults                                 |
| `objectdb.New`                                                     | func         | The object database over a backend and a lease manager                                                       |
| `objectdb.WithIdScheme`, `WithNow`, `WithVerifyPreImages`          | func         | Construction options                                                                                         |
| `objectdb.DB`, `Update`, `ChangeSet`, `Object`                     | type         | The storage-facing types a datatype plugin writes against                                                    |
| `maintain.Run`, `maintain.RunOnce`, `maintain.Config`              | func / type  | Reclamation, as a loop or a single crank                                                                     |
| `jmap.Id`, `jmap.NewId`, `jmap.NewULID`, `jmap.NewSequenceId`      | type / func  | Identifiers and the shipped id schemes                                                                       |
| `jmap.Session`, `jmap.Request`, `jmap.Response`, `jmap.Invocation` | type         | The protocol envelope                                                                                        |
| `jmap.MethodError`, `jmap.SetError`, and the error constants       | type / const | The full RFC 8620 error catalog                                                                              |
| `runtime.QueryHooks`, `runtime.SetHooks`, `runtime.Extensions`     | type         | Where a datatype attaches meaning to a derived method                                                        |
| `runtime.CheckRecordLocal`                                         | func         | The shipped checker a datatype's tests prove `QueryHooks` declarations with                                  |
| `runtime.ComputedProperties`                                       | interface    | Expose properties on `/get` that are derived rather than stored                                              |
| `runtime.CopyBlob`, `runtime.OpenBlob`                             | func         | Read and copy blob content from a datatype that references blobs of its own                                  |
| `jmap.CoreCapabilities`                                            | type         | The advertised limits themselves; mutate one to change `maxSizeUpload` and friends before `NewServer`        |
| `lease.RunSingleton`, `lease.SingletonConfig`                      | func / type  | Run a background job on exactly one instance of a fleet                                                      |
| `tuning.*` (package-level vars)                                    | var          | The knobs. Set them before serving; `tuning.Validate` reports inconsistent combinations                      |

Adding a method beyond the derived six uses a separate group of symbols; see
[Custom methods](#custom-methods).

## Concepts

**Descriptors, not methods.** A datatype is registered as data - a
`descriptor.Type` with properties - and the runtime derives its six standard
methods from that description. A plugin writes method code only where the RFC
gives the type meaning the schema cannot express.

**The runtime owns correctness; plugins own meaning; backends own persistence.**
This is the placement rule for the whole project. If a behaviour is mandated of
every conformant server by RFC 8620 or 8621, it lives here. If it is optional,
it lives in its own module and reaches the runtime through a public seam.

**One writer per account.** Consistency is a per-account writer lease plus
in-commit index maintenance, not a global transaction. `lease.Manager` is what
makes a multi-process deployment safe; the in-process implementation is what
makes a single-process one cheap.

**States are commit numbers.** How far behind a client is costs a subtraction,
which is what makes `/changes` and `/queryChanges` affordable.

**Selection is by import.** There is no build tool and no feature flags. What
your `main.go` imports is what exists in the binary and in the dependency graph.
A blob store you do not import is absent from both.

## Extension points

Each provider is an interface plus a built-in in-process implementation, so a
server runs before you write any of them.

| Interface                      | You implement it to...                                 | Ships with                                                                                                 |
|--------------------------------|--------------------------------------------------------|------------------------------------------------------------------------------------------------------------|
| `providers/backend.Backend`    | Store bytes. A small ordered key-value contract        | `backend/memory`, plus the [sqlite](../drivers/sqlite) and [postgres](../drivers/postgres) driver modules  |
| `providers/auth.Authenticator` | Verify a credential and name the accounts it may reach | Nothing: authentication is always yours. See `examples/internal` for a password and a bearer-token pattern |
| `providers/blob.Store`         | Persist blob content                                   | `blob/kvstore`, `blob/chunkstore`, `blob/fsstore`. See [Choosing a blob store](#choosing-a-blob-store)     |
| `providers/lease.Manager`      | Serialize writers per account                          | `lease.NewInProcess` (single process), `lease.NewStoreLease` (shared store)                                |
| `providers/notify.Notifier`    | Fan out post-commit changes to push                    | `notify.NewInProcess`, plus the postgres driver's LISTEN/NOTIFY hints                                      |

Optional companions to the above: `auth.Challenger` (authentication challenges),
`auth.Revoker` (credential revocation), `backend.CompareAndSwapper` and
`backend.MultiGetter` (backends that can do better than the base contract),
`blob.BatchDeleter`, `lease.Waker`.

**Conformance suites.** An implementation is correct when it passes the shipped
contract suite for its interface, not when it merely compiles:
`backend/backendtest.Run`, `lease/leasetest.Run`, `notify/notifytest.Run` and
`notifytest.RunLinked`. Every shipped implementation passes them; a driver that
does not pass is not interchangeable with one that does. Write the suite call
first. [`drivers/`](../drivers) walks through implementing a `backend.Backend`
end to end.

### Custom methods

The six derived methods cover the CRUD-shaped surface RFC 8620 defines. Anything
else a datatype needs - an action, a bulk operation, a query that is not a query -
is registered as a method of its own, and the runtime dispatches it exactly like
a derived one: same capability gating, same back-reference resolution, same
concurrency slot.

```go
proc.Register("Todo/archive", "urn:example:todo",
    func(ctx context.Context, call *runtime.Call) []jmap.Invocation {
        var args struct{ AccountId jmap.Id `json:"accountId"` }
        if err := runtime.DecodeArgs(call.Args, &args); err != nil {
            return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
        }
        if errType, desc := runtime.CheckAccount(call, args.AccountId, true); errType != "" {
            return runtime.Fail(call.CallID, errType, desc)
        }
        // ... do the work against db ...
        return runtime.Reply(call.Name, call.CallID, map[string]any{"archived": 0})
    })
```

| Symbol | Kind | What it is |
|--------|------|------------|
| `Processor.Register(name, capability, h)` | method | Register a method under a capability URI |
| `Registration.Method(name, h)` | method | The same, chained off `Server.Capability` |
| `runtime.Handler` | type | `func(ctx, *Call) []jmap.Invocation` - what you implement |
| `runtime.Call` | type | The invocation: `Name`, `Args`, `CallID`, `Identity`, `CreatedIds` |
| `runtime.Reply`, `runtime.Fail` | func | Build the success or error response invocation |
| `runtime.DecodeArgs` | func | Decode `Call.Args` with the strict I-JSON rules the protocol requires |
| `runtime.CheckAccount` | func | Enforce that the caller may touch the account, before you act |
| `runtime.ResolveIdArg` | func | Resolve a `"#creationId"` reference against the request's `createdIds` |

Handlers must honour the same rules the derived methods do: check account access
before acting, return `Fail` rather than panicking, and never write outside the
account lease.

## Choosing a blob store

Three implementations of `blob.Store` ship. Nothing above the interface can tell
them apart, so this is an operational choice, not an architectural one - pick by
importing, since a store you do not import is absent from the binary and from
the dependency graph.

| Store        | Shape                                 | Choose it when                                                                                                                                                                                                                                                                                                            |
|--------------|---------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `kvstore`    | Each blob is one value in the backend | Blobs are reliably small **and you can bound both blob size and ingress concurrency**. Fewest writes per blob, and blobs commit in the same transaction as the objects referencing them. Cost: an arriving blob is held whole in memory, so peak scales with blob size times concurrent writers - see the warning below.  |
| `chunkstore` | Fixed-size pieces plus a manifest     | Blobs can be large or the size is not known in advance. Memory stays flat regardless of blob size, still transactional, at the cost of several writes per blob. The default for mail.                                                                                                                                     |
| `fsstore`    | One file per blob, tmp-then-rename    | Throughput on large blobs matters more than transactional coupling. A transactional store has to journal every byte it takes; this does not. Cost: blobs no longer commit with the objects referencing them (the ordering is chosen so the survivable inconsistency is an unreferenced blob, which the sweeper reclaims). |

> **`kvstore`'s memory is attacker-controlled.** It holds each arriving blob whole
> in memory, and both factors in `size x concurrency` are normally chosen by the
> client: blob size up to `maxSizeUpload`, concurrency up to whatever the ingress
> allows. Measured at 16 concurrent deliveries of a 16 MiB message, peak RSS was
> about **162 MiB** with `chunkstore` and about **1.1 GiB** with `kvstore`; at the
> shipped mail defaults (64 concurrent LMTP connections, a 50 MB size cap) the
> same arithmetic reaches several gigabytes. That is a denial-of-service vector,
> not a tuning preference. Choose `kvstore` only where you can bound both factors
> - `chunkstore`'s peak depends on neither.

<details>
<summary>Where the crossover between kvstore and chunkstore actually falls</summary>

The crossover between `kvstore` and `chunkstore` is real but not a fixed number:
it moves with the backend's per-row overhead and its own out-of-line policy
(Postgres already moves large values out of line via TOAST), with blob size, and
with write concurrency. As one data point, on SQLite at 32 concurrent writers
`kvstore` ingested roughly 40% faster at 4 KiB, the two were level around 64 KiB,
and `chunkstore` was roughly 3x faster at 1 MiB. Measure on your own backend
before treating any threshold as settled.

</details>

## Protocol support

<details>
<summary>Full RFC 8620 support matrix</summary>

`Foo` below stands for any registered datatype; the methods are derived
from its descriptor, not written per type.

| Category   | Feature                                | Status | Notes                                                                                                                                                                            |
|------------|----------------------------------------|--------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Session    | Session resource (`/.well-known/jmap`) | Yes    | Accounts, capabilities, URLs, `sessionState` on every response                                                                                                                   |
| Session    | Authentication                         | Yes    | Pluggable via the `providers/auth` interface; quickstart uses Basic, mailserver uses bearer tokens                                                                               |
| Session    | Advertised limits                      | Yes    | `maxSizeUpload`, `maxSizeRequest`, `maxCallsInRequest`, `maxObjectsInGet/Set`, etc. (section 2), enforced server-side                                                            |
| Core       | Capability negotiation (`using`)       | Yes    | Non-opted capabilities behave as absent                                                                                                                                          |
| Core       | `Core/echo`                            | Yes    |                                                                                                                                                                                  |
| API        | Request envelope (`/api`)              | Yes    | Batched method calls, strict I-JSON, request limits                                                                                                                              |
| API        | Request- and method-level errors       | Yes    | Full RFC 8620 error catalog                                                                                                                                                      |
| References | Back-references (`#arg`)               | Yes    | JSON Pointer evaluation with `*` array flattening                                                                                                                                |
| References | Creation-id references (`#creationId`) | Yes    | Request-wide `createdIds` map                                                                                                                                                    |
| Methods    | `Foo/get`, `Foo/changes`, `Foo/set`    | Yes    | PatchObject validation, change coalescing, per-record atomicity                                                                                                                  |
| Methods    | `Foo/copy`                             | Yes    | Cross-account copy with `onSuccessDestroyOriginal`                                                                                                                               |
| Methods    | `Foo/query`                            | Yes    | Indexed range scans, in-memory residual, anchors, windowing                                                                                                                      |
| Methods    | `Foo/queryChanges`                     | Yes    | Real diffs for record-local queries (declared via `QueryHooks`), tiered answering, `upToId`, work-budget refusals                                                                |
| State      | State strings and `ifInState`          | Yes    | Optimistic concurrency with `stateMismatch`                                                                                                                                      |
| Blobs      | Upload/download endpoints, `Blob/copy` | Yes    | Reference tracking and unreferenced-blob sweeping                                                                                                                                |
| Push       | EventSource (`/eventsource`)           | Yes    | `types`, `closeafter`, `ping` arguments                                                                                                                                          |
| Push       | `PushSubscription` webhooks            | Yes    | Verification flow, RFC 8291 payload encryption                                                                                                                                   |
| Transport  | JMAP over WebSocket (RFC 8887)         | Yes    | `jmap` subprotocol, request `id` correlation, socket push; no `pushState` (snapshot covers reconnection). Separate module: [`capabilities/websocket`](../capabilities/websocket) |

</details>

<details>
<summary>Design decision: what queryChanges will and will not answer</summary>

Where the RFCs leave a behavior to the server, the choice is recorded so
embedders know what to expect. The mail module records its own in
[`datatypes/mail`](../datatypes/mail#design-decisions).

**queryChanges answers only what it can prove.** `Foo/queryChanges` computes
real diffs for record-local queries: those whose filter and sort verdicts
depend only on each record's own data, declared per name on
`runtime.QueryHooks` (core-language queries qualify by construction, and
`runtime.CheckRecordLocal` is the shipped checker a datatype's tests prove
declarations with). Anything undeclared - Email's thread-keyword conditions,
Mailbox tree arrangements - answers `cannotCalculateChanges`, the section
5.6 escape that costs the client one refetch; a forgotten declaration
degrades service but can never corrupt a client's cached list. States are
plain commit numbers, so how far behind a client is costs a subtraction, and
a single work budget refuses oversized answers before the expensive work
happens. Collapsed Email queries stay sound through the Thread group
companion: every membership change updates the Thread record, so a destroyed
representative's untouched sibling is still re-reported. Ordered streaming
evaluation for mutable queries (bounding tier-2 work by index order) is a
recognized future extension of the query planner, not built.

</details>

## Examples

- [`examples/quickstart`](../examples/quickstart) - the whole module in one
  screen: one datatype described as data, served over HTTP.
- [`examples/mailserver`](../examples/mailserver) - the runtime wired to a real
  driver, a real datatype and a real delivery path.

## Status and compatibility

Pre-release, tagged v0.1.0. Breaking changes may land in minor bumps until 1.0.
Every other module in this repository requires `core` at v0.1.0 or later; keep
them on the same version.

## Related modules

| Module                                                | Relationship                                        |
|-------------------------------------------------------|-----------------------------------------------------|
| [`datatypes/mail`](../datatypes/mail)                 | The RFC 8621 datatype plugin built on this runtime  |
| [`drivers/sqlite`](../drivers/sqlite)                 | `backend.Backend` over a SQLite file                |
| [`drivers/postgres`](../drivers/postgres)             | `backend.Backend` over Postgres, plus cluster hints |
| [`capabilities/websocket`](../capabilities/websocket) | RFC 8887 transport beside the HTTP endpoints        |

For task-oriented guidance see the documentation site:
[architecture](https://naust.email/naust-jmap/architecture),
[auth](https://naust.email/naust-jmap/auth),
[fleet](https://naust.email/naust-jmap/fleet), and the full
[reference matrices](https://naust.email/naust-jmap/reference).
