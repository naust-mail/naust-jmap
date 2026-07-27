# mail

[![Go Reference](https://pkg.go.dev/badge/github.com/naust-mail/naust-jmap/datatypes/mail.svg)](https://pkg.go.dev/github.com/naust-mail/naust-jmap/datatypes/mail)

The RFC 8621 datatype plugin: Mailbox, Thread, Email, Identity and
EmailSubmission as descriptor types over the derived RFC 8620 machinery, plus
the parts of a mail server that sit below the JMAP protocol - delivery, sending,
and vacation responses.

```sh
go get github.com/naust-mail/naust-jmap/datatypes/mail
```

## Contents

- [Overview](#overview)
- [When to use this module](#when-to-use-this-module)
- [Public API](#public-api)
- [Concepts](#concepts)
- [Extension points](#extension-points)
- [Design decisions](#design-decisions)
- [Protocol support](#protocol-support)
- [Examples](#examples)
- [Status and compatibility](#status-and-compatibility)
- [Related modules](#related-modules)

## Overview

The first datatype module, `datatypes/mail`, implements RFC 8621: Mailbox,
Thread, Email, Identity and EmailSubmission as descriptor types over the
derived RFC 8620 machinery - reading, composing and sending - plus message
delivery (which sits below the JMAP protocol) through LMTP and HTTP ingest
adapters, and a sending worker that relays queued submissions through a
`Submitter` socket (a reference SMTP relay ships; sending is gated by a
deny-by-default `SendPolicy`).

It is one Go package. Everything below is `mail.X`.

## When to use this module

Import it when you are serving mail over JMAP. It is the only datatype module
today, and it is the reference for what a datatype plugin looks like - but it is
not privileged: it gets no table, method, index or state that another datatype
could not also have.

Import cost: `golang.org/x/text` (character-set decoding for MIME). Everything
else is standard library.

Registering the types gives you JMAP. Mail also has to arrive and leave, and
those two paths sit below the protocol:

```go
// Inbound. One engine, two adapters; resolver is yours - it decides who a
// recipient is and whether to accept them.
d := mail.NewDeliverer(db, blobs, myResolver)
go mail.ServeLMTP(lmtpLn, d, "mx.example.com")   // behind an MTA
mux.Handle("/ingest", mail.NewHTTPIngest(d))     // or over plain HTTP

// Outbound. The queue is the submission records; the worker reads them.
relay, _ := mail.NewSMTPRelay(mail.SMTPRelayConfig{Addr: "smarthost.example.com:587"})
w, _ := mail.NewSubmissionWorker(queue, relay, mail.SubmissionWorkerConfig{})
go w.Run(ctx)
```

## Public API

godoc is the reference. This table is the map: the entry points a consumer
actually calls, grouped by what you are trying to do.

**Registering the types**

| Symbol                                                                                               | Kind  | What it is                                                                     |
|------------------------------------------------------------------------------------------------------|-------|--------------------------------------------------------------------------------|
| `RegisterMailbox`, `RegisterThread`, `RegisterEmail`                                                 | func  | Register the three reading types into a `runtime.Processor`                    |
| `RegisterIdentity`, `RegisterEmailSubmission`                                                        | func  | Register the sending types; the latter returns the `SubmissionQueue`           |
| `RegisterVacationResponse`                                                                           | func  | Register the section 8 singleton                                               |
| `MailboxType`, `ThreadType`, `EmailType`, `IdentityType`, `EmailSubmissionType`, `EmailDeliveryType` | func  | The underlying `descriptor.Type` values, if you need to inspect or extend them |
| `CapabilityURI`, `SubmissionCapabilityURI`, `VacationCapabilityURI`                                  | const | The capability URIs to advertise                                               |
| `TypeEmail`, `TypeMailbox`, ...                                                                      | const | JMAP type names, for push type sets and capability wiring                      |

<details>
<summary><b>Capability objects</b> - the advertised limits</summary>

| Symbol                                                          | Kind        | What it is                                                  |
|-----------------------------------------------------------------|-------------|-------------------------------------------------------------|
| `AccountCapability`, `DefaultAccountCapability`                 | type / func | Section 1.3.1 mail limits                                   |
| `SubmissionLimits`, `DefaultSubmissionLimits`                   | type / func | Section 1.3.2 submission limits, including `maxDelayedSend` |
| `SubmissionAccountCapability`, `SubmissionAccountCapabilityFor` | type / func | The advertised submission capability object                 |

</details>

**Delivery (below JMAP)**

| Symbol                                              | Kind             | What it is                                                                      |
|-----------------------------------------------------|------------------|---------------------------------------------------------------------------------|
| `NewDeliverer`                                      | func             | The transport-agnostic delivery engine                                          |
| `Deliverer.Deliver(ctx, env, r)`                    | method           | Deliver one message. The method every adapter calls; returns per-recipient events |
| `Deliverer.MaxMessageSize()`                        | method           | The configured cap, for an adapter that must reject early                       |
| `ServeLMTP`                                         | func             | RFC 2033 LMTP server over a listener                                            |
| `NewHTTPIngest`                                     | func             | Plain HTTP ingest endpoint                                                      |
| `HTTPIngest.ServeHTTP`                              | method           | It is an `http.Handler`; mount it on your mux                                   |
| `Resolver`, `RecipientResult`, `Outcome`            | interface / type | How the host maps a recipient to an account, and the per-recipient verdict      |
| `DelivererOption`, `LMTPOption`, `HTTPIngestOption` | func type        | Size caps, connection caps, report ingestion, vacation responder, delivery sink |
| `DeliverySink`, `DeliveryEvent`                     | interface / type | Observe deliveries as they land                                                 |

**Sending**

| Symbol                                                                   | Kind             | What it is                                            |
|--------------------------------------------------------------------------|------------------|-------------------------------------------------------|
| `SubmissionQueue`                                                        | type             | The queue, which is the submission records themselves |
| `NewSubmissionWorker`, `SubmissionWorkerConfig`, `SubmissionWorkerStats` | func / type      | The worker that drains due submissions                |
| `SubmissionWorker.Run(ctx)`                                              | method           | Start sending. Blocks until the context is cancelled  |
| `SubmissionWorker.ProcessDue(ctx, limit)`                                | method           | The manual crank: a queue flush, a pacer, a test      |
| `SubmissionWorker.Stats()`                                               | method           | Read the worker's counters at runtime                 |
| `Submitter`                                                              | interface        | Where outbound mail actually goes                     |
| `NewSMTPRelay`, `SMTPRelayConfig`, `TLSMode`, `PlainAuth`                | func / type      | The reference `Submitter` over SMTP, RFC 3461         |
| `SendPolicy`, `StaticSendPolicy`, `NewStaticSendPolicy`                  | interface / func | Who may send as what. Deny by default                 |
| `StaticSendPolicy.Allow(acct, addrs...)`                                 | method           | Grant an account the addresses it may send as         |
| `SMTPRelay.Submit(ctx, env, msg)`                                        | method           | The `Submitter` implementation the worker calls       |
| `Envelope`, `SubmissionEnvelope`, `SubmissionRecipient`                  | type             | Envelope derivation, RFC 8621 section 7               |

<details>
<summary><b>Everything else</b> - search, message-id domain, migration</summary>


| Symbol                      | Kind             | What it is                                                                  |
|-----------------------------|------------------|-----------------------------------------------------------------------------|
| `Searcher`, `SearchSnippet` | interface / type | Swappable search; the built-in is case-insensitive substring matching       |
| `WithMessageIDDomain`       | func             | The domain synthesized Message-IDs live under. Configuration, never guessed |
| `MigrateThreadCounters`     | func             | One-time counter migration helper                                           |

</details>

## Concepts

**Delivery is not JMAP.** Mail arriving is below the protocol: LMTP and HTTP
ingest are adapters onto one `Deliverer`, and the host decides who a recipient
is through `Resolver`. JMAP only ever sees the result.

**The submission records are the queue.** There is no separate outbound store.
A submission with work remaining carries a due-time index entry, and the worker
is a reader of that index - which is what lets several processes share one
database without double-sending.

**Sending is deny-by-default.** `SendPolicy` gates both Identity creation and
submission. A server that does not install one does not send.

**Threads are assigned, never merged.** See [Design
decisions](#design-decisions); a Thread id is immutable once assigned.

## Extension points

| Interface      | You implement it to...                                | Ships with                                   |
|----------------|-------------------------------------------------------|----------------------------------------------|
| `Resolver`     | Map an envelope recipient to an account, or reject it | Nothing; the host owns its user directory    |
| `SendPolicy`   | Decide who may send as which address                  | `StaticSendPolicy`                           |
| `Submitter`    | Hand outbound mail to the world                       | `SMTPRelay` (reference relay, RFC 3461)      |
| `Searcher`     | Replace substring search with a real index            | Built-in case-insensitive substring matching |
| `DeliverySink` | Observe deliveries                                    | Nothing; optional                            |

## Design decisions

<details>
<summary>Where RFC 8621 is silent, what this module chose</summary>

Where the RFCs leave a behavior to the server, the choice is recorded here so
embedders know what to expect. The core runtime records its own in
[`core`](../../core#protocol-support).

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

## Protocol support

<details>
<summary>RFC 8621 support matrix</summary>

| Object / method                             | Status | Notes                                                                                                  |
|---------------------------------------------|--------|--------------------------------------------------------------------------------------------------------|
| `Mailbox/get`, `/query`, `/changes`         | Yes    | 18 IANA roles, tree with a depth limit, computed `myRights`                                            |
| `Mailbox/set`                               | Yes    | create/update/destroy, `onDestroyRemoveEmails` cascade                                                 |
| Mailbox counters                            | Yes    | `totalEmails`, `unreadEmails`, `totalThreads`, `unreadThreads` (section 2.1, trash-aware)              |
| `Thread/get`, `/changes`                    | Yes    | References + subject grouping; Threads never merge (see Design decisions)                              |
| `Email/get`                                 | Yes    | stored fast fields + on-demand MIME parse; `header:{name}:as{Form}:all` parsed forms                   |
| `Email/query`                               | Yes    | every section 4.4.1 condition, section 4.4.2 sort, `collapseThreads`, fast total                       |
| `Email/set` (keywords, mailboxIds, destroy) | Yes    | flag and file existing mail; per-record atomic                                                         |
| `Email/set` (create / compose)              | Yes    | strict-reject message generation from parts (see Design decisions)                                     |
| `Email/import`, `Email/parse`               | Yes    | ingest a blob; parse without storing (`notParsable`, section 4.9, not yet split from serverFail)       |
| `Email/copy`                                | Yes    | cross-account copy with `onSuccessDestroyOriginal`                                                     |
| `SearchSnippet/get`                         | Yes    | highlighted subject and body preview                                                                   |
| Delivery (LMTP, HTTP ingest)                | Yes    | transport-agnostic `Deliverer`; RFC 2033 LMTP; host-provided recipient `Resolver`                      |
| `EmailDelivery` push type                   | Yes    | section 1.5 method-less push; state advances on new mail only                                          |
| `Identity/get`, `/changes`, `/set`          | Yes    | section 6 defaults, `SendPolicy`-gated creation, immutable `email`                                     |
| `EmailSubmission` (all methods)             | Yes    | section 7 envelope derivation, section 7.5 error taxonomy, `onSuccessUpdateEmail/Destroy`              |
| Sending worker + SMTP relay                 | Yes    | records-as-queue worker (see Design decisions); reference `Submitter` over SMTP, RFC 3461              |
| FUTURERELEASE (RFC 4865)                    | Yes    | native holds via `sendAt`; over-limit or conflicting holds rejected, not clamped                       |
| Trace stamping at delivery                  | Yes    | `Return-Path` + `Received` prefixed as the message streams in (RFC 5321 section 4.4); no FOR clause    |
| DSN/MDN ingestion                           | Yes    | ENVID = submission id (RFC 3461); RFC 3464/8098 parsed into `deliveryStatus`/`dsnBlobIds`/`mdnBlobIds` |
| `VacationResponse/get`, `/set`              | Yes    | section 8 singleton; delivery-side responder per RFC 3834 through the one submission queue             |
| Mail/Submission capability objects          | Yes    | `maxMailboxesPerEmail`, `maxSizeAttachmentsPerEmail`, `maxDelayedSend`, etc. (sections 1.3.1/1.3.2)    |

Search is a swappable interface (`mail.Searcher`); the built-in implementation
is case-insensitive substring matching. MDN (RFC 9007), S/MIME verification
(RFC 9219), and quotas (RFC 9425) are later datatype modules.

</details>

## Examples

A complete, persistent mail server - the sqlite driver, all five types, both
delivery adapters, sending, and push - is in
[`examples/mailserver`](../../examples/mailserver):

```sh
go run ./examples/mailserver
```

It serves JMAP on `:8080`, accepts LMTP on `127.0.0.1:2400`, and takes an HTTP
ingest `POST` on `/ingest`; the file's header walks through creating an Inbox,
delivering a message both ways, and reading it back over JMAP. With no
`-relay` flag it "sends" by delivering to local accounts (loopback mode);
`-relay host:port` relays outbound through a real smarthost.

## Status and compatibility

Pre-release, tagged v0.1.0. Breaking changes may land in minor bumps until 1.0.
Requires `core` v0.1.0 or later.

Not yet implemented as separate datatype modules: MDN send/parse (RFC 9007),
S/MIME verification (RFC 9219), quotas (RFC 9425).

## Related modules

| Module                                                   | Relationship                                        |
|----------------------------------------------------------|-----------------------------------------------------|
| [`core`](../../core)                                     | The runtime this plugin registers into. Required    |
| [`drivers/sqlite`](../../drivers/sqlite)                 | Persistence for a single-node mail server           |
| [`drivers/postgres`](../../drivers/postgres)             | Persistence for a multi-node one                    |
| [`capabilities/websocket`](../../capabilities/websocket) | Adds RFC 8887 transport; independent of this module |

For task-oriented guidance see the documentation site:
[mail](https://naust.email/naust-jmap/mail) and the full
[reference matrices](https://naust.email/naust-jmap/reference).
