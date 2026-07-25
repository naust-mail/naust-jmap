package mail

// Inbound report parsing: recognizing a delivery status notification
// (RFC 3464) or message disposition notification (RFC 8098) inside the
// multipart/report container (RFC 6522), and reducing it to the few values
// submission correlation needs. The parse is deliberately fail-open: a
// report this file cannot read with confidence yields nil, and the message
// is delivered as ordinary mail - misreading can only cost a convenience,
// never forge a delivery verdict.

import (
	"bytes"
	"strings"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// maxReportCapture bounds what one report part's sink retains. The
// machine-parsable content is a short group of header-format fields per
// message and per recipient (RFC 3464 section 2, RFC 8098 section 3.1), and
// correlation needs only the leading header block of the returned content -
// so a conformant report uses a fraction of this. A part that overruns the
// bound is left uninterpreted (the sink marks it) and the message falls back
// to ordinary delivery.
const maxReportCapture = 64 << 10

// reportSink captures one report part's decoded content up to
// maxReportCapture, remembering when there was more.
type reportSink struct {
	raw  []byte
	over bool
}

func (s *reportSink) Write(b []byte) (int, error) {
	if room := maxReportCapture - len(s.raw); room > 0 {
		if len(b) > room {
			b = b[:room]
			s.over = true
		}
		s.raw = append(s.raw, b...)
	} else {
		s.over = true
	}
	return len(b), nil // octets past the bound are discarded, not an error
}

func (s *reportSink) Close() error { return nil }

// Report kinds and the state-machine slots one report can consume (see
// ingestReport).
const (
	reportKindDSN = "dsn"
	reportKindMDN = "mdn"
)

// dsnRecipient is one per-recipient field group of a DSN (RFC 3464 section
// 2.3): who the report is about, what happened, and the diagnostic.
type dsnRecipient struct {
	addr     string // Final-Recipient (or Original-Recipient) generic-address
	action   string // Action field, lowercased: failed/delayed/delivered/relayed/expanded
	status   string // Status field, e.g. "5.1.1"
	smtpDiag string // Diagnostic-Code text when its type is smtp, else ""
}

// inboundReport is one recognized report reduced to its correlation keys
// and per-recipient content.
type inboundReport struct {
	kind string // reportKindDSN or reportKindMDN

	// DSN correlation and content.
	envid             string // Original-Envelope-Id (RFC 3464 section 2.2.1)
	returnedMessageID string // Message-ID of the returned content, for opt-in fallback correlation
	rcpts             []dsnRecipient

	// MDN correlation and content.
	origMessageID  string // Original-Message-ID (RFC 8098 section 3.2.5): the only MDN correlation key
	finalRecipient string // Final-Recipient generic-address (section 3.2.4)
	disposition    string // disposition-type, lowercased (section 3.2.6): displayed/deleted/dispatched/processed
}

// extractReport interprets a parsed message as a report. It returns nil
// unless the message is a well-formed multipart/report (RFC 6522 section 3:
// the machine-parsable status is the second body part; its own media type,
// not the report-type parameter, decides how it is read) whose machine part
// was captured whole.
func extractReport(p *parsed) *inboundReport {
	root := p.msg.Root
	if root.Type != "multipart/report" || len(root.SubParts) < 2 {
		return nil
	}
	machine := root.SubParts[1]
	sink := p.cap.reportSinks[machine]
	if sink == nil || sink.over {
		return nil
	}
	var rep *inboundReport
	switch machine.Type {
	case "message/delivery-status":
		rep = parseDeliveryStatus(sink.raw)
	case "message/disposition-notification":
		rep = parseDispositionNotification(sink.raw)
	}
	if rep == nil {
		return nil
	}
	// The third part, when present, is the returned content (RFC 6522
	// section 3): the original message or its headers, whose Message-ID is
	// the fallback correlation key.
	if len(root.SubParts) >= 3 {
		if s := p.cap.reportSinks[root.SubParts[2]]; s != nil && !s.over {
			rep.returnedMessageID = messageIDFromHeaderBlock(s.raw)
		}
	}
	return rep
}

// parseDeliveryStatus reads message/delivery-status content (RFC 3464
// section 2): a per-message field group, then one field group per
// recipient. A report with no usable per-recipient group (Final-Recipient
// or Original-Recipient plus Action, both required by section 2.3) is not
// usable for correlation and yields nil.
func parseDeliveryStatus(raw []byte) *inboundReport {
	groups := parseFieldGroups(raw)
	if len(groups) < 2 {
		return nil
	}
	rep := &inboundReport{kind: reportKindDSN}
	rep.envid = strings.TrimSpace(groupField(groups[0], "Original-Envelope-Id"))
	for _, g := range groups[1:] {
		r := dsnRecipient{
			addr:   typedAddress(groupField(g, "Final-Recipient")),
			action: strings.ToLower(strings.TrimSpace(groupField(g, "Action"))),
			status: strings.TrimSpace(groupField(g, "Status")),
		}
		if r.addr == "" {
			r.addr = typedAddress(groupField(g, "Original-Recipient"))
		}
		if typ, text, ok := splitTyped(groupField(g, "Diagnostic-Code")); ok && strings.EqualFold(typ, "smtp") {
			// Multiline SMTP diagnostics were folded; collapse to one line so
			// the value is usable as an smtpReply string.
			r.smtpDiag = oneLine(text)
		}
		if r.addr == "" || r.action == "" {
			continue
		}
		rep.rcpts = append(rep.rcpts, r)
	}
	if len(rep.rcpts) == 0 {
		return nil
	}
	return rep
}

// parseDispositionNotification reads message/disposition-notification
// content (RFC 8098 section 3.1): a single field group. The Disposition
// field is required; Original-Message-ID is the correlation key, so a
// notification without one yields nil (there is nothing to correlate by).
func parseDispositionNotification(raw []byte) *inboundReport {
	groups := parseFieldGroups(raw)
	if len(groups) == 0 {
		return nil
	}
	g := groups[0]
	disp := groupField(g, "Disposition")
	if disp == "" {
		return nil
	}
	rep := &inboundReport{
		kind:           reportKindMDN,
		finalRecipient: typedAddress(groupField(g, "Final-Recipient")),
	}
	// Disposition: disposition-mode ";" disposition-type [/ modifiers]
	// (section 3.2.6). Only the type matters here.
	if _, after, ok := strings.Cut(disp, ";"); ok {
		typ := after
		if i := strings.IndexByte(typ, '/'); i >= 0 {
			typ = typ[:i]
		}
		rep.disposition = strings.ToLower(strings.TrimSpace(typ))
	}
	if rep.disposition == "" {
		return nil
	}
	if ids := message.MessageIDsForm(groupField(g, "Original-Message-ID")); len(ids) > 0 {
		rep.origMessageID = ids[0]
	}
	if rep.origMessageID == "" {
		return nil
	}
	return rep
}

// parseFieldGroups splits header-format content into groups of fields
// separated by blank lines (the "*(field CRLF) CRLF" structure of RFC 3464
// section 2.1), unfolding continuation lines (RFC 5322 section 2.2.3).
// Fields whose lines carry no colon are skipped, not errors.
func parseFieldGroups(raw []byte) [][]headerKV {
	var groups [][]headerKV
	var cur []headerKV
	flushGroup := func() {
		if len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
	}
	lines := bytes.Split(raw, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			flushGroup()
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(cur) > 0 { // folded continuation of the previous field
				cur[len(cur)-1].value += " " + strings.TrimSpace(string(line))
			}
			continue
		}
		name, value, ok := strings.Cut(string(line), ":")
		if !ok {
			continue
		}
		cur = append(cur, headerKV{name: strings.TrimSpace(name), value: strings.TrimSpace(value)})
	}
	flushGroup()
	return groups
}

