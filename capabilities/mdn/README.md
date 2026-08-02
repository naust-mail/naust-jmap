# mdn

[![Go Reference](https://pkg.go.dev/badge/github.com/naust-mail/naust-jmap/capabilities/mdn.svg)](https://pkg.go.dev/github.com/naust-mail/naust-jmap/capabilities/mdn)

Message Disposition Notifications over JMAP (RFC 9007) as an optional
capability: `MDN/send` issues a read receipt for a message that requested one
(RFC 8098), and `MDN/parse` reads a received receipt back into a JMAP MDN
object. Built entirely on the mail module's public seams.

```sh
go get github.com/naust-mail/naust-jmap/capabilities/mdn
```

## Overview

One package, two methods, one capability URI. `MDN/send` validates the
message's `Disposition-Notification-To` request, assembles the
`multipart/report` receipt, and sends it through the same submission queue and
sending policy as every other outbound message - blob, `EmailSubmission`,
`$mdnsent` keyword and the server's own one-MDN-per-message record all commit
together under the account lease. `MDN/parse` opens receipt blobs the account
can reach, parses them strictly, and correlates `forEmailId` through the
submission queue's Message-ID index.

## When to use this module

Import it when clients should issue or read receipts over JMAP. Skip it
otherwise: unimported, it is absent from the binary and from the dependency
graph.

Import cost: `datatypes/mail` (which this module rides on) and its
`golang.org/x/text` dependency - nothing further.

```go
// The Email type must carry this module's internal property...
mail.RegisterEmail(proc, mail.EmailConfig{
    DB: db, Store: blobs, Core: core,
    InternalProperties: mdn.EmailInternalProperties(),
})

// ...then register against the submission queue and advertise.
queue, _ := submit.Register(proc, submit.Config{DB: db, Store: blobs, Core: core, Policy: policy})
mdn.Register(proc, mdn.Config{DB: db, Store: blobs, Core: core, Queue: queue})
srv.Capability(mdn.CapabilityURI).Advertise(struct{}{}, struct{}{})
```

Register verifies the Email descriptor carries the internal property, so a
missing wiring step is a startup error rather than a runtime surprise.

## Public API

| Symbol                      | Kind  | What it is                                                                     |
|-----------------------------|-------|--------------------------------------------------------------------------------|
| `Register(proc, Config)`    | func  | Registers `MDN/send` and `MDN/parse` under the capability                      |
| `Config`                    | type  | `DB`, `Store`, `Core`, `Queue` - all required                                  |
| `EmailInternalProperties()` | func  | The internal Email property declaration to pass through `EmailConfig`          |
| `MDN`                       | type  | The RFC 9007 section 2 object, both methods' wire shape                        |
| `Disposition`               | type  | The `actionMode` / `sendingMode` / `type` triple (closed vocabularies)         |
| `CapabilityURI`             | const | `urn:ietf:params:jmap:mdn`                                                     |

## Concepts

**One MDN per message, ever.** RFC 8098 section 2.1. The `$mdnsent` keyword is
the client-visible marker; the server keeps its own internal record on the
Email, written in the same commit as the send. Both are re-checked under the
account lease, so concurrent sends race to exactly one receipt and the loser
gets `mdnAlreadySent`.

**Automatic mode is guarded.** An automatic-mode send requires the
notification address to equal the `Return-Path` address (local part
case-sensitive, domain not), refuses messages that are themselves automatic
(RFC 3834), and an MDN is never issued for an MDN. A manual-mode send carries
the user's own judgment instead. Receipts go out with a null reverse-path and
`NOTIFY=NEVER`, so they can never loop.

**Notification options suppress.** RFC 8098 section 2.2: a
`Disposition-Notification-Options` parameter of `required` importance that is
not understood suppresses the MDN, and honoring `optional`-only parameters is
a MAY. No parameters are supported (the IANA registry holds only the RFC 3335
and RFC 3297 legacy attributes), so the field's presence refuses the send in
both modes; its contents are never interpreted.

**The sending policy still applies.** The queue's registered `SendPolicy` is
re-checked at send time against the resolved From address; a wildcard
Identity is a permission, not an address, so the client must name
`finalRecipient` explicitly.

**A generated receipt always parses back.** Assembly is bounded by the same
capture bound the parser enforces, so `MDN/send` cannot produce a receipt
`MDN/parse` rejects. An original message too large to return whole is
returned as its header block (`text/rfc822-headers`, RFC 6522 section 4)
rather than failing the send.

## Examples

The wiring block above is the complete integration - three calls on
`examples/mailserver`'s seams. The example server does not wire it by
default: many deployments treat disposition notifications as tracking and
decline to answer them, which is a choice each embedder should make
deliberately.

## Status and compatibility

Pre-release, tagged v0.1.1. Requires `core` v0.3.0 or later and
`datatypes/mail` v0.3.2 or later.

## Related modules

| Module                                   | Relationship                                                              |
|------------------------------------------|----------------------------------------------------------------------------|
| [`core`](../../core)                     | The runtime this capability registers into. Required                      |
| [`datatypes/mail`](../../datatypes/mail) | Provides the Email/Identity views, the report format, and the queue. Required |
