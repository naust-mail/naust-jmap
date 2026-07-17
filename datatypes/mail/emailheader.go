package mail

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// headerForm is a parsed-form name for a header:{name} property (RFC 8621
// section 4.1.2).
type headerForm int

const (
	formRaw headerForm = iota
	formText
	formAddresses
	formGroupedAddresses
	formMessageIds
	formDate
	formURLs
)

// headerFormByName maps the :as{Form} suffix to a headerForm.
var headerFormByName = map[string]headerForm{
	"Raw": formRaw, "Text": formText, "Addresses": formAddresses,
	"GroupedAddresses": formGroupedAddresses, "MessageIds": formMessageIds,
	"Date": formDate, "URLs": formURLs,
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

// headerProp is a parsed header:{name} property request.
type headerProp struct {
	field string     // header field name, original capitalization
	form  headerForm // requested parsed form
	all   bool       // :all suffix present
}

// parseHeaderProp parses a "header:{name}[:as{Form}][:all]" property name
// (RFC 8621 section 4.1.3). ok is false if the string is not a header
// property, is malformed, or requests a form inappropriate for a
// well-known field.
func parseHeaderProp(name string) (headerProp, bool) {
	rest, ok := strings.CutPrefix(name, "header:")
	if !ok {
		return headerProp{}, false
	}
	parts := strings.Split(rest, ":")
	field := parts[0]
	if !validHeaderFieldName(field) {
		return headerProp{}, false
	}
	hp := headerProp{field: field, form: formRaw}
	rest2 := parts[1:]
	// Optional :as{Form} then optional :all, in that order (section 4.1.3).
	if len(rest2) > 0 && strings.HasPrefix(rest2[0], "as") {
		form, ok := headerFormByName[rest2[0][2:]]
		if !ok {
			return headerProp{}, false
		}
		hp.form = form
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && rest2[0] == "all" {
		hp.all = true
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 {
		return headerProp{}, false // trailing junk
	}
	if !formAppropriate(field, hp.form) {
		return headerProp{}, false
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
func formAppropriate(field string, form headerForm) bool {
	if form == formRaw || form == formText {
		return true
	}
	lf := strings.ToLower(field)
	switch {
	case addressFields[lf]:
		return form == formAddresses || form == formGroupedAddresses
	case messageIdFields[lf]:
		return form == formMessageIds
	case dateFields[lf]:
		return form == formDate
	default:
		return true // no restriction known for this field
	}
}

// resolve computes the JSON value of the header property against a header
// list (RFC 8621 section 4.1.3): without :all, the last instance's parsed
// value or null; with :all, an array of the parsed value per instance in
// message order (empty array if none).
func (hp headerProp) resolve(headers []message.HeaderField) json.RawMessage {
	var instances []string
	for _, h := range headers {
		if strings.EqualFold(h.Name, hp.field) {
			instances = append(instances, h.Value)
		}
	}
	if hp.all {
		arr := make([]json.RawMessage, 0, len(instances))
		for _, raw := range instances {
			arr = append(arr, hp.parseOne(raw))
		}
		return mustJSON(arr)
	}
	if len(instances) == 0 {
		return jsonNull
	}
	return hp.parseOne(instances[len(instances)-1])
}

// parseOne applies the form to one raw header value. A nil/failed
// structured parse yields JSON null, matching the form functions' contract.
func (hp headerProp) parseOne(raw string) json.RawMessage {
	switch hp.form {
	case formText:
		return mustJSON(message.TextForm(raw))
	case formAddresses:
		return marshalOrNull(message.AddressesForm(raw))
	case formGroupedAddresses:
		return marshalOrNull(message.GroupedAddressesForm(raw))
	case formMessageIds:
		return marshalOrNull(message.MessageIDsForm(raw))
	case formURLs:
		return marshalOrNull(message.URLsForm(raw))
	case formDate:
		t := message.DateForm(raw)
		if t == nil {
			return jsonNull
		}
		return mustJSON(t.Format(time.RFC3339))
	default: // formRaw
		return mustJSON(raw)
	}
}

// marshalOrNull marshals v, but a nil slice becomes JSON null rather than
// an empty array (the form functions return nil to mean "no value").
func marshalOrNull[T any](v []T) json.RawMessage {
	if v == nil {
		return jsonNull
	}
	return mustJSON(v)
}