// headerKV is one unfolded header-format field.
type headerKV struct{ name, value string }

// groupField returns the first value of the named field in a group, "" when
// absent. Field names are case-insensitive (RFC 5322 section 1.2.2).
func groupField(g []headerKV, name string) string {
	for _, f := range g {
		if strings.EqualFold(f.name, name) {
			return f.value
		}
	}
	return ""
}

// splitTyped splits a "type; value" field (RFC 3464's address-type and
// diagnostic-type prefixes).
func splitTyped(v string) (typ, value string, ok bool) {
	typ, value, ok = strings.Cut(v, ";")
	return strings.TrimSpace(typ), strings.TrimSpace(value), ok
}

// typedAddress extracts the generic-address of a "type; address" field,
// tolerating a bare address with no type prefix. Only the rfc822 (and
// utf-8, RFC 6533) address types name an email address this package can
// match an envelope against; other types yield "".
func typedAddress(v string) string {
	typ, addr, ok := splitTyped(v)
	if !ok {
		return strings.TrimSpace(v)
	}
	if !strings.EqualFold(typ, "rfc822") && !strings.EqualFold(typ, "utf-8") {
		return ""
	}
	return strings.TrimSpace(addr)
}

// messageIDFromHeaderBlock reads the Message-ID from the leading header
// block of returned content: a full message (message/rfc822) or just its
// header section (text/rfc822-headers, RFC 6522). The block ends at the
// first blank line; folding is unfolded the same way the group parser does.
func messageIDFromHeaderBlock(raw []byte) string {
	groups := parseFieldGroups(raw)
	if len(groups) == 0 {
		return ""
	}
	if ids := message.MessageIDsForm(groupField(groups[0], "Message-ID")); len(ids) > 0 {
		return ids[0]
	}
	return ""
}
