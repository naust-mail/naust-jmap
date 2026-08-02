# Hardening reference

naust-jmap applies defense-in-depth limits against malformed or hostile input. This
file catalogues them; the vulnerability-reporting policy is in
[SECURITY.md](SECURITY.md). The runtime owns protocol correctness and input
validation; the host owns TLS, authentication, authorization, rate limiting across
connections, and the storage/blob backends. Values below are fixed constants or
suggested defaults - the cited source is authoritative. The core module's
tunable defaults live together in `core/tuning`, whose `Validate` warns at
startup when a value is set below a floor the spec fixes.

<details>
<summary><b>Request and parsing limits</b> - fixed constants against resource exhaustion</summary>

| Limit                    | Value | Bounds                                       | Source                                         |
|--------------------------|-------|----------------------------------------------|------------------------------------------------|
| `maxNestingDepth`        | 1024  | JSON body nesting (stack exhaustion)         | `core/jmap/ijson.go`                           |
| `maxMultipartDepth`      | 64    | MIME multipart nesting                       | `datatypes/mail/internal/message/structure.go` |
| `maxParts`               | 2048  | MIME body parts (breadth)                    | `datatypes/mail/internal/message/structure.go` |
| `maxHeaderValue`         | 64 KB | one header field value (folded, kept linear) | `datatypes/mail/internal/message/header.go`    |
| `maxHeaders`             | 1024  | header fields per block                      | `datatypes/mail/internal/message/header.go`    |
| `MaxFilterNodes`         | 1024  | filter tree breadth                          | `core/tuning/tuning.go`                        |
| `maxBodyProperties`      | 256   | `bodyProperties` per request                 | `datatypes/mail/internal/emailmethods/emailget.go` |
| `MaxRequestedProperties` | 512   | `properties` per `Foo/get`                   | `core/tuning/tuning.go`                        |
| `maxParseProperties`     | 512   | `properties` in `Email/parse`                | `datatypes/mail/internal/emailmethods/emailparse.go` |
| `maxPreviewCapture`      | 256 KB| preview text retained per message (breadth)  | `datatypes/mail/internal/parse/parse.go`       |
| `DefaultMaxChanges`      | 2048  | `Foo/changes` page when `maxChanges` omitted | `core/tuning/tuning.go`                        |
| `MaxDepth`               | 10000 | JSON scan depth (scanner, distinct from decode) | `core/internal/jsonscan/jsonscan.go`        |
| `maxHeaderName`          | 1024  | one header field name                        | `datatypes/mail/internal/message/header.go`    |
| `maxDelimLine`           | 4096  | prefix inspected for a MIME boundary         | `datatypes/mail/internal/message/walk.go`      |
| `MaxReportCapture`       | 64 KB | DSN/MDN report content retained per part     | `datatypes/mail/internal/parse/report.go`      |
| `maxKeywordsPerEmail`    | 100   | keywords settable on one Email               | `datatypes/mail/internal/emailmethods/validate.go` |

The MIME parser is streaming: it never holds the whole message or a whole
decoded body part. `maxHeaderValue`, `maxHeaders`, `maxParts`, and
`maxMultipartDepth` bound each dimension so a single message cannot exhaust
memory. `maxPreviewCapture` bounds the preview across all parts of one message,
so a message of many small text parts cannot multiply the per-part preview
budget into a large retained buffer.

</details>

<details>
<summary><b>Configurable capability limits</b> - tune per deployment (RFC 8620 section 2)</summary>

| Capability                            | Suggested default | Bounds                                    |
|---------------------------------------|-------------------|-------------------------------------------|
| `maxSizeRequest`                      | 10 MB             | API request body, enforced before parsing |
| `maxSizeUpload`                       | 50 MB             | blob upload size                          |
| `maxConcurrentRequests`               | 4                 | concurrent API requests                   |
| `maxConcurrentUpload`                 | 4                 | concurrent uploads                        |
| `maxCallsInRequest`                   | 16                | method calls per request                  |
| `maxObjectsInGet` / `maxObjectsInSet` | 500               | objects per get / set                     |

These are per-connection/per-request. Rate limiting across requests and connections,
and TLS, are the host's responsibility.

Bounds that apply per user or per unit of server work, rather than per request
(all in `core/tuning/tuning.go`):

