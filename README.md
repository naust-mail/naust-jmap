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
| `drivers/`        | Provider implementations that need third-party dependencies (sqlite, postgres), each its own module                            | import at most one or two |
| `datatypes/`      | JMAP datatypes served on top of the runtime (mail arrives here first), each its own module                                     | import what you serve     |
| `examples/`       | Runnable servers: the quickstart below and a full mail server (`examples/mailserver`)                                          | read                      |

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
- Binary data (`Server.EnableBlobs`): streaming upload/download endpoints and
  `Blob/copy`, with reference tracking and unreferenced-blob sweeping. Two blob
  stores ship: a whole-value store for the small-blob common case, and a chunked
  streaming store that never holds a whole blob in memory, for content larger than
  a comfortable in-memory value.
- Push (`Server.EnablePush`): the event-source endpoint, plus verified
  `PushSubscription` webhooks with RFC 8291 encryption when given a
  subscription store and sender.

<details>
<summary>Full RFC 8620 support matrix</summary>

`Foo` below stands for any registered datatype; the methods are derived
from its descriptor, not written per type.

| Category   | Feature                                | Status   | Notes                                                                                                                 |
|------------|----------------------------------------|----------|-----------------------------------------------------------------------------------------------------------------------|
| Session    | Session resource (`/.well-known/jmap`) | Yes      | Accounts, capabilities, URLs, `sessionState` on every response                                                        |
| Session    | Authentication                         | Yes      | Pluggable via the `providers/auth` interface; quickstart uses Basic, mailserver uses bearer tokens                    |
| Session    | Advertised limits                      | Yes      | `maxSizeUpload`, `maxSizeRequest`, `maxCallsInRequest`, `maxObjectsInGet/Set`, etc. (section 2), enforced server-side |
| Core       | Capability negotiation (`using`)       | Yes      | Non-opted capabilities behave as absent                                                                               |
| Core       | `Core/echo`                            | Yes      |                                                                                                                       |
| API        | Request envelope (`/api`)              | Yes      | Batched method calls, strict I-JSON, request limits                                                                   |
| API        | Request- and method-level errors       | Yes      | Full RFC 8620 error catalog                                                                                           |
| References | Back-references (`#arg`)               | Yes      | JSON Pointer evaluation with `*` array flattening                                                                     |
| References | Creation-id references (`#creationId`) | Yes      | Request-wide `createdIds` map                                                                                         |
| Methods    | `Foo/get`, `Foo/changes`, `Foo/set`    | Yes      | PatchObject validation, change coalescing, per-record atomicity                                                       |
| Methods    | `Foo/copy`                             | Yes      | Cross-account copy with `onSuccessDestroyOriginal`                                                                    |
| Methods    | `Foo/query`                            | Yes      | Indexed range scans, in-memory residual, anchors, windowing                                                           |
| Methods    | `Foo/queryChanges`                     | Fallback | Always answers `cannotCalculateChanges` (permitted by section 5.6); clients refetch the query                         |
| State      | State strings and `ifInState`          | Yes      | Optimistic concurrency with `stateMismatch`                                                                           |
| Blobs      | Upload/download endpoints, `Blob/copy` | Yes      | Reference tracking and unreferenced-blob sweeping                                                                     |
| Push       | EventSource (`/eventsource`)           | Yes      | `types`, `closeafter`, `ping` arguments                                                                               |
| Push       | `PushSubscription` webhooks            | Yes      | Verification flow, RFC 8291 payload encryption                                                                        |

</details>

<details>
<summary>Design decisions worth knowing about</summary>

Where the RFCs leave a behavior to the server, the choice is recorded here so
embedders know what to expect. The entries below concern the mail module
(RFC 8621); see Mail below.

**Threads never merge.** Emails are grouped into Threads by their
References/In-Reply-To chain plus a normalized subject. If two existing
Threads later turn out to be one conversation (the message linking them
arrives late), they stay separate: the late message joins the first Thread
it matches. RFC 8621 section 3 leaves the algorithm server-defined, and this
matches Gmail's behavior. Merging would require destroying and re-creating
Email objects, because a Thread id is immutable once assigned. Splits are
rare in practice: replies carry their full ancestor chain in References, so
one missing message almost never breaks the link. The message-id index is
stored permanently, so opt-in merging can be added later as a configuration
option (default off) without a data-model change.

**Unread Thread counts use the trash-aware rules.** RFC 8621 section 2 does
not mandate how a Mailbox's unreadThreads count is calculated and sketches
both a simple and a quality method. This runtime implements the quality
method: Emails that are only in the trash do not make a conversation look
unread in other Mailboxes, and vice versa. There is deliberately no flag to
select the simple method: counts are stored and maintained incrementally, so
switching semantics would require a full recount, and one correct behavior
beats two switchable ones. Accounts with no trash-role Mailbox naturally get
the simple behavior.

**Composing rejects, never repairs.** `Email/set` create generates the RFC
5322 message exactly from the properties given: anything the generator cannot
represent faithfully is an `invalidProperties` SetError, not a silent fix-up.
The server adds only what the spec assigns it, including missing `Date` and
`Message-ID` headers; the domain synthesized Message-IDs live under is
configuration (`mail.WithMessageIDDomain`), never guessed from a hostname.

