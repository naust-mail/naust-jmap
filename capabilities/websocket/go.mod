module github.com/naust-mail/naust-jmap/capabilities/websocket

go 1.24

require github.com/naust-mail/naust-jmap/core v0.0.0

// The naust-jmap modules are unpublished pre-release; drop this replace
// once they have tagged versions.
replace github.com/naust-mail/naust-jmap/core => ../../core
