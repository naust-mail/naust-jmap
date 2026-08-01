# postgres driver

[![Go Reference](https://pkg.go.dev/badge/github.com/naust-mail/naust-jmap/drivers/postgres.svg)](https://pkg.go.dev/github.com/naust-mail/naust-jmap/drivers/postgres)

A `backend.Backend` implementation over a single Postgres database, one kv
table - plus the optional cluster hint transport that lets several processes
share it without waiting on timers.

```sh
go get github.com/naust-mail/naust-jmap/drivers/postgres
```

## Overview

All JMAP structure - collections, indexes, the change log - is built above the
`backend.Backend` interface by `core/objectdb`, so nothing here knows about the
protocol; the same split as [`drivers/sqlite`](../sqlite).

Beyond storage, this module carries the pieces a fleet needs: a LISTEN/NOTIFY
hint transport for change notifications, lease wakes and credential revocation.

Uses pgx natively (pgxpool), not the `database/sql` adapter: a `Batch` maps
directly onto pgx's pipelining, and LISTEN/NOTIFY is first-class there.

## When to use this module

Use it when more than one process must serve the same accounts - the case
[`drivers/sqlite`](../sqlite) cannot cover - or when you already run Postgres and
want one database to operate.

Import cost: pgx, quarantined to this module.

## Public API

| Symbol                                     | Kind      | What it is                                                                                                                      |
|--------------------------------------------|-----------|---------------------------------------------------------------------------------------------------------------------------------|
| `Open(ctx, dsn string) (*Store, error)`    | func      | Opens the pool and ensures the schema                                                                                           |
| `Store`                                    | type      | The `backend.Backend` implementation. Pass it to `objectdb.New`                                                                 |
| `OpenHints(ctx, store) (*Hints, error)`    | func      | Starts the cluster hint transport over LISTEN/NOTIFY                                                                            |
| `Hints`                                    | type      | A `notify.Notifier` and lease `Waker` that crosses process boundaries                                                           |
| `PublishRevocation(ctx, db, username)`     | func      | Durably records a credential revocation and broadcasts it to every process in the fleet                                         |
| `RevocationExecer`                         | interface | What `PublishRevocation` needs; satisfied by the pool, a conn, or a tx                                                          |
| `Hints.Notifier()`, `Waker()`, `Revoker()` | method    | The three provider interfaces the hint transport satisfies. Hand them to `EnablePush`, the lease manager, and auth respectively |
| `Store.Close()`, `Hints.Close()`           | method    | Shut down the pool and the listener connection                                                                                  |

Beyond the base `backend.Backend` contract, `Store` also implements
`backend.CompareAndSwapper` and `backend.MultiGetter` - the optional interfaces
the runtime uses when a backend can do better than get-and-put. The sqlite
driver implements neither, so a fleet gets both optimizations and a single node
does not.

```go
store, err := postgres.Open(ctx, "postgres://user:pass@host:5432/naust")
defer store.Close()

hints, err := postgres.OpenHints(ctx, store)   // optional: cross-instance
defer hints.Close()

db := objectdb.New(store, lease.NewStoreLease(store, lease.StoreLeaseConfig{}))
srv.EnablePush(db, hints.Notifier(), subs, sender)
```

## Concepts

**Concurrency model.** Batches need atomicity but not isolation from readers,
because the runtime serializes writes per account through leases. Unlike SQLite
there is no dedicated write connection: Postgres MVCC handles concurrent writers,
and the account lease already means at most one writer per account is active.

**Change and lease hints are an accelerator, never a correctness dependency.**
A dropped or duplicated hint costs latency and nothing else. Change delivery is
reconciled by clients on state strings (RFC 8620 section 7), and lease safety
is the store's generation fence, not a hint. Without hints, a fleet still
converges on the periodic scan.

**Revocations are held to a stronger contract: at-least-once.** Each
`PublishRevocation` upserts a durable `revocations` row in the publisher's
transaction and fires a NOTIFY; the NOTIFY is only the fast path. Every
process also re-reads the table's retention window (24h) on a slow poll, so a
NOTIFY lost while a listener was reconnecting is re-delivered by the next
poll, about a minute later at worst. Events carry the revocation time and the
runtime applies them idempotently - redelivery closes nothing that
authenticated after the revocation. The residual risks are narrow: the
database being unreachable for longer than the retention window while revoked
connections stay open, or clock skew between the database and a host beyond
the runtime's slack.

**Hint payloads are untrusted input.** Any database role that can NOTIFY on
these channels can forge one, so the listener decodes strictly into a typed
struct and drops anything malformed. The worst a forgery achieves is a spurious
wakeup or resync.

**One dedicated listener connection per process** carries every hint. LISTEN is
session state, so that connection is dialed from the pool's config rather than
borrowed from the pool, and it never runs a transaction - a LISTENer inside a
long transaction stops Postgres reclaiming its notification queue.

**Conformance.** This driver passes `backendtest.Run`, and `Hints` passes
`notifytest.Run` and `notifytest.RunLinked`.

## Examples

- [`examples/mailserver -postgres <dsn>`](../../examples/mailserver) - the same
  server on Postgres instead of SQLite.
- [`examples/cluster`](../../examples/cluster) - two independent stacks sharing
  one database, proving writes serialize and hints cross the process boundary.
  Requires `PG_TEST_DSN`.

## Status and compatibility

Pre-release, tagged v0.1.1. Requires `core` v0.3.0 or later.

Tests in this module read `PG_TEST_DSN` and skip when it is unset.

## Related modules

| Module                        | Relationship                                                     |
|-------------------------------|------------------------------------------------------------------|
| [`core`](../../core)          | Defines `backend.Backend`, `notify.Notifier` and `lease.Manager` |
| [`drivers/sqlite`](../sqlite) | The single-process alternative                                   |
| [`drivers/`](..)              | How to write a backend of your own                               |

For running a fleet, see [naust.email/naust-jmap/fleet](https://naust.email/naust-jmap/fleet).
