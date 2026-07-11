module github.com/naust-mail/naust-jmap/examples

go 1.24

require github.com/naust-mail/naust-jmap/core v0.0.0

// The core module is unpublished pre-release; drop this once it has
// tagged versions.
replace github.com/naust-mail/naust-jmap/core => ../core
