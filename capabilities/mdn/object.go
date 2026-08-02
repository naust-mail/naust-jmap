package mdn

import "github.com/naust-mail/naust-jmap/core/jmap"

// MDN is the RFC 9007 section 2 MDN object: the JMAP representation of
// a message disposition notification, used both as the creation object
// of MDN/send and as the parsed representation MDN/parse returns.
// Nullable properties are pointers so a null on the wire round-trips as
// nil rather than as a zero value.
type MDN struct {
	// ForEmailID is the Email id of the received message this MDN is
	// about. It MUST NOT be null in MDN/send creations; MDN/parse leaves
	// it null when the Original-Message-ID correlation finds nothing or
	// is ambiguous (section 2.2).
	ForEmailID *jmap.Id `json:"forEmailId"`
	// Subject is the MDN message's Subject header field value.
	Subject *string `json:"subject"`
	// TextBody is the human-readable first part of the multipart/report,
	// as plain text.
	TextBody *string `json:"textBody"`
	// IncludeOriginalMessage asks for the original message as the third
	// component of the multipart/report (default false; see RFC 8098
	// for the security considerations of returning full content).
	IncludeOriginalMessage bool `json:"includeOriginalMessage"`
	// ReportingUA names the MUA creating the MDN; null omits the
	// Reporting-UA field, which has better privacy properties.
	ReportingUA *string `json:"reportingUA"`
	// Disposition carries the action-mode, sending-mode, and
	// disposition-type of the notification.
	Disposition *Disposition `json:"disposition"`
	// MDNGateway is the RFC 8098 section 3.2.2 MDN-Gateway field: the
	// MTA that translated a foreign disposition notification into this
	// MDN. Server-set.
	MDNGateway *string `json:"mdnGateway"`
	// OriginalRecipient is the recipient address as specified by the
	// sender of the original message (RFC 8098 section 3.2.3).
	// Server-set.
	OriginalRecipient *string `json:"originalRecipient"`
	// FinalRecipient is the recipient the MDN is issued for (RFC 8098
	// section 3.2.4). In MDN/send the server derives it from the
	// Identity unless the client sets it explicitly.
	FinalRecipient *string `json:"finalRecipient"`
	// OriginalMessageID is the Message-ID header field (RFC 5322) of
	// the message the MDN is issued for - not a JMAP id. Server-set.
	OriginalMessageID *string `json:"originalMessageId"`
	// Error carries the text of RFC 8098 section 3.2.7 Error fields when
	// the error disposition modifier appears. Server-set.
	Error []string `json:"error"`
	// ExtensionFields maps RFC 8098 section 3.3 extension-field names to
	// their values.
	ExtensionFields map[string]string `json:"extensionFields"`
}

// Disposition is the RFC 9007 section 2 Disposition object. RFC 8098
// section 3.2.6 defines the value sets and treats them as case
// insensitive on the wire; in JMAP they are case sensitive and always
// lowercase, and MDN/parse lowercases what it reads.
type Disposition struct {
	// ActionMode MUST be "manual-action" or "automatic-action".
	ActionMode string `json:"actionMode"`
	// SendingMode MUST be "mdn-sent-manually" or "mdn-sent-automatically".
	SendingMode string `json:"sendingMode"`
	// Type MUST be "deleted", "dispatched", "displayed", or "processed".
	Type string `json:"type"`
}
