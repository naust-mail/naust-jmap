# cluster

Two independent mail stacks sharing one Postgres database, proven to behave as
one coherent service.

## Purpose

This is a test suite, not a runnable server - the only example that is. It
exists because the fleet guarantees cannot be demonstrated in a single object
graph: they are about what two processes do to each other through the database.

Each "instance" is a full object graph of its own - its own connection pool, its
own hint transport, its own store lease, its own `Server` and submission worker -
never a shared Go object. Two such graphs in one test binary contend inside
Postgres exactly as two operating-system processes would, since advisory locks
and LISTEN are per-connection rather than per-process, with the advantage that
the race detector sees both sides.

## Concepts demonstrated

- Writes to the same account serialize across instances
- A stale writer's commit fails its fence cleanly instead of corrupting
- Change notifications and submission wakes cross the process boundary over the
  LISTEN/NOTIFY hint transport
- The timer-driven sweep still drains work when that transport is absent

## Running

```sh
PG_TEST_DSN='postgres://user:pass@localhost:5432/db' go test ./examples/cluster
```

Requires a real shared Postgres server. **Without `PG_TEST_DSN` the tests skip
rather than fail** - none of these guarantees can be exercised in process. Each
test namespaces its accounts with a per-run unique suffix, so repeated runs
against the same database never collide.

## Next steps

- [`drivers/postgres`](../../drivers/postgres) - the hint transport and what it
  does and does not guarantee
- [`mailserver`](../mailserver) - the single-instance version, which also takes
  `-postgres`
- [naust.email/naust-jmap/fleet](https://naust.email/naust-jmap/fleet) - running a fleet
