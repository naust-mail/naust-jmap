// Package mail is the RFC 8621 datatype plugin for the naust-jmap
// runtime: Mailbox, Thread and Email registered as descriptor types
// with method extensions over the derived RFC 8620 machinery. The
// runtime owns protocol correctness; this package owns what the mail
// objects mean.
package mail

// CapabilityURI is the RFC 8621 mail capability. Its session-level
// value is an empty JSON object; its account-level value is an
// AccountCapability.
const CapabilityURI = "urn:ietf:params:jmap:mail"

// Limits this implementation advertises and enforces.
const (
	// maxSizeMailboxName is the Mailbox name limit in UTF-8 octets
	// (RFC 8621 section 1.3.1 requires at least 100).
	maxSizeMailboxName = 490
	// maxMailboxDepth bounds the Mailbox tree: a mailbox plus its
	// ancestors never exceed this many levels.
	maxMailboxDepth = 10
)

// AccountCapability is the mail capability object inside an account's
// accountCapabilities (RFC 8621 section 1.3.1).
type AccountCapability struct {
	// MaxMailboxesPerEmail is the most mailboxes one Email may belong
	// to; nil means no limit.
	MaxMailboxesPerEmail *int64 `json:"maxMailboxesPerEmail"`
	// MaxMailboxDepth is the deepest allowed Mailbox tree; nil means no
	// limit.
	MaxMailboxDepth *int64 `json:"maxMailboxDepth"`
	// MaxSizeMailboxName is the Mailbox name limit in UTF-8 octets.
	MaxSizeMailboxName int64 `json:"maxSizeMailboxName"`
	// MaxSizeAttachmentsPerEmail is the limit, in octets, on the summed
	// size of attachments when creating an Email.
	MaxSizeAttachmentsPerEmail int64 `json:"maxSizeAttachmentsPerEmail"`
	// EmailQuerySortOptions lists the Email/query sort properties the
	// server supports.
	EmailQuerySortOptions []string `json:"emailQuerySortOptions"`
	// MayCreateTopLevelMailbox reports whether the user may create a
	// Mailbox with a null parentId.
	MayCreateTopLevelMailbox bool `json:"mayCreateTopLevelMailbox"`
}

// DefaultAccountCapability returns the capability object matching what
// this package enforces. EmailQuerySortOptions stays empty until
// Email/query lands.
func DefaultAccountCapability() AccountCapability {
	depth := int64(maxMailboxDepth)
	return AccountCapability{
		MaxMailboxesPerEmail:       nil,
		MaxMailboxDepth:            &depth,
		MaxSizeMailboxName:         maxSizeMailboxName,
		MaxSizeAttachmentsPerEmail: 50_000_000,
		EmailQuerySortOptions:      []string{},
		MayCreateTopLevelMailbox:   true,
	}
}
