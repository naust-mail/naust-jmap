package report

// Validation of an outbound MDN, run in full before Write emits a byte.
//
// Two rules decide every case here. Anything the caller supplies that lands
// in a header field must not be able to end that field early: a bare CR or
// LF would start a new field, and a semicolon or angle bracket would break
// the structured grammars of RFC 8098 section 3.2. And the mode and type
// values of the Disposition field are a closed vocabulary (section 3.2.6),
// so an unrecognized one is a caller bug, not something to pass through.
//
// Every failure is an error, never a silent rewrite. This is server-side
// generation: a value that cannot be represented means the caller built the
// wrong Message, and sanitizing would hide that while changing the report's
// meaning.

import (
	"fmt"
	"strings"
)

// lineLimit is the RFC 5322 section 2.1.1 maximum line length, 998 octets
// excluding CRLF. The notification content fields are written unfolded, so
// a value that would exceed it is rejected rather than emitted as an
// illegal line.
const lineLimit = 998

// The closed vocabularies of the Disposition field (RFC 8098 sections
// 3.2.6.1 and 3.2.6.2), lowercased as RFC 9007 section 2 mandates.
var (
	actionModes      = map[string]bool{"manual-action": true, "automatic-action": true}
	sendingModes     = map[string]bool{"mdn-sent-manually": true, "mdn-sent-automatically": true}
	dispositionTypes = map[string]bool{"displayed": true, "deleted": true, "dispatched": true, "processed": true}
)

// notificationFields are the field names RFC 8098 section 3.1 defines for
// the notification content. An extension field (section 3.3) may not reuse
// one: a second Disposition or Final-Recipient would let a caller restate
// the report's verdict below the one this package generated.
var notificationFields = map[string]bool{
	"reporting-ua": true, "mdn-gateway": true, "original-recipient": true,
	"final-recipient": true, "original-message-id": true, "disposition": true,
	"error": true,
}

// validate reports the first reason m cannot be written.
func (m Message) validate() error {
	if err := validateAddress("From", m.From); err != nil {
		return err
	}
	if err := validateAddress("To", m.To); err != nil {
		return err
	}
	if !headerTextSafe(m.Subject) {
		return errf("Subject contains a line break or NUL")
	}
	if m.ReportingUA != "" && !textSafe(m.ReportingUA) {
		return errf("ReportingUA is not printable ASCII")
	}
	if err := validateGenericAddress("FinalRecipient", m.FinalRecipient, true); err != nil {
		return err
	}
	if err := validateGenericAddress("OriginalRecipient", m.OriginalRecipient, false); err != nil {
		return err
	}
	if m.OriginalMessageID != "" && !msgIDSafe(m.OriginalMessageID) {
		return errf("OriginalMessageID %q is not a msg-id without angle brackets", m.OriginalMessageID)
	}
	if err := m.Disposition.validate(); err != nil {
		return err
	}
	return m.validateExtensions()
}

// validate checks the Disposition field's three values against the closed
// grammar of RFC 8098 section 3.2.6.
func (d Disposition) validate() error {
	if !actionModes[d.ActionMode] {
		return errf("Disposition.ActionMode %q is not manual-action or automatic-action", d.ActionMode)
	}
	if !sendingModes[d.SendingMode] {
		return errf("Disposition.SendingMode %q is not mdn-sent-manually or mdn-sent-automatically", d.SendingMode)
	}
	if !dispositionTypes[d.Type] {
		return errf("Disposition.Type %q is not displayed, deleted, dispatched or processed", d.Type)
	}
	return nil
}

// validateExtensions checks each extension field (RFC 8098 section 3.3):
// the name is a field-name (RFC 5322 section 3.6.8, printable ASCII with
// no colon or white space), the value is one line of printable ASCII, and
// the pair fits an RFC 5322 line.
func (m Message) validateExtensions() error {
	for _, ext := range m.ExtensionFields {
		if !tokenSafe(ext.Name) || strings.ContainsAny(ext.Name, ":;") {
			return errf("extension field name %q is not a field-name", ext.Name)
		}
		if notificationFields[strings.ToLower(ext.Name)] {
			return errf("extension field name %q is a defined notification field", ext.Name)
		}
		if !textSafe(ext.Value) {
			return errf("extension field %q value is not printable ASCII", ext.Name)
		}
		if len(ext.Name)+2+len(ext.Value) > lineLimit {
			return errf("extension field %q exceeds the maximum line length", ext.Name)
		}
	}
	return nil
}

// validateAddress checks one mailbox of the MDN's own header block. The
// display-name may be any text (it is carried as RFC 2047 encoded-words
// when it is not ASCII), but not one carrying its own line break; the
// address itself must be a representable addr-spec.
func validateAddress(field string, a Address) error {
	if !headerTextSafe(a.Name) {
		return errf("%s display name contains a line break or NUL", field)
	}
	if !tokenSafe(a.Email) || strings.ContainsAny(a.Email, "<>,;") {
		return errf("%s address %q is not a usable addr-spec", field, a.Email)
	}
	if at := strings.LastIndexByte(a.Email, '@'); at <= 0 || at == len(a.Email)-1 {
		return errf("%s address %q has no domain", field, a.Email)
	}
	return nil
}

// validateGenericAddress checks a Final-Recipient or Original-Recipient
// generic-address (RFC 8098 sections 3.2.3 and 3.2.4). The address-type
// prefix is Write's to supply, so a value carrying a semicolon is rejected
// rather than reinterpreted.
func validateGenericAddress(field, value string, required bool) error {
	if value == "" {
		if required {
			return errf("%s is required", field)
		}
		return nil
	}
	if !tokenSafe(value) {
		return errf("%s %q is not printable ASCII without white space", field, value)
	}
	if strings.ContainsAny(value, ";<>") {
		return errf("%s %q must be a bare address with no address-type prefix", field, value)
	}
	if len(field)+2+len("rfc822; ")+len(value) > lineLimit {
		return errf("%s exceeds the maximum line length", field)
	}
	return nil
}

// msgIDSafe reports whether s is usable inside the angle brackets of a
// msg-id (RFC 5322 section 3.6.4): printable ASCII with no white space and
// no bracket of its own. It matches what the parse side hands back, which
// is a Message-ID with its brackets already stripped.
func msgIDSafe(s string) bool {
	return tokenSafe(s) && !strings.ContainsAny(s, "<>") && len(s) < lineLimit
}

// tokenSafe reports whether s is non-empty printable ASCII with no white
// space - safe in a header field position whose grammar admits neither
// folding nor quoting.
//
// This duplicates internal/addr's TokenSafe: report may import only
// internal/message and the standard library, so the same small predicate
// is defined here rather than shared.
func tokenSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return s != ""
}

// textSafe reports whether s is printable ASCII, spaces allowed - the
// "text" of a notification content field (RFC 8098 section 3.1), which has
// no encoded-word escape available to it (section 3.2.7) and so cannot
// carry anything else.
func textSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// headerTextSafe reports whether s can be carried in an unstructured
// header field of the MDN itself. Non-ASCII is fine, it becomes RFC 2047
// encoded-words; a CR, LF or NUL is not, because it would end the field.
func headerTextSafe(s string) bool {
	return !strings.ContainsAny(s, "\r\n\x00")
}

// errf builds a package-tagged error.
func errf(format string, args ...any) error {
	return fmt.Errorf("report: "+format, args...)
}