| Tunable                             | Default | Bounds                                                |
|-------------------------------------|---------|-------------------------------------------------------|
| `MaxConnectionsPerUser`             | 64      | simultaneous push/EventSource/WebSocket connections    |
| `MaxConcurrentRequestsPerUser`      | derived | execution slots one username may hold across every transport; 0 derives half the shared pool, floored at 1, and a value not below the pool is clamped to pool-1 |
| `MaxPushSubscriptionsPerCredential` | 16      | PushSubscription records one credential may hold       |
| `MaxMultiGetBatch`                  | 200     | keys per backend multi-get                             |
| `QueryChangesMaxWork`               | 4096    | rows a `Foo/queryChanges` may scan before giving up    |
| `ChangeLogMaxEntries`               | 100000  | retained change-log entries per account                |

</details>

<details>
<summary><b>Message delivery limits</b> - the LMTP/HTTP ingest path faces untrusted senders</summary>

| Limit                | Value                | Bounds                                           | Reference           |
|----------------------|----------------------|--------------------------------------------------|---------------------|
| `maxCommandLine`     | 1024                 | LMTP command line length                         | RFC 5321 section 4.5.3.1.4 |
| `maxRecipients`      | 128                  | recipients accepted per transaction              | -                   |
| `maxRcptAttempts`    | 1024                 | `RCPT` commands per transaction                  | -                   |
| `maxDrain`           | 64 KB                | body drained after a mid-DATA reject, then close | -                   |
| `lmtpCommandTimeout` | 5 min                | idle wait for the next command                   | RFC 5321 section 4.5.3.2.7 |
| `lmtpDataTimeout`    | 10 min               | DATA body phase                                  | RFC 5321 section 4.5.3.2.6 |
| message size         | 50 MB (configurable) | rejected as it streams past, before store        | -                   |
| `ThreadSizeCap`      | 1024                 | Emails per thread (threading work)               | RFC 8621 section 3         |
| LMTP connections     | 64 (configurable)    | connections served at once; excess gets 421      | RFC 5321 section 3.8       |
| HTTP ingest in flight| 64 (configurable)    | requests served at once; excess gets 503         | -                   |

The ceiling on an ingest is how many connections it serves, not how many
messages it parses: a delivery streams, so a sender in flight costs a buffer,
not a message. A connection past the LMTP ceiling is answered `421` and closed;
a request past the HTTP ceiling is answered `503` with `Retry-After`. Both are
tunable (`LMTPConfig.MaxConnections`, `HTTPIngestConfig.MaxInFlight`). The message size
limit is enforced on the octets as they arrive, so an oversize message is
rejected before its excess is read.


Outbound bounds face a client rather than a sender, but bound the same way:

| Limit                    | Value  | Bounds                                          | Source                        |
|--------------------------|--------|-------------------------------------------------|-------------------------------|
| `MaxRecipients`          | 100    | envelope `rcptTo` per submission                 | `datatypes/mail/submit/capability.go` |
| `MaxMessageBytes`        | 75 MB  | a message this server will send                  | `datatypes/mail/submit/capability.go` |
| `mdnAssemblyBytes`       | 2 MB   | one MDN assembled in memory                      | `capabilities/mdn/send.go`    |
| `mdnOriginalWholeBytes`  | 1 MB   | returned original carried whole; larger is header-only (RFC 6522 section 4) | `capabilities/mdn/send.go` |
| `maxTextGrants`          | 8      | text parts granted capture when parsing an MDN   | `datatypes/mail/report/parsemdn.go` |

Source: `datatypes/mail/deliver/lmtp.go`, `datatypes/mail/deliver/httpingest.go`,
`datatypes/mail/deliver/delivery.go`, `datatypes/mail/internal/emailstore/thread.go`.

</details>

<details>
<summary><b>WebSocket limits</b> - a directly network-facing surface (RFC 6455, RFC 8887)</summary>

