# quotas

[![Go Reference](https://pkg.go.dev/badge/github.com/naust-mail/naust-jmap/capabilities/quotas.svg)](https://pkg.go.dev/github.com/naust-mail/naust-jmap/capabilities/quotas)

Resource quotas over JMAP (RFC 9425) as an optional capability: read-only
`Quota` records reporting an account's limits and its current usage, with
`Quota/get`, `Quota/changes`, `Quota/query` and `Quota/queryChanges` derived
from the core runtime. Depends on `core` alone - no mail module, no
third-party packages.

```sh
go get github.com/naust-mail/naust-jmap/capabilities/quotas
```

## Overview

A Quota record answers two separate questions, and this module keeps their
answers on separate paths.

**What are the limits?** They are the embedder's rules, not this package's
data. Supply them either as hand-written records through `Upsert` and
`Delete`, or by implementing `Source` and letting `Refresh` mirror your system
into the database. Nothing forces a choice; both kinds of record coexist in one
account.

**How much is used?** That is server-computed (RFC 9425 section 4.1), so a
definition never carries it. `AddUsed` and `SetUsed` maintain the counter on
their own small write path, which is what lets `Quota/changes` answer
`updatedProperties: ["used"]` for the frequent usage-only change.

The module never computes usage itself and never enforces a limit. It reports.

## When to use this module

Import it when clients should see their quotas over JMAP. Skip it otherwise:
unimported, it is absent from the binary and from the dependency graph.

Import cost: `core` only.

```go
svc, err := quotas.Register(proc, quotas.Config{DB: db, Core: core})
if err != nil {
    log.Fatal(err)
}
// Advertising the capability is the embedder's step; RFC 9425 section 2.1
// makes both values the empty object.
if err := srv.Capability(quotas.CapabilityURI).
    Advertise(struct{}{}, struct{}{}).Err(); err != nil {
    log.Fatal(err)
}
```

## Public API

| Name                       | Purpose                                                          |
|----------------------------|------------------------------------------------------------------|
| `Register`                 | Registers the Quota type and its methods; returns the `Service`  |
| `Config`                   | `DB` (required), `Core`, `Source`, `TypeCapabilities`            |
| `Service.Upsert`           | Writes one hand-written definition; empty `Id` creates           |
| `Service.Delete`           | Destroys one record                                              |
| `Service.Refresh`          | Pulls a `Source` and commits only real differences               |
| `Service.AddUsed`          | Moves the usage counter by a signed delta                        |
| `Service.SetUsed`          | Sets the usage counter to an absolute value                      |
| `Source`                   | Interface supplying definitions from the system that owns them   |
| `Quota`                    | One definition, as supplied to `Upsert` or returned by a `Source`|
| `CapabilityURI`, `TypeName`| `urn:ietf:params:jmap:quota`, `Quota`                            |
| `DefaultTypeCapabilities`  | The IANA type-name-to-capability table used for types filtering  |

## Concepts

### Definitions come from wherever you keep the rules

A `Source` is pulled per account and returns that account's full definition
set. It exists so limits that are really rules - subscription tiers, a fleet
controller's policy - stay in the system that owns them instead of being
materialized per user twice. Ids must be stable across pulls; that is how
`Refresh` recognizes a definition it has already mirrored.

`Refresh` diffs the pull against the mirror and commits only what actually
differs: a repeated pull of unchanged rules stages nothing, so state strings
stay put and no push fires. A definition that disappears from the source is
destroyed; a definition that changes is updated in place, so client caches and
the usage counter survive a limit change. Records written by hand carry no
source id and are never touched by a refresh.

The mirrored copy is not a second place to manage quotas - it is the
protocol's memory. State strings, the change log and push all require stored
records with a history, so a JMAP server has to have one.

### Usage is a counter, and counters drift

`AddUsed` is the hot path: one small write, no source call, no diff. Because it
changes exactly one property, `Quota/changes` can promise
`updatedProperties: ["used"]` and a client can refetch just the number.

Increment-maintained counters drift - a delete whose size nobody recorded, a
restart mid-write - so treat a periodic `SetUsed` recount as part of the design
rather than as a repair. A delta that would drive usage below zero is pinned at
zero and logged: usage cannot be negative, and the recount is what restores the
true figure. A delta so large it leaves the representable range is refused
outright, leaving the stored value untouched.

### Shared-scope quotas are withheld by default

A quota whose scope is `domain` or `global` counts a resource other people
share, so watching it move reports their activity - RFC 9425 section 8 gives
the example of learning a private list's subscriber count by sending it a
message and reading the delta. Those scopes are therefore hidden from clients
by default; only `account` scope is returned.

Deciding who is an administrator is your knowledge, not this package's, so
widening the rule is a `Config.ScopeVisible` predicate. It runs per request and
receives the request context, which your own middleware can carry a verdict on:

```go
quotas.Config{
    DB: db, Core: core,
    ScopeVisible: func(ctx context.Context, _ jmap.Id, scope string) bool {
        return scope == "account" || isAdmin(ctx)
    },
}
```

### Clients only see quotas that mean something to them

RFC 9425 section 4.1 requires the server to filter each Quota's `types` list to
the capabilities the request opted into, and to withhold entirely any Quota
with no type the client recognizes. Both happen per request. A mail-only client
never learns that a calendar quota exists.

Type names map to capabilities through `DefaultTypeCapabilities`, which mirrors
the IANA "JMAP Data Types" registry; pass your own table through
`Config.TypeCapabilities` for vendor types.

Because visibility depends on the caller rather than on the record,
`Quota/query` reports `canCalculateChanges: false` and `Quota/queryChanges`
answers `cannotCalculateChanges` - the RFC 8620 section 5.6 escape hatch that
tells a client to re-run the query. `Quota/changes` is deliberately unfiltered:
it carries ids only, and a client whose cached copy went stale has to hear
about it even while the record is hidden.

### Enforcement is somewhere else

This module reports; it does not refuse writes. Enforcing a limit belongs to
whatever owns the operation being limited, which is where the knowledge of what
is being created lives. RFC 8620 already provides the vocabulary for the
refusal: the `overQuota` `SetError`.

## Examples

```go
// Definitions from your own system.
type tierSource struct{ billing *billing.Client }

func (s *tierSource) Quotas(ctx context.Context, acct jmap.Id) ([]quotas.Quota, error) {
    tier, err := s.billing.TierFor(ctx, string(acct))
    if err != nil {
        return nil, err
    }
    return []quotas.Quota{{
        Id:           "storage",
        ResourceType: "octets",
        HardLimit:    tier.StorageBytes,
        Scope:        "account",
        Name:         "storage",
        Types:        []string{"Email"},
    }}, nil
}

svc, err := quotas.Register(proc, quotas.Config{DB: db, Core: core, Source: &tierSource{billing: b}})

// Re-pull when your rules change, or on a schedule.
err = svc.Refresh(ctx, acct)

// Track usage as it moves.
err = svc.AddUsed(ctx, acct, quotaId, int64(len(message)))
```

The `mailserver` example wires a periodic recount over stored Emails through
`maintain.Config.Extra`.

## Status and compatibility

Pre-release, tagged v0.1.0. Requires `core` v0.4.0 or later.

Known gap: RFC 9425 section 4.1 requires a quota's `description` to be selected
from the request's `Accept-Language` header **or** from out-of-band knowledge of
the user's locale. The second route works today - descriptions are supplied per
account, so an embedder who knows the account holder's language stores the right
one. The first does not: this module stores a single description per record and
returns it unchanged, and nothing in the stack carries a request language,
because RFC 8620 section 3.8 (localisation of user-visible strings) is not yet
implemented in `core`. When it is, this module should follow it.

## Related modules

| Module               | Relationship                                          |
|----------------------|-------------------------------------------------------|
| [`core`](../../core) | The runtime this capability registers into. Required  |
