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
| `maxParts`               | 10000 | MIME body parts (breadth)                    | `datatypes/mail/internal/message/structure.go` |
| `maxHeaderValue`         | 64 KB | one header field value (folded, kept linear) | `datatypes/mail/internal/message/header.go`    |
| `maxHeaders`             | 1024  | header fields per block                      | `datatypes/mail/internal/message/header.go`    |
| `MaxFilterNodes`         | 1024  | filter tree breadth                          | `core/tuning/tuning.go`                        |
| `maxBodyProperties`      | 256   | `bodyProperties` per request                 | `datatypes/mail/emailget.go`                   |
| `MaxRequestedProperties` | 512   | `properties` per `Foo/get`                   | `core/tuning/tuning.go`                        |
| `maxParseProperties`     | 512   | `properties` in `Email/parse`                | `datatypes/mail/emailparse.go`                 |
| `maxPreviewCapture`      | 256 KB| preview text retained per message (breadth)  | `datatypes/mail/parse.go`                      |
| `DefaultMaxChanges`      | 2048  | `Foo/changes` page when `maxChanges` omitted | `core/tuning/tuning.go`                        |

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
| `threadSizeCap`      | 1024                 | Emails per thread (threading work)               | RFC 8621 section 3         |
| LMTP connections     | 64 (configurable)    | connections served at once; excess gets 421      | RFC 5321 section 3.8       |
| HTTP ingest in flight| 64 (configurable)    | requests served at once; excess gets 503         | -                   |

The ceiling on an ingest is how many connections it serves, not how many
messages it parses: a delivery streams, so a sender in flight costs a buffer,
not a message. A connection past the LMTP ceiling is answered `421` and closed;
a request past the HTTP ceiling is answered `503` with `Retry-After`. Both are
tunable (`WithMaxLMTPConnections`, `WithMaxIngestInFlight`). The message size
limit is enforced on the octets as they arrive, so an oversize message is
rejected before its excess is read.

Source: `datatypes/mail/lmtp.go`, `datatypes/mail/httpingest.go`,
`datatypes/mail/delivery.go`, `datatypes/mail/thread.go`.

</details>

<details>
<summary><b>Structural guards</b> - behavioural, not numeric</summary>

| Guard                       | Behaviour                                                                                                         | Source                                  |
|-----------------------------|-------------------------------------------------------------------------------------------------------------------|-----------------------------------------|
| Envelope address validation | reject control characters (C0/C1/DEL) in a path; graphic ASCII + UTF-8 only (RFC 5321 section 4.1.2, RFC 6531)           | `datatypes/mail/lmtp.go`                |
| Delivery fault isolation    | a panic while serving a connection or delivering is recovered, never a process crash; verdicts already decided are kept and undecided recipients default to a transient failure (the safe default) | `datatypes/mail/lmtp.go`, `delivery.go` |
| Streaming blob I/O          | upload, download, and `Blob/copy` never buffer a whole blob; the chunked store holds one piece at a time          | `core/providers/blob/chunkstore`        |
| Streaming MIME parse        | delivery, import, `Email/get`, `Email/parse`, and search stream the blob through the parser; no path buffers a whole message or a whole decoded part | `datatypes/mail/internal/message`, `datatypes/mail/parse.go` |
| Linear body search          | the naive body matcher scans in fixed batches, retaining only a term-width tail, so work stays linear in the body regardless of chunking | `datatypes/mail/search.go`              |
| Malformed-input degradation | parsing never fails on malformed structure; it degrades to the closest sensible shape (a boundaryless multipart becomes a text leaf; base64 stops at its padding) rather than erroring or looping | `datatypes/mail/internal/message`       |
| Backend capacity budget     | an over-capacity write is rejected with `ErrNoSpace`, applying nothing                                            | `core/providers/backend`                |
| Patch overlap check         | linear set-based prefix check, not O(P^2)                                                                          | `core/runtime` (`applyPatch`)           |
| Thread assignment           | O(N) via a composite index, not O(N^2) per thread                                                                  | `datatypes/mail` (`threadKeys`)         |
| Panic disclosure            | a recovered panic is logged server-side; the client gets a generic "internal error", never the panic value        | `core/runtime/processor.go`             |

</details>
