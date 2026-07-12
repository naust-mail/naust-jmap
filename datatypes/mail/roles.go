package mail

// mailboxRoles is the set of valid Mailbox role values: the IANA "IMAP
// Mailbox Name Attributes" registry names converted to lowercase, the
// registry RFC 8621 section 2 designates for roles.
var mailboxRoles = map[string]bool{
	"all":           true,
	"archive":       true,
	"drafts":        true,
	"flagged":       true,
	"haschildren":   true,
	"hasnochildren": true,
	"important":     true,
	"inbox":         true,
	"junk":          true,
	"marked":        true,
	"noinferiors":   true,
	"nonexistent":   true,
	"noselect":      true,
	"remote":        true,
	"sent":          true,
	"subscribed":    true,
	"trash":         true,
	"unmarked":      true,
}
