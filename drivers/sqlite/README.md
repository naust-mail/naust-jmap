# sqlite driver

[![Go Reference](https://pkg.go.dev/badge/github.com/naust-mail/naust-jmap/drivers/sqlite.svg)](https://pkg.go.dev/github.com/naust-mail/naust-jmap/drivers/sqlite)

A `backend.Backend` implementation over a single SQLite database file. One
key-value table, no knowledge of JMAP.

```sh
go get github.com/naust-mail/naust-jmap/drivers/sqlite
```

## Overview

All JMAP structure - collections, indexes, the change log - is built above the
`backend.Backend` interface by `core/objectdb`, so nothing in this module knows
about the protocol. It stores keys and values in order, and that is the whole
contract.

It exists as its own Go module so the core runtime stays free of third-party
dependencies: embedders who want SQLite import this module, everyone else never
downloads it.

## When to use this module

Single-node servers, development, and tests that need real persistence. This is
the default choice for one process serving one database file.

Use [`drivers/postgres`](../postgres) instead when more than one process must
serve the same accounts. SQLite gives you no cluster story, by design.

Import cost: a SQLite driver dependency, quarantined to this module.

## Public API

| Symbol                              | Kind   | What it is                                                      |
|-------------------------------------|--------|-----------------------------------------------------------------|
| `Open(path string) (*Store, error)` | func   | Opens (and creates) the database file                           |
| `Store`                             | type   | The `backend.Backend` implementation. Pass it to `objectdb.New` |
| `Store.Close()`                     | method | Closes both connection pools                                    |

`Store`'s other methods (`Get`, `Scan`, `WriteBatch`) are the `backend.Backend`
contract; the runtime calls them, you do not. That is the entire public surface.

```go
store, err := sqlite.Open("./naust-mail.db")
defer store.Close()

db := objectdb.New(store, lease.NewInProcess(store))
```

## Concepts

**Concurrency model.** Batches need atomicity but not isolation from readers,
because the runtime already serializes writes per account through leases. Writes
go through a single connection with immediate transactions; reads run on a
separate pool, which WAL mode lets proceed without blocking the writer.

**Conformance.** This driver passes `core/providers/backend/backendtest.Run`,
the shared contract suite every `Backend` must pass. If you are writing your own
driver, that suite is the specification.

## Examples

[`examples/mailserver`](../../examples/mailserver) runs on this driver by
default, writing `./naust-mail.db`.

## Status and compatibility

Pre-release, tagged v0.1.1. Requires `core` v0.3.0 or later.

## Related modules

| Module                            | Relationship                                            |
|-----------------------------------|---------------------------------------------------------|
| [`core`](../../core)              | Defines the `backend.Backend` interface this implements |
| [`drivers/postgres`](../postgres) | The alternative, for more than one process              |
| [`drivers/`](..)                  | How to write a backend of your own                      |
