// Package report parses inbound reports: recognizing a delivery status
// notification (RFC 3464) or message disposition notification (RFC 8098)
// inside the multipart/report container (RFC 6522), and reducing it to the
// few values submission correlation needs. The parse is deliberately
// fail-open: a report this package cannot read with confidence yields nil,
// and the message is delivered as ordinary mail - misreading can only cost a
// convenience, never forge a delivery verdict.
package report

import (
	"bytes"
	"strings"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// Report kinds and the state-machine slots one report can consume (see
// ingestReport).
const (
	KindDSN = "dsn"
	KindMDN = "mdn"
)

// Recipient is one per-recipient field group of a DSN (RFC 3464 section
// 2.3): who the report is about, what happened, and the diagnostic.
type Recipient struct {
	Addr     string // Final-Recipient (or Original-Recipient) generic-address
	Action   string // Action field, lowercased: failed/delayed/delivered/relayed/expanded
	Status   string // Status field, e.g. "5.1.1"
	SMTPDiag string // Diagnostic-Code text when its type is smtp, else ""
}

// DeliveryStatus is a parsed message/delivery-status content (RFC 3464
// section 2): the per-message envelope id and one Recipient per
// per-recipient field group.
type DeliveryStatus struct {
	Envid string // Original-Envelope-Id (RFC 3464 section 2.2.1)
	Rcpts []Recipient
}

// Notification is a parsed message/disposition-notification content (RFC
// 8098 section 3.1).
type Notification struct {
	OrigMessageID  string // Original-Message-ID (RFC 8098 section 3.2.5): the only MDN correlation key
	FinalRecipient string // Final-Recipient generic-address (section 3.2.4)
	Disposition    string // disposition-type, lowercased (section 3.2.6): displayed/deleted/dispatched/processed
}

// Inbound is one recognized report reduced to its correlation keys and
// per-recipient content.
type Inbound struct {
	Kind string // KindDSN or KindMDN

	// DSN correlation and content.
	Envid             string // Original-Envelope-Id (RFC 3464 section 2.2.1)
	ReturnedMessageID string // Message-ID of the returned content, for opt-in fallback correlation
	Rcpts             []Recipient

	// MDN correlation and content.
	OrigMessageID  string // Original-Message-ID (RFC 8098 section 3.2.5): the only MDN correlation key
	FinalRecipient string // Final-Recipient generic-address (section 3.2.4)
	Disposition    string // disposition-type, lowercased (section 3.2.6): displayed/deleted/dispatched/processed
}

// ParseDeliveryStatus reads message/delivery-status content (RFC 3464
// section 2): a per-message field group, then one field group per
// recipient. A report with no usable per-recipient group (Final-Recipient
// or Original-Recipient plus Action, both required by section 2.3) is not
// usable for correlation and yields nil.
func ParseDeliveryStatus(raw []byte) *DeliveryStatus {
	groups := ParseFieldGroups(raw)
	if len(groups) < 2 {
		return nil
	}
	ds := &DeliveryStatus{}
	ds.Envid = strings.TrimSpace(groupField(groups[0], "Original-Envelope-Id"))
	for _, g := range groups[1:] {
		r := Recipient{
			Addr:   typedAddress(groupField(g, "Final-Recipient")),
			Action: strings.ToLower(strings.TrimSpace(groupField(g, "Action"))),
			Status: strings.TrimSpace(groupField(g, "Status")),
		}
		if r.Addr == "" {
			r.Addr = typedAddress(groupField(g, "Original-Recipient"))
		}
		if typ, text, ok := splitTyped(groupField(g, "Diagnostic-Code")); ok && strings.EqualFold(typ, "smtp") {
			// Multiline SMTP diagnostics were folded; collapse to one line so
			// the value is usable as an smtpReply string.
			r.SMTPDiag = oneLine(text)
		}
		if r.Addr == "" || r.Action == "" {
			continue
		}
		ds.Rcpts = append(ds.Rcpts, r)
	}
	if len(ds.Rcpts) == 0 {
		return nil
	}
	return ds
}

// ParseDispositionNotification reads message/disposition-notification
// content (RFC 8098 section 3.1): a single field group. The Disposition
// field is required; Original-Message-ID is the correlation key, so a
// notification without one yields nil (there is nothing to correlate by).
func ParseDispositionNotification(raw []byte) *Notification {
	groups := ParseFieldGroups(raw)
	if len(groups) == 0 {
		return nil
	}
	g := groups[0]
	disp := groupField(g, "Disposition")
	if disp == "" {
		return nil
	}
	n := &Notification{
		FinalRecipient: typedAddress(groupField(g, "Final-Recipient")),
	}
	// Disposition: disposition-mode ";" disposition-type [/ modifiers]
	// (section 3.2.6). Only the type matters here.
	if _, after, ok := strings.Cut(disp, ";"); ok {
		typ := after
		if i := strings.IndexByte(typ, '/'); i >= 0 {
			typ = typ[:i]
		}
		n.Disposition = strings.ToLower(strings.TrimSpace(typ))
	}
	if n.Disposition == "" {
		return nil
	}
	if ids := message.MessageIDsForm(groupField(g, "Original-Message-ID")); len(ids) > 0 {
		n.OrigMessageID = ids[0]
	}
	if n.OrigMessageID == "" {
		return nil
	}
	return n
}

// FieldGroup is one blank-line-delimited group of unfolded header-format
// fields (RFC 3464 section 2.1).
type FieldGroup []headerKV

// ParseFieldGroups splits header-format content into groups of fields
// separated by blank lines (the "*(field CRLF) CRLF" structure of RFC 3464
// section 2.1), unfolding continuation lines (RFC 5322 section 2.2.3).
// Fields whose lines carry no colon are skipped, not errors.
func ParseFieldGroups(raw []byte) []FieldGroup {
	var groups []FieldGroup
	var cur FieldGroup
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
func groupField(g FieldGroup, name string) string {
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

// MessageIDFromHeaderBlock reads the Message-ID from the leading header
// block of returned content: a full message (message/rfc822) or just its
// header section (text/rfc822-headers, RFC 6522). The block ends at the
// first blank line; folding is unfolded the same way the group parser does.
func MessageIDFromHeaderBlock(raw []byte) string {
	groups := ParseFieldGroups(raw)
	if len(groups) == 0 {
		return ""
	}
	if ids := message.MessageIDsForm(groupField(groups[0], "Message-ID")); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// oneLine collapses a reason to a single reply line (no CR/LF can leak into
// the protocol stream).
//
// This duplicates lmtp.go's oneLine (root package): report must not import
// root, so the same small helper is defined here rather than shared.
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
