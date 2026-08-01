package submit

// CapabilityURI is the RFC 8621 submission capability, which Identity and
// EmailSubmission live under (section 1.3.2).
const CapabilityURI = "urn:ietf:params:jmap:submission"

// Limits are the enforced EmailSubmission/set limits. Only MaxDelayedSend
// is advertised in the capability object (the spec defines no fields for
// the others); MaxMessageBytes surfaces as the tooLarge SetError's maxSize
// and MaxRecipients as tooManyRecipients' maxRecipients (RFC 8621 section
// 7.5). Values are used verbatim - start from DefaultLimits and override.
type Limits struct {
	// MaxRecipients caps the envelope rcptTo list.
	MaxRecipients uint64
	// MaxMessageBytes caps the size of a message that may be sent, in
	// octets.
	MaxMessageBytes uint64
	// MaxDelayedSend is the longest FUTURERELEASE hold accepted, in
	// seconds (RFC 4865 via RFC 8621 section 7); 0 disables delayed send.
	MaxDelayedSend int64
}

// DefaultLimits returns this package's default sending limits.
// MaxMessageBytes leaves headroom over DefaultAccountCapability's
// attachment cap after base64 expansion, so anything composable is
// sendable.
func DefaultLimits() Limits {
	return Limits{
		MaxRecipients:   100,
		MaxMessageBytes: 75_000_000,
		MaxDelayedSend:  7 * 24 * 3600,
	}
}

// AccountCapability is the submission capability object inside an
// account's accountCapabilities (RFC 8621 section 1.3.2).
type AccountCapability struct {
	// MaxDelayedSend is the maximum sending delay in seconds; 0 means
	// delayed send is not supported.
	MaxDelayedSend int64 `json:"maxDelayedSend"`
	// SubmissionExtensions maps each supported submission extension's
	// ehlo-name to its ehlo-args.
	SubmissionExtensions map[string][]string `json:"submissionExtensions"`
}

// AccountCapabilityFor derives the advertised capability object from the
// enforced limits. FUTURERELEASE is listed because this package
// implements it natively (the hold happens in the submission queue, not
// the smarthost); its RFC 4865 ehlo-args (max interval, max date-time)
// describe a live SMTP session and have no static value here, so the
// args list is empty - JMAP clients read maxDelayedSend instead.
func AccountCapabilityFor(limits Limits) AccountCapability {
	exts := map[string][]string{}
	if limits.MaxDelayedSend > 0 {
		exts["FUTURERELEASE"] = []string{}
	}
	return AccountCapability{
		MaxDelayedSend:       limits.MaxDelayedSend,
		SubmissionExtensions: exts,
	}
}
