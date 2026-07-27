# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities privately through GitHub's private
vulnerability reporting: go to the
[Security tab](https://github.com/naust-mail/naust-jmap/security) of this
repository and choose **Report a vulnerability**. This keeps the report
confidential until a fix is available.

Do not open a public issue or pull request for a suspected vulnerability.

A useful report includes:

- the affected package and version or commit,
- a description of the impact (what an attacker can do),
- the smallest input or steps that reproduce it, and
- any conditions required (authenticated vs. unauthenticated, configuration).

We aim to acknowledge a report within a few business days and to coordinate a
fix and disclosure timeline with you. Credit is given to reporters who want it.

## Supported versions

naust-jmap is pre-1.0. Security fixes land on the default branch and the latest
tag; there is no backport line until a stable release exists. Pin a commit or
tag and update to pick up fixes.

## Known dependency findings

`govulncheck` reports GO-2026-5970 against `datatypes/mail`: a loop in
`golang.org/x/text/unicode/norm`, reached only through `norm.Iter`. This module
calls `NFC.String` and `NFC.IsNormalString`, neither of which constructs an
`Iter`, and every `x/text` release carrying the fix requires Go 1.25 while this
module supports Go 1.24. Both halves of that are enforced by
`TestNormSurfaceIsStringAndIsNormalOnly` and `TestNormLoopStaysBehindIter` in
`datatypes/mail/internal/depsguard`, which fail if either stops holding. On
Go 1.25 or later, require `x/text` v0.39.0 in your own `go.mod` to clear it.

## Scope

naust-jmap is a library that executes the JMAP protocol. It owns protocol
correctness and input validation. The host application owns everything the
runtime delegates: TLS termination, authentication, authorization, the storage
and blob backends, search, and delivery. Vulnerabilities in a host's own
implementation of those are out of scope here, though we are happy to be told
about a runtime interface that makes them easy to get wrong.

The defense-in-depth limits the runtime enforces - request and parsing caps,
message-delivery bounds, blob resource limits, and structural guards - are
catalogued in [HARDENING.md](HARDENING.md). That is engineering reference, not
policy, so it lives outside this file.
