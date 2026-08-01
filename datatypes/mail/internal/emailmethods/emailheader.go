package emailmethods

import (
	"strings"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
)

// headerFormByName maps the :as{Form} suffix to a HeaderForm.
var headerFormByName = map[string]emailstore.HeaderForm{
	"Raw": emailstore.FormRaw, "Text": emailstore.FormText, "Addresses": emailstore.FormAddresses,
	"GroupedAddresses": emailstore.FormGroupedAddresses, "MessageIds": emailstore.FormMessageIds,
	"Date": emailstore.FormDate, "URLs": emailstore.FormURLs,
}

// Well-known header fields grouped by the structured form appropriate to
// them (RFC 8621 section 4.2: a form used on an inappropriate field, e.g.
// header:From:asDate, MUST be rejected). Fields outside these groups
// (Subject, List-*, X-*, ...) accept any form on a best-effort basis.
var (
	addressFields = map[string]bool{
		"from": true, "sender": true, "to": true, "cc": true, "bcc": true,
		"reply-to": true, "resent-from": true, "resent-sender": true,
		"resent-to": true, "resent-cc": true, "resent-bcc": true,
		"resent-reply-to": true,
	}
	messageIdFields = map[string]bool{
		"message-id": true, "in-reply-to": true, "references": true,
		"resent-message-id": true,
	}
	dateFields = map[string]bool{"date": true, "resent-date": true}
)

// parseHeaderProp parses a "header:{name}[:as{Form}][:all]" property name
// (RFC 8621 section 4.1.3). ok is false if the string is not a header
// property, is malformed, or requests a form inappropriate for a
// well-known field.
func parseHeaderProp(name string) (emailstore.HeaderProp, bool) {
	rest, ok := strings.CutPrefix(name, "header:")
	if !ok {
		return emailstore.HeaderProp{}, false
	}
	parts := strings.Split(rest, ":")
	field := parts[0]
	if !validHeaderFieldName(field) {
		return emailstore.HeaderProp{}, false
	}
	hp := emailstore.HeaderProp{Field: field, Form: emailstore.FormRaw}
	rest2 := parts[1:]
	// Optional :as{Form} then optional :all, in that order (section 4.1.3).
	if len(rest2) > 0 && strings.HasPrefix(rest2[0], "as") {
		form, ok := headerFormByName[rest2[0][2:]]
		if !ok {
			return emailstore.HeaderProp{}, false
		}
		hp.Form = form
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && rest2[0] == "all" {
		hp.All = true
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 {
		return emailstore.HeaderProp{}, false // trailing junk
	}
	if !formAppropriate(field, hp.Form) {
		return emailstore.HeaderProp{}, false
	}
	return hp, true
}

// validHeaderFieldName reports whether s is one or more printable ASCII
// characters excluding colon (RFC 8621 section 4.1.3).
func validHeaderFieldName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 || r == ':' {
			return false
		}
	}
	return true
}

// formAppropriate reports whether form may be applied to field. Structured
// forms are restricted to their well-known fields; the Raw and Text forms
// apply to any field, and unknown fields accept any form.
func formAppropriate(field string, form emailstore.HeaderForm) bool {
	if form == emailstore.FormRaw || form == emailstore.FormText {
		return true
	}
	lf := strings.ToLower(field)
	switch {
	case addressFields[lf]:
		return form == emailstore.FormAddresses || form == emailstore.FormGroupedAddresses
	case messageIdFields[lf]:
		return form == emailstore.FormMessageIds
	case dateFields[lf]:
		return form == emailstore.FormDate
	default:
		return true // no restriction known for this field
	}
}
