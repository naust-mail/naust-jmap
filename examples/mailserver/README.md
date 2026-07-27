# mailserver

A complete, persistent JMAP mail server in one process.

## Purpose

The integration counterpart to [`quickstart`](../quickstart): where that shows
one derived datatype, this shows the whole mail plugin wired to a real backend
and a real delivery path - the RFC 8621 types over the SQLite driver, with mail
actually arriving through two adapters and live push on the event-source stream.

It is also the worked demonstration of the project's placement rule: the runtime
owns protocol correctness, the mail package owns what the objects mean, the
driver owns persistence, and none of the three knows about the others beyond the
interfaces.

## Concepts demonstrated

- The RFC 8621 types registered over a real driver ([`drivers/sqlite`](../../drivers/sqlite), or Postgres)
- Delivery below the protocol: LMTP (behind an MTA) and a plain HTTP ingest endpoint
- Bearer-token authentication - the argon2id password check runs once at login, never per request
- Sending: the submission queue, the worker, and an SMTP relay or loopback mode
- Push over the event-source stream, and over WebSocket in `websocket_test.go`

## Running

```sh
go run ./examples/mailserver
```

Writes `./naust-mail.db`, serves JMAP on `localhost:8080`, accepts LMTP on
`127.0.0.1:2400`, and takes an HTTP ingest `POST` on `/ingest`. The file header
walks through creating an Inbox, delivering a message both ways, and reading it
back over JMAP.

On Postgres instead, where several instances may share one database as a fleet:

```sh
go run ./examples/mailserver -postgres 'postgres://user:pass@localhost:5432/naust'
```

Flags: `-db`, `-postgres` (replaces `-db` entirely), `-http`, `-lmtp`, `-relay`,
`-relay-user`, `-relay-pass`, `-relay-tls`. With no `-relay` it runs loopback
mode, "sending" by delivering to local accounts; `-relay host:port` relays
outbound through a real smarthost.

## Next steps

- [`datatypes/mail`](../../datatypes/mail) - the public API, and the calls this
  module makes where RFC 8621 is silent
- [`cluster`](../cluster) - what changes when two of these share one database
- [naust.email/naust-jmap/mail](https://naust.email/naust-jmap/mail) - the task-by-task version
