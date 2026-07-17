module github.com/naust-mail/naust-jmap/examples

go 1.24

require (
	github.com/naust-mail/naust-jmap/core v0.0.0
	github.com/naust-mail/naust-jmap/datatypes/mail v0.0.0
	github.com/naust-mail/naust-jmap/drivers/sqlite v0.0.0
)

// The naust-jmap modules are unpublished pre-release; drop these replaces
// once they have tagged versions.
replace (
	github.com/naust-mail/naust-jmap/core => ../core
	github.com/naust-mail/naust-jmap/datatypes/mail => ../datatypes/mail
	github.com/naust-mail/naust-jmap/drivers/sqlite => ../drivers/sqlite
)
