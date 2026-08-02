package report

// Outbound generation of a message disposition notification: the write-twin
// of the MDN parser in report.go. A generated MDN is an RFC 5322 message
// whose body is a multipart/report container with report-type
// "disposition-notification" (RFC 8098 section 3, RFC 6522 section 3):
// part one a human-readable explanation, part two the machine-readable
// message/disposition-notification content (RFC 8098 section 3.1), and an
// optional part three returning the original message or just its header
// block (RFC 6522 sections 3 and 4).
//
// Header ownership: the caller supplies the identities and the report
// content; Write owns the wire framing. Date and Message-ID are generated
// here (RFC 8098 section 3 requires only that a generated Message-ID
// differ from the original message's, which a fresh random token
// guarantees), MIME-Version and the multipart/report Content-Type are
// structural, and Auto-Submitted: auto-replied is mandatory on every
// generated MDN (RFC 3834 section 5). No Disposition-Notification-To is
// ever written: an MDN MUST NOT itself request an MDN (section 3).
//
// The envelope is not this package's concern; RFC 8098 section 3 requires
// the MDN be sent with a null return path, which the submitting caller
// sets on the SMTP transaction.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// Address is one mailbox of a generated MDN. Name is the display-name,
// "" for a bare addr-spec; non-ASCII names are carried as RFC 2047
// encoded-words.
type Address struct {
	Name  string
	Email string
}

// Disposition is the three grammar values of the Disposition field (RFC
// 8098 section 3.2.6), lowercased exactly as the parse side and RFC 9007
// section 2 produce them. Write emits them as the single field value
// "action-mode/sending-mode; type"; the grammar's optional disposition
// modifiers are not generated.
type Disposition struct {
	// ActionMode is "manual-action" or "automatic-action": whether the
	// disposition followed an explicit user instruction (section 3.2.6.1).
	ActionMode string
	// SendingMode is "mdn-sent-manually" or "mdn-sent-automatically":
	// whether the user permitted this particular MDN (section 3.2.6.1).
	SendingMode string
	// Type is "displayed", "deleted", "dispatched" or "processed": what
	// became of the message (section 3.2.6.2).
	Type string
}

// ExtensionField is one MDN extension field of the notification content
// (RFC 8098 section 3.3). The list is ordered rather than a map because
// the fields are written in the order given: a Go map has no stable
// iteration order, which would make generated output nondeterministic,
// and a map could not carry a repeated field name.
type ExtensionField struct {
	Name  string
	Value string
}

// Message is everything one MDN needs. Its vocabulary is the JMAP MDN
// object's (RFC 9007 section 2), so the eventual MDN/send caller maps its
// properties across one for one.
type Message struct {
	// From is the identity the MDN is issued for: the address of the
	// person whose disposition is being reported (RFC 8098 section 3).
	From Address
	// To is the address the MDN is addressed to, which the caller takes
	// from the original message's Disposition-Notification-To header field
	// (RFC 8098 sections 2.1 and 3).
	To Address
	// Subject is the MDN's Subject header field (RFC 9007 section 2).
	Subject string
	// TextBody is the human-readable explanation carried as the first
	// component of the multipart/report (RFC 6522 section 3).
	TextBody string
	// ReportingUA names the MUA that performed the disposition, as
	// "ua-name" or "ua-name; ua-product" (RFC 8098 section 3.2.1). Empty
	// omits the field, which section 3.2.1 permits and which has the
	// better privacy properties.
	ReportingUA string
	// FinalRecipient is the recipient the MDN is issued for, as a bare
	// generic-address (RFC 8098 section 3.2.4). Required. Write supplies
	// the "rfc822" address-type; a value carrying its own type prefix is
	// rejected.
	FinalRecipient string
	// OriginalMessageID is the Message-ID of the message being reported
	// on, without angle brackets (RFC 8098 section 3.2.5). Empty omits the
	// field, which section 3.2.5 requires when the original message
	// carried no Message-ID. It is the only key an MDN can be correlated
	// by, so a caller that has one should always set it.
	OriginalMessageID string
	// OriginalRecipient is the recipient address as the sender of the
	// original message specified it, as a bare generic-address (RFC 8098
	// section 3.2.3). Empty omits the field, which section 3.2.3 requires
	// when no reliable original-recipient information exists.
	OriginalRecipient string
	// Disposition is the required Disposition field (RFC 8098 section
	// 3.2.6).
	Disposition Disposition
	// ExtensionFields are additional notification fields written after the
	// standard ones (RFC 8098 section 3.3).
	ExtensionFields []ExtensionField
	// Original is the message the MDN reports on. When non-nil it is
	// returned as the third component of the multipart/report (RFC 8098
	// section 3, RFC 6522 section 3); when nil the report has two parts.
	// It is read at most once, while Write streams.
	Original io.Reader
	// HeadersOnly selects the form of that third component: false returns
	// the whole original as message/rfc822, true returns only its leading
	// header block as text/rfc822-headers (RFC 6522 section 4), which is
	// what a caller returns when the original must not be resent in full.
	HeadersOnly bool
}