| Limit                 | Value  | Bounds                                              | Source                                  |
|-----------------------|--------|-----------------------------------------------------|-----------------------------------------|
| `MaxMessageSize`      | 10 MB  | one coalesced message, fragments included           | `capabilities/websocket/tuning.go`      |
| `MaxFragments`        | 1024   | frames one message may span                         | `capabilities/websocket/tuning.go`      |
| `allocChunk`          | 32 KB  | how far payload allocation may run ahead of bytes received | `capabilities/websocket/internal/frame/frame.go` |
| `MaxRequestIDLength`  | 256    | client `requestId` echoed in a response             | `capabilities/websocket/tuning.go`      |
| `LaneCap`             | 2      | requests processed concurrently per connection      | `capabilities/websocket/tuning.go`      |
| `IdleTimeout`         | 10 min | silence on a connection with nothing in flight      | `capabilities/websocket/tuning.go`      |
| `MessageDeadline`     | 2 min  | assembling one fragmented message                   | `capabilities/websocket/tuning.go`      |
| `WriteDeadline`       | 30 s   | one frame write, so a stalled reader cannot pin a connection | `capabilities/websocket/tuning.go` |
| `DrainDeadline`       | 10 s   | graceful shutdown drain                             | `capabilities/websocket/tuning.go`      |
| `CloseReplyDeadline`  | 5 s    | wait for the peer's Close after sending one         | `capabilities/websocket/tuning.go`      |
| `ReauthInterval`      | 10 min | re-verification when the authenticator has no `auth.Revoker` | `capabilities/websocket/tuning.go` |

`allocChunk` is the load-bearing one against a hostile peer: a frame header may
declare an enormous payload, but the reassembly buffer grows with octets actually
received, never with a merely claimed length (RFC 6455 section 10.4), so claiming
a large message costs the sender the bandwidth to send it.

| Guard                | Behaviour                                                                                          |
|----------------------|-----------------------------------------------------------------------------------------------------|
| Origin policy        | a handshake from a disallowed browser origin is refused 403 before authentication runs, so a hostile page never reaches the authenticator (RFC 6455 section 10.2). With none stated, same-origin only |
| Masking required     | an unmasked client frame fails the connection with 1002 (section 5.1)                                |
| Control frame bounds | control frames are rejected if fragmented or over 125 octets, and reserved opcodes fail the connection (section 5.5) |
| Minimal length coding | non-minimal 16- and 64-bit lengths, and a 64-bit length with the high bit set, fail the connection (section 5.2) |
| UTF-8 validation     | text payloads and close reasons are validated incrementally, correct across a rune split between fragments (section 8.1) |
| Write serialization  | every writer holds one mutex across a whole frame, so push, responses and control frames cannot interleave |

</details>

<details>
<summary><b>Structural guards</b> - behavioural, not numeric</summary>

| Guard                       | Behaviour                                                                                                         | Source                                  |
|-----------------------------|-------------------------------------------------------------------------------------------------------------------|-----------------------------------------|
| Envelope address validation | reject control characters (C0/C1/DEL) in a path; graphic ASCII + UTF-8 only (RFC 5321 section 4.1.2, RFC 6531)           | `datatypes/mail/deliver/lmtp.go`        |
| Delivery fault isolation    | a panic while serving a connection or delivering is recovered, never a process crash; verdicts already decided are kept and undecided recipients default to a transient failure (the safe default) | `datatypes/mail/deliver/lmtp.go`, `datatypes/mail/deliver/delivery.go` |
| Streaming blob I/O          | upload, download, and `Blob/copy` never buffer a whole blob; the chunked store holds one piece at a time          | `core/providers/blob/chunkstore`        |
| Streaming MIME parse        | delivery, import, `Email/get`, `Email/parse`, and search stream the blob through the parser; no path buffers a whole message or a whole decoded part | `datatypes/mail/internal/message`, `datatypes/mail/internal/parse` |
| Linear body search          | the naive body matcher scans in fixed batches, retaining only a term-width tail, so work stays linear in the body regardless of chunking | `datatypes/mail/search`                 |
| Malformed-input degradation | parsing never fails on malformed structure; it degrades to the closest sensible shape (a boundaryless multipart becomes a text leaf; base64 stops at its padding) rather than erroring or looping | `datatypes/mail/internal/message`       |
| Backend capacity budget     | an over-capacity write is rejected with `ErrNoSpace`, applying nothing                                            | `core/providers/backend`                |
| Patch overlap check         | linear set-based prefix check, not O(P^2)                                                                          | `core/runtime` (`applyPatch`)           |
| Thread assignment           | O(N) via a composite index, not O(N^2) per thread                                                                  | `datatypes/mail/internal/emailstore` (`threadKeys`) |
| Panic disclosure            | a recovered panic is logged server-side; the client gets a generic "internal error", never the panic value        | `core/runtime/processor.go`             |

</details>
