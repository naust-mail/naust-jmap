module github.com/naust-mail/naust-jmap/datatypes/mail

go 1.24.0

require (
	github.com/naust-mail/naust-jmap/core v0.0.0-00010101000000-000000000000
	golang.org/x/text v0.37.0
)

// The core module is unpublished pre-release; drop this once it has
// tagged versions.
replace github.com/naust-mail/naust-jmap/core => ../../core