// Write streams the complete MDN message to w. The whole Message is
// validated first, so a Message that cannot be represented on the wire
// produces an error and no output at all - a header value able to smuggle
// a line break is a caller bug, never something this package quietly
// rewrites.
func Write(ctx context.Context, w io.Writer, m Message) error {
	if err := m.validate(); err != nil {
		return err
	}
	headers, err := m.topHeaders(time.Now())
	if err != nil {
		return err
	}
	return message.WriteMessage(ctx, w, headers, m.report())
}

// topHeaders builds the MDN's own header block (RFC 5322 section 3.6),
// ordered as a generated message conventionally carries them.
func (m Message) topHeaders(now time.Time) ([]message.HeaderField, error) {
	from, ok := message.FormatAddresses([]message.Address{addressOf(m.From)})
	if !ok {
		return nil, errf("From address %q cannot be represented in a header", m.From.Email)
	}
	to, ok := message.FormatAddresses([]message.Address{addressOf(m.To)})
	if !ok {
		return nil, errf("To address %q cannot be represented in a header", m.To.Email)
	}
	return []message.HeaderField{
		{Name: "Date", Value: message.FormatDate(now)},
		{Name: "From", Value: message.FoldValue("From", from)},
		{Name: "To", Value: message.FoldValue("To", to)},
		{Name: "Subject", Value: message.FoldValue("Subject", message.EncodeText(m.Subject))},
		{Name: "Message-ID", Value: "<" + randomToken() + "@" + domainOf(m.From.Email) + ">"},
		{Name: "MIME-Version", Value: "1.0"},
		// RFC 3834 section 5: an automatic response carries exactly one
		// Auto-Submitted field, and an MDN is not a human-authored reply.
		{Name: "Auto-Submitted", Value: "auto-replied"},
	}, nil
}

// report builds the multipart/report body: the human-readable part, the
// notification content, and the returned original when there is one (RFC
// 6522 section 3, RFC 8098 section 3).
func (m Message) report() *message.OutPart {
	boundary := message.NewBoundary()
	root := &message.OutPart{
		Headers: []message.HeaderField{{
			Name:  "Content-Type",
			Value: "multipart/report; report-type=disposition-notification;\r\n\tboundary=\"" + boundary + "\"",
		}},
		Boundary: boundary,
		SubParts: []*message.OutPart{m.textPart(), m.notificationPart()},
	}
	if p := m.originalPart(); p != nil {
		root.SubParts = append(root.SubParts, p)
	}
	return root
}

