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

// SubmissionCapabilityURI is the RFC 8621 submission capability, which
// Identity and EmailSubmission live under (section 1.3.2).
const SubmissionCapabilityURI = "urn:ietf:params:jmap:submission"

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
// this package enforces. EmailQuerySortOptions advertises exactly the
// Email/query sort properties emailSort implements (RFC 8621 section 4.4.2).
func DefaultAccountCapability() AccountCapability {
	depth := int64(maxMailboxDepth)
	return AccountCapability{
		MaxMailboxesPerEmail:       nil,
		MaxMailboxDepth:            &depth,
		MaxSizeMailboxName:         maxSizeMailboxName,
		MaxSizeAttachmentsPerEmail: 50_000_000,
		EmailQuerySortOptions:      append([]string(nil), emailSortProps...),
		MayCreateTopLevelMailbox:   true,
	}
}

// SubmissionLimits are the enforced EmailSubmission/set limits. Only
// MaxDelayedSend is advertised in the capability object (the spec defines
// no fields for the others); MaxMessageBytes surfaces as the tooLarge
// SetError's maxSize and MaxRecipients as tooManyRecipients' maxRecipients
// (RFC 8621 section 7.5). Values are used verbatim - start from
// DefaultSubmissionLimits and override.
type SubmissionLimits struct {
	// MaxRecipients caps the envelope rcptTo list.
	MaxRecipients uint64
	// MaxMessageBytes caps the size of a message that may be sent, in
	// octets.
	MaxMessageBytes uint64
	// MaxDelayedSend is the longest FUTURERELEASE hold accepted, in
	// seconds (RFC 4865 via RFC 8621 section 7); 0 disables delayed send.
	MaxDelayedSend int64
}

// DefaultSubmissionLimits returns this package's default sending limits.
// MaxMessageBytes leaves headroom over DefaultAccountCapability's
// attachment cap after base64 expansion, so anything composable is
// sendable.
func DefaultSubmissionLimits() SubmissionLimits {
	return SubmissionLimits{
		MaxRecipients:   100,
		MaxMessageBytes: 75_000_000,
		MaxDelayedSend:  7 * 24 * 3600,
	}
}

// SubmissionAccountCapability is the submission capability object inside
// an account's accountCapabilities (RFC 8621 section 1.3.2).
type SubmissionAccountCapability struct {
	// MaxDelayedSend is the maximum sending delay in seconds; 0 means
	// delayed send is not supported.
	MaxDelayedSend int64 `json:"maxDelayedSend"`
	// SubmissionExtensions maps each supported submission extension's
	// ehlo-name to its ehlo-args.
	SubmissionExtensions map[string][]string `json:"submissionExtensions"`
}

// SubmissionAccountCapabilityFor derives the advertised capability object
// from the enforced limits. FUTURERELEASE is listed because this package
// implements it natively (the hold happens in the submission queue, not
// the smarthost); its RFC 4865 ehlo-args (max interval, max date-time)
// describe a live SMTP session and have no static value here, so the
// args list is empty - JMAP clients read maxDelayedSend instead.
func SubmissionAccountCapabilityFor(limits SubmissionLimits) SubmissionAccountCapability {
	exts := map[string][]string{}
	if limits.MaxDelayedSend > 0 {
		exts["FUTURERELEASE"] = []string{}
	}
	return SubmissionAccountCapability{
		MaxDelayedSend:       limits.MaxDelayedSend,
		SubmissionExtensions: exts,
	}
}