**The submission records are the queue.** There is no separate outbound queue
store: an `EmailSubmission` with work remaining carries a due-time index
entry, and the sending worker is a reader of that index. The database is the
coordination point - any process sharing the store discovers queued work
through a periodic scan of a tag worklist (worst case one scan interval,
default a minute), an in-process bell is only a latency optimization, and
claims are wall-clock stamps verified under the account lease, so workers
never double-send and a crashed claim is reclaimed after a window. Retries
follow a backoff schedule; abandonment requires both an age past
`GiveUpAfter` and at least `MinAttempts` real attempts, so a long worker
outage cannot instantly bounce stale mail. `ProcessDue` is the manual crank
over the same engine (a queue flush, a pacer, a deterministic test).

**Undo send is cancellation before relay.** `undoStatus` stays `pending`
until a recipient is irrevocably handed to the smarthost, so a client can
cancel any queued submission - including one held by FUTURERELEASE (RFC
4865), which this module implements natively: the hold is `sendAt` in the
queue, nothing is parked on the smarthost. Holds beyond `maxDelayedSend` are
rejected, not clamped.

</details>

## Mail (RFC 8621)

The first datatype module, `datatypes/mail`, implements RFC 8621: Mailbox,
Thread, Email, Identity and EmailSubmission as descriptor types over the
derived RFC 8620 machinery - reading, composing and sending - plus message
delivery (which sits below the JMAP protocol) through LMTP and HTTP ingest
adapters, and a sending worker that relays queued submissions through a
`Submitter` socket (a reference SMTP relay ships; sending is gated by a
deny-by-default `SendPolicy`).

A complete, persistent mail server - the sqlite driver, all five types, both
delivery adapters, sending, and push - is in
[`examples/mailserver`](examples/mailserver/main.go):

```sh
go run ./examples/mailserver
```

It serves JMAP on `:8080`, accepts LMTP on `127.0.0.1:2400`, and takes an HTTP
ingest `POST` on `/ingest`; the file's header walks through creating an Inbox,
delivering a message both ways, and reading it back over JMAP. With no
`-relay` flag it "sends" by delivering to local accounts (loopback mode);
`-relay host:port` relays outbound through a real smarthost.

<details>
<summary>RFC 8621 support matrix</summary>

| Object / method                             | Status  | Notes                                                                                               |
|---------------------------------------------|---------|-----------------------------------------------------------------------------------------------------|
| `Mailbox/get`, `/query`, `/changes`         | Yes     | 18 IANA roles, tree with a depth limit, computed `myRights`                                         |
| `Mailbox/set`                               | Yes     | create/update/destroy, `onDestroyRemoveEmails` cascade                                              |
| Mailbox counters                            | Yes     | `totalEmails`, `unreadEmails`, `totalThreads`, `unreadThreads` (section 2.1, trash-aware)           |
| `Thread/get`, `/changes`                    | Yes     | References + subject grouping; Threads never merge (see Design decisions)                           |
| `Email/get`                                 | Yes     | stored fast fields + on-demand MIME parse; `header:{name}:as{Form}:all` parsed forms                |
| `Email/query`                               | Yes     | every section 4.4.1 condition, section 4.4.2 sort, `collapseThreads`, fast total                    |
| `Email/set` (keywords, mailboxIds, destroy) | Yes     | flag and file existing mail; per-record atomic                                                      |
| `Email/set` (create / compose)              | Yes     | strict-reject message generation from parts (see Design decisions)                                  |
| `Email/import`, `Email/parse`               | Yes     | ingest a blob; parse without storing (`notParsable`, section 4.9, not yet split from serverFail)    |
| `Email/copy`                                | Yes     | cross-account copy with `onSuccessDestroyOriginal`                                                  |
| `SearchSnippet/get`                         | Yes     | highlighted subject and body preview                                                                |
| Delivery (LMTP, HTTP ingest)                | Yes     | transport-agnostic `Deliverer`; RFC 2033 LMTP; host-provided recipient `Resolver`                   |
| `EmailDelivery` push type                   | Yes     | section 1.5 method-less push; state advances on new mail only                                       |
| `Identity/get`, `/changes`, `/set`          | Yes     | section 6 defaults, `SendPolicy`-gated creation, immutable `email`                                  |
| `EmailSubmission` (all methods)             | Yes     | section 7 envelope derivation, section 7.5 error taxonomy, `onSuccessUpdateEmail/Destroy`           |
| Sending worker + SMTP relay                 | Yes     | records-as-queue worker (see Design decisions); reference `Submitter` over SMTP, RFC 3461           |
| FUTURERELEASE (RFC 4865)                    | Yes     | native holds via `sendAt`; over-limit or conflicting holds rejected, not clamped                    |
| `VacationResponse`                          | Planned | own capability (section 8), a later module                                                          |
| Mail/Submission capability objects          | Yes     | `maxMailboxesPerEmail`, `maxSizeAttachmentsPerEmail`, `maxDelayedSend`, etc. (sections 1.3.1/1.3.2) |

Search is a swappable interface (`mail.Searcher`); the built-in implementation
is case-insensitive substring matching. MDN (RFC 9007), S/MIME verification
(RFC 9219), and quotas (RFC 9425) are later datatype modules.

</details>

## Roadmap

naust-jmap is pre-release: no tagged versions yet. The mail module (see Mail
above) reads, composes and sends, over either the sqlite or postgres driver
(the latter including a multi-node cluster hint layer). Coming next, in
order:

- DSN/MDN ingestion into `EmailSubmission` (`dsnBlobIds`, final
  `deliveryStatus`), and `VacationResponse`
- MDN, S/MIME verification, and quotas as further RFC 8621-family modules

## License

[Apache-2.0](LICENSE). See also [NOTICE](NOTICE).
