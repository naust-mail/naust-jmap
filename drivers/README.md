# drivers

Provider implementations that carry third-party dependencies. Each is its own Go
module, so its dependencies enter your build only if you import it.

This directory is not itself a module; there is nothing here to `go get`.

| Driver                 | Implements                                                                              | Use it for                             |
|------------------------|-----------------------------------------------------------------------------------------|----------------------------------------|
| [`sqlite`](sqlite)     | `backend.Backend`                                                                       | A single process, one database file    |
| [`postgres`](postgres) | `backend.Backend`, plus `notify.Notifier`, `lease.Waker` and `auth.Revoker` via `Hints` | Several processes sharing one database |

If neither fits, write your own. The rest of this page is how.

## Contents

- [Should you write one](#should-you-write-one)
- [The contract](#the-contract)
- [What you do not have to do](#what-you-do-not-have-to-do)
- [The conformance suite is the specification](#the-conformance-suite-is-the-specification)
- [Optional interfaces](#optional-interfaces)
- [Packaging](#packaging)
- [Checklist](#checklist)
- [Related](#related)

## Should you write one

Write a `backend.Backend` when you need naust-jmap's data in a store the two
shipped drivers do not cover - an embedded engine you already operate, a hosted
key-value service, a storage layer your organization mandates.

Do not write one to change *how JMAP behaves*. Backends never see protocol
concepts: collections, indexes, the change log and state strings are all built
above the interface by `core/objectdb`. A backend that knows what a Mailbox is
has been given the wrong job. If the behaviour you want is protocol-shaped, the
seam you need is a datatype plugin or a capability module, not a driver.

A backend is a small piece of work. The interface is four methods over an
ordered key-value store - carrying six operations, since writes arrive as a
batch - and the reference driver ([`sqlite`](sqlite)) is about 300 lines.

## The contract

Implement [`backend.Backend`](https://pkg.go.dev/github.com/naust-mail/naust-jmap/core/providers/backend#Backend):

| Method                               | Obligation                                                                                                                                                                        |
|--------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Get(ctx, key)`                      | Return the value or `ErrNotFound`. **The returned slice belongs to the caller** - do not retain, reuse or modify it, because readers hold sub-slices of it inside decoded records |
| `Scan(ctx, start, end, reverse, fn)` | Visit `[start, end)` in byte order, descending when `reverse`; stop early when `fn` returns false. Key and value slices are valid **only during the call**                        |
| `WriteBatch(ctx, b)`                 | Apply every op atomically or none. A failing `Assert` aborts the whole batch with `ErrAssertFailed`                                                                               |
| `Close()`                            | Release resources; unusable afterwards                                                                                                                                            |

Keys are ordered by `bytes.Compare` and values are opaque. Your store must
preserve that ordering exactly - the runtime's index scans depend on it.

A batch carries four op kinds: `Set`, `Delete`, `Add` (an atomic counter delta,
stored in the 8-byte `EncodeInt64` form), and `Assert` (the value at a key must
match for the batch to apply, where nil means the key must be absent).

**`Assert` is the one to get right.** It is the runtime's lease fencing: a
stalled writer's late commit fails its assertion instead of corrupting the
account. A backend that applies a batch whose `Assert` did not match is not
merely slow or lossy, it is unsafe, and no layer above can compensate.

## What you do not have to do

More of the design is in what the contract omits. The runtime serializes all
writes to an account through a per-account lease and fences every batch, so:

- **No interactive transactions.** Atomic batches are enough.
- **No isolation between concurrent batches** to different accounts.
- **No isolation from readers.** A reader may observe a batch mid-flight only if
  your engine allows it; the lease means no second writer is racing the account.
- **No protocol awareness.** No collections, no indexes, no change log.

This is why an engine can be wired up in an afternoon. If you find yourself
reaching for two-phase commit or snapshot isolation, re-read this section - you
are almost certainly solving a problem the lease already solved.

## The conformance suite is the specification

`backendtest.Run` is the shared contract suite. Every shipped backend passes the
identical tests, and a backend that passes is behaviorally interchangeable with
every other for the guarantees it exercises.

Write this test before writing the engine:

```go
func TestContract(t *testing.T) {
    backendtest.Run(t, backendtest.Config{
        Open: func(t *testing.T) backend.Backend {
            s, err := mydriver.Open(filepath.Join(t.TempDir(), "test.db"))
            if err != nil {
                t.Fatal(err)
            }
            return s
        },
        // Reopen proves durability across a close. Leave it nil for an
        // in-memory backend and the persistence tests are skipped.
        Reopen: func(t *testing.T, b backend.Backend) backend.Backend {
            // close b, reopen the same underlying storage
        },
    })
}
```

The method set is only the shape of the contract. The suite is the contract.
Treat a failure as a bug in your driver, not in the suite.

Implementing `notify.Notifier` or `lease.Manager` as well? They have suites too:
`notifytest.Run`, `notifytest.RunLinked` (for a notifier that links separate
processes), and `leasetest.Run`.

## Optional interfaces

Implement these when your engine can beat the base contract. The runtime detects
them and uses them; skip them and nothing breaks.

| Interface                   | What it buys                                             | Shipped in |
|-----------------------------|----------------------------------------------------------|------------|
| `backend.MultiGetter`       | One round trip for a batch of keys instead of N          | postgres   |
| `backend.CompareAndSwapper` | Native CAS for lease claims rather than read-then-assert | postgres   |
| `blob.BatchDeleter`         | Bulk reclamation during blob sweeps                      | -          |

## Packaging

**A dependency means a module.** If your driver imports anything outside the
standard library, give it its own `go.mod`. That is why `sqlite` and `postgres`
are separate modules rather than packages: their dependencies are quarantined
and enter a consumer's build only on import.

Build tags are not a substitute. Go resolves dependencies per module, not per
build configuration, so a tagged-off driver still drags its dependencies into
`go.sum`.

Your driver does not need to live in this repository. It is an ordinary Go
module that imports `core` and satisfies an interface.

## Checklist

1. `backendtest.Run` passes, including `Reopen` if your store persists.
2. `Assert` failures abort the entire batch and return `ErrAssertFailed`.
3. `Get` returns caller-owned slices; `Scan` slices are not retained past `fn`.
4. Ordering matches `bytes.Compare` for the full key space, including keys with
   embedded zero bytes.
5. `Close` is safe to call once and leaves the store unusable, not panicking.
6. Own `go.mod` if you carry any dependency.
7. `go test -race` passes: the runtime calls a backend from many goroutines.

## Related

- [`core`](../core#extension-points) - the provider interfaces and where each fits
- [`core/providers/backend`](https://pkg.go.dev/github.com/naust-mail/naust-jmap/core/providers/backend) - the interface reference
- [`sqlite`](sqlite) - the smallest complete implementation, worth reading first
- [`postgres`](postgres) - the same contract plus the optional interfaces and a
  cluster hint transport
