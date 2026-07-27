# quickstart

The smallest complete naust-jmap server, in one file.

## Purpose

Shows the whole idea on one screen: describe a datatype as data, and the runtime
derives its `/get`, `/changes`, `/set`, `/copy`, `/query` and `/queryChanges`
methods with full RFC 8620 semantics. No method code is written here.

The datatype is Todo, RFC 8620's own worked example (section 5.7), so the
requests can be read side by side with the spec.

Start here. [`mailserver`](../mailserver) is the same runtime with a real
datatype, a real backend and a real delivery path.

## Concepts demonstrated

- A `descriptor.Type` and the six methods derived from it
- The in-memory backend and in-process lease manager (`objectdb.New`), swappable
  for a driver module in two lines
- `auth.Authenticator` via a demo password list
- Binary data (`Server.EnableBlobs`, RFC 8620 section 6)
- Push (`Server.EnablePush`, section 7)

## Running

```sh
go run ./examples/quickstart
```

Listens on `localhost:8080`. Nothing is written to disk - state is in memory and
gone when the process exits. The demo user is `demo@example.com`, password
`demo`.

```sh
curl -su demo@example.com:demo http://localhost:8080/.well-known/jmap
```

The file header shows a full request that creates a record, queries for it and
fetches it back by reference in one round trip.

## Next steps

- [`core`](../../core) - the runtime's public API and provider interfaces
- [`mailserver`](../mailserver) - the same picture with persistence and mail
- [naust.email/naust-jmap/quickstart](https://naust.email/naust-jmap/quickstart) - the task-by-task version