// textPart is the required human-readable first component (RFC 6522
// section 3). It is emitted as UTF-8 under quoted-printable: the text
// comes from a JMAP client and may hold any Unicode, and quoted-printable
// keeps a mostly-ASCII explanation legible to a reader with no MIME
// support while still being safe on a 7-bit path (RFC 2045 section 6.7).
func (m Message) textPart() *message.OutPart {
	return &message.OutPart{
		Headers: []message.HeaderField{
			{Name: "Content-Type", Value: "text/plain; charset=utf-8"},
			{Name: "Content-Transfer-Encoding", Value: message.EncQP},
		},
		Encoding: message.EncQP,
		Content:  readerOf(strings.NewReader(m.TextBody)),
	}
}

// notificationContent assembles the message/disposition-notification
// content, its field order following the grammar of RFC 8098 section 3.1.
// Validation measures this same string against the parse-side capture
// bound, so what it produces is by construction content the parser in
// this package reads back whole.
func (m Message) notificationContent() string {
	var b strings.Builder
	field := func(name, value string) {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteString("\r\n")
	}
	if m.ReportingUA != "" {
		field("Reporting-UA", m.ReportingUA)
	}
	if m.OriginalRecipient != "" {
		field("Original-Recipient", "rfc822; "+m.OriginalRecipient)
	}
	field("Final-Recipient", "rfc822; "+m.FinalRecipient)
	if m.OriginalMessageID != "" {
		field("Original-Message-ID", "<"+m.OriginalMessageID+">")
	}
	field("Disposition", m.Disposition.ActionMode+"/"+m.Disposition.SendingMode+"; "+m.Disposition.Type)
	for _, ext := range m.ExtensionFields {
		field(ext.Name, ext.Value)
	}
	return b.String()
}

// notificationPart is the machine-readable second component. Its encoding
// is 7bit, which RFC 8098 section 3.1 requires (validation has already
// established the content is ASCII).
func (m Message) notificationPart() *message.OutPart {
	return &message.OutPart{
		Headers: []message.HeaderField{
			{Name: "Content-Type", Value: "message/disposition-notification"},
			{Name: "Content-Transfer-Encoding", Value: message.Enc7Bit},
		},
		Encoding: message.Enc7Bit,
		Content:  readerOf(strings.NewReader(m.notificationContent())),
	}
}

// originalPart is the optional third component returning the original
// message (RFC 8098 section 3, RFC 6522 section 3), either whole as
// message/rfc822 or truncated to its header block as text/rfc822-headers
// (RFC 6522 section 4). Both forms declare 8bit: the returned content is
// copied through verbatim, and message/rfc822 admits no other encoding
// than the identity ones (RFC 2045 section 6.4).
func (m Message) originalPart() *message.OutPart {
	if m.Original == nil {
		return nil
	}
	ctype, src := "message/rfc822", m.Original
	if m.HeadersOnly {
		ctype, src = "text/rfc822-headers", &headerBlockReader{src: m.Original, bol: true}
	}
	return &message.OutPart{
		Headers: []message.HeaderField{
			{Name: "Content-Type", Value: ctype},
			{Name: "Content-Transfer-Encoding", Value: message.Enc8Bit},
		},
		Encoding: message.Enc8Bit,
		Content:  readerOf(src),
	}
}

// readerOf adapts a reader to the OutPart content source, which owns the
// close. Nothing here holds an OS resource, so closing is a no-op.
func readerOf(r io.Reader) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) { return io.NopCloser(r), nil }
}

// addressOf converts a report Address to the generator's form; a "" name
// means a bare addr-spec with no display-name.
func addressOf(a Address) message.Address {
	if a.Name == "" {
		return message.Address{Email: a.Email}
	}
	name := a.Name
	return message.Address{Name: &name, Email: a.Email}
}

// domainOf returns the domain of an addr-spec, for scoping a generated
// Message-ID (RFC 5322 section 3.6.4). Validation has already established
// the address has one.
func domainOf(email string) string {
	if at := strings.LastIndexByte(email, '@'); at >= 0 {
		return email[at+1:]
	}
	return "localhost"
}

// randomToken returns 128 bits of randomness as hex, the unique half of a
// generated Message-ID. RFC 8098 section 3 requires an MDN's Message-ID
// differ from the original message's, which randomness settles.
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("report: reading random message-id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
