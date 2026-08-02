package report

// Parsing a complete message disposition notification message back into
// the vocabulary Write generates it from (RFC 8098 section 3, RFC 6522
// section 3). Where the rest of this package reduces an inbound report to
// the few values submission correlation needs, ParseMDN keeps every
// notification field, because its consumer renders the full MDN object
// (RFC 9007 section 2) rather than correlating a delivery.

import (
	"errors"
	"io"
	"strings"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
)

// ErrNotMDN reports that the input was read successfully but is not a
// message disposition notification this package recognizes: not a
// multipart/report, no message/disposition-notification second component
// (RFC 6522 section 3: the machine part's own media type is authoritative,
// the report-type parameter is not consulted), a machine part that
// overruns the capture bound, or notification content missing a value the
// RFC 8098 section 3.1 grammar requires (Final-Recipient, or a
// Disposition whose action-mode, sending-mode, and disposition-type all
// come from their closed word lists; only disposition modifiers are
// extensible).
var ErrNotMDN = errors.New("report: not a message disposition notification")

// ParsedMDN is one parsed MDN in the vocabulary of Message: the same
// fields Write consumes, read back. String fields hold "" where the
// notification omits the field; OriginalMessageID is the bare id without
// angle brackets, exactly the form Message.OriginalMessageID takes.
// Disposition values are lowercased (the RFC 8098 grammar words are
// case-insensitive on the wire).
type ParsedMDN struct {
	// Subject is the MDN message's decoded Subject header field.
	Subject string
	// TextBody is the human-readable first component (RFC 6522 section
	// 3), charset-decoded, when that component is a text/plain leaf;
	// "" otherwise. A body over the capture bound is truncated, not an
	// error - it is advisory text.
	TextBody string
	// ReportingUA is the Reporting-UA field value (RFC 8098 section
	// 3.2.1).
	ReportingUA string
	// MDNGateway is the MDN-Gateway field value (RFC 8098 section 3.2.2).
	MDNGateway string
	// OriginalRecipient is the Original-Recipient field value, its
	// address-type prefix kept (RFC 8098 section 3.2.3).
	OriginalRecipient string
	// FinalRecipient is the Final-Recipient field value, its
	// address-type prefix kept (RFC 8098 section 3.2.4). Always present:
	// the grammar requires the field.
	FinalRecipient string
	// OriginalMessageID is the Original-Message-ID without angle brackets
	// (RFC 8098 section 3.2.5), "" when the notification carries none.
	OriginalMessageID string
	// Disposition is the parsed Disposition field (RFC 8098 section
	// 3.2.6), all three values lowercased.
	Disposition Disposition
	// Errors are the Error field values, in notification order (RFC 8098
	// section 3.2.7).
	Errors []string
	// ExtensionFields are the notification fields beyond the RFC 8098
	// section 3.1 standard set, in notification order with their
	// original names (section 3.3).
	ExtensionFields []ExtensionField
	// HasOriginal reports a third component: the returned original
	// message or its header section (RFC 6522 section 3). Its content is
	// never read.
	HasOriginal bool
}

// ParseMDN reads a complete MDN message from r: the inverse of Write.
// The message is streamed, never buffered whole; the notification content
// and the human-readable text are each captured up to a fixed bound (the
// same bound delivery's report ingest applies), and the returned original
// in the third component is structure only - its content is never
// decoded or retained. Recognition is strict (see ErrNotMDN); any other
// error is a failure reading r.
func ParseMDN(r io.Reader) (*ParsedMDN, error) {
	sinks := map[*message.Part]*parse.ReportSink{}
	msg, err := message.Parse(r, func(p *message.Part) message.LeafSinks {
		switch p.Type {
		case "text/plain", "message/disposition-notification":
			s := &parse.ReportSink{}
			sinks[p] = s
			return message.LeafSinks{Sinks: []message.Sink{s}}
		}
		return message.LeafSinks{}
	})
	if err != nil {
		return nil, err
	}
	root := msg.Root
	if root.Type != "multipart/report" || len(root.SubParts) < 2 {
		return nil, ErrNotMDN
	}
	machine := root.SubParts[1]
	ms := sinks[machine]
	if machine.Type != "message/disposition-notification" || ms == nil || ms.Over {
		return nil, ErrNotMDN
	}
	groups := ParseFieldGroups(ms.Raw)
	if len(groups) == 0 {
		return nil, ErrNotMDN
	}
	g := groups[0]
	m := &ParsedMDN{HasOriginal: len(root.SubParts) >= 3}

	disp, ok := parseDispositionField(groupField(g, "Disposition"))
	if !ok {
		return nil, ErrNotMDN
	}
	m.Disposition = disp
	m.FinalRecipient = strings.TrimSpace(groupField(g, "Final-Recipient"))
	if m.FinalRecipient == "" {
		return nil, ErrNotMDN
	}
	m.ReportingUA = strings.TrimSpace(groupField(g, "Reporting-UA"))
	m.MDNGateway = strings.TrimSpace(groupField(g, "MDN-Gateway"))
	m.OriginalRecipient = strings.TrimSpace(groupField(g, "Original-Recipient"))
	if ids := message.MessageIDsForm(groupField(g, "Original-Message-ID")); len(ids) > 0 {
		m.OriginalMessageID = ids[0]
	}
	for _, f := range g {
		switch {
		case strings.EqualFold(f.name, "Error"):
			m.Errors = append(m.Errors, strings.TrimSpace(f.value))
		case !notificationFields[strings.ToLower(f.name)]:
			m.ExtensionFields = append(m.ExtensionFields, ExtensionField{Name: f.name, Value: strings.TrimSpace(f.value)})
		}
	}

	if raw, found := msg.HeaderLast("Subject"); found {
		m.Subject = message.TextForm(raw)
	}
	if first := root.SubParts[0]; first.Type == "text/plain" {
		if ts := sinks[first]; ts != nil {
			m.TextBody, _ = message.DecodeBody(ts.Raw, first.Charset)
		}
	}
	return m, nil
}

// parseDispositionField parses "action-mode/sending-mode; type[/modifiers]"
// (RFC 8098 section 3.2.6) into lowercased values, enforcing the closed
// word lists. Modifiers are dropped: the RFC 9007 section 2 Disposition
// object has no slot for them, and the error modifier's information
// travels in the Error fields.
func parseDispositionField(v string) (Disposition, bool) {
	mode, rest, found := strings.Cut(v, ";")
	if !found {
		return Disposition{}, false
	}
	am, sm, found := strings.Cut(mode, "/")
	if !found {
		return Disposition{}, false
	}
	d := Disposition{
		ActionMode:  strings.ToLower(strings.TrimSpace(am)),
		SendingMode: strings.ToLower(strings.TrimSpace(sm)),
	}
	typ := strings.TrimSpace(rest)
	if i := strings.IndexByte(typ, '/'); i >= 0 {
		typ = typ[:i]
	}
	d.Type = strings.ToLower(strings.TrimSpace(typ))
	if !actionModes[d.ActionMode] || !sendingModes[d.SendingMode] || !dispositionTypes[d.Type] {
		return Disposition{}, false
	}
	return d, true
}
