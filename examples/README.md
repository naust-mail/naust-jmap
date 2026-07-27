# examples

Runnable servers built on naust-jmap, in increasing order of completeness. Each
directory's `main.go` (or test file) carries a header comment that walks through
what it does; the READMEs here say which one you want and how to run it.

This is its own Go module, so building the examples never adds anything to a
consumer's dependency graph.

| Example                    | What it is                                                                                    | Needs                       |
|----------------------------|-----------------------------------------------------------------------------------------------|-----------------------------|
| [`quickstart`](quickstart) | The smallest complete server: one datatype described as data, in memory, over HTTP            | Nothing                     |
| [`mailserver`](mailserver) | A complete, persistent RFC 8621 mail server: five types, two delivery adapters, sending, push | Nothing (SQLite by default) |
| [`cluster`](cluster)       | Two independent stacks sharing one Postgres database, proving they behave as one service      | `PG_TEST_DSN`               |

`internal/` holds two authenticator patterns the examples share - a demo password
list and a bearer-token minter. They are illustrations of `auth.Authenticator`,
not library code, which is why they are internal.

## Related

- [`core`](../core) - the runtime every example imports
- [`datatypes/mail`](../datatypes/mail) - what `mailserver` and `cluster` serve

For task-oriented guidance, see
[naust.email/naust-jmap](https://naust.email/naust-jmap).
