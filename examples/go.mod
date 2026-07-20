module github.com/naust-mail/naust-jmap/examples

go 1.25.0

require (
	github.com/naust-mail/naust-jmap/core v0.0.0
	github.com/naust-mail/naust-jmap/datatypes/mail v0.0.0
	github.com/naust-mail/naust-jmap/drivers/postgres v0.0.0
	github.com/naust-mail/naust-jmap/drivers/sqlite v0.0.0
	golang.org/x/crypto v0.33.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.53.0 // indirect
)

// The naust-jmap modules are unpublished pre-release; drop these replaces
// once they have tagged versions.
replace (
	github.com/naust-mail/naust-jmap/core => ../core
	github.com/naust-mail/naust-jmap/datatypes/mail => ../datatypes/mail
	github.com/naust-mail/naust-jmap/drivers/postgres => ../drivers/postgres
	github.com/naust-mail/naust-jmap/drivers/sqlite => ../drivers/sqlite
)
