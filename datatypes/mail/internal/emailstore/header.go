package emailstore

// The header form builders: turning one or more raw header field values
// into the parsed forms RFC 8621 section 4.1.2 defines (Text, Addresses,
// GroupedAddresses, MessageIds, Date, URLs, or Raw). BuildEmailRecord uses
// these to compute the convenience header properties at insert time; the
// root package's Email/get computed resolver uses the same HeaderProp type
// and Resolve method to answer an on-demand header:{name} request, so the
// stored value and the on-demand value can never disagree. Parsing the
// header:{name} property NAME itself (the "header:" prefix, :asForm/:all
// suffixes, and which forms are appropriate for a well-known field) is
// Email/get rendering surface and stays in the root package.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// HeaderForm is a parsed-form name for a header:{name} property (RFC 8621
// section 4.1.2).
type HeaderForm int

const (
	FormRaw HeaderForm = iota
	FormText
	FormAddresses
	FormGroupedAddresses
	FormMessageIds
	FormDate
	FormURLs
)

// HeaderProp is a parsed header:{name} property request.
type HeaderProp struct {
	Field string     // header field name, original capitalization
	Form  HeaderForm // requested parsed form
	All   bool       // :all suffix present
}

// Resolve computes the JSON value of the header property against a header
// list (RFC 8621 section 4.1.3): without :all, the last instance's parsed
// value or null; with :all, an array of the parsed value per instance in
// message order (empty array if none).
func (hp HeaderProp) Resolve(headers []message.HeaderField) json.RawMessage {
	var instances []string
	for _, h := range headers {
		if strings.EqualFold(h.Name, hp.Field) {
			instances = append(instances, h.Value)
		}
	}
	if hp.All {
		arr := make([]json.RawMessage, 0, len(instances))
		for _, raw := range instances {
			arr = append(arr, hp.parseOne(raw))
		}
		return record.MustJSON(arr)
	}
	if len(instances) == 0 {
		return record.JSONNull
	}
	return hp.parseOne(instances[len(instances)-1])
}

// parseOne applies the form to one raw header value. A nil/failed
// structured parse yields JSON null, matching the form functions' contract.
func (hp HeaderProp) parseOne(raw string) json.RawMessage {
	switch hp.Form {
	case FormText:
		return record.MustJSON(message.TextForm(raw))
	case FormAddresses:
		return marshalOrNull(message.AddressesForm(raw))
	case FormGroupedAddresses:
		return marshalOrNull(message.GroupedAddressesForm(raw))
	case FormMessageIds:
		return marshalOrNull(message.MessageIDsForm(raw))
	case FormURLs:
		return marshalOrNull(message.URLsForm(raw))
	case FormDate:
		t := message.DateForm(raw)
		if t == nil {
			return record.JSONNull
		}
		return record.MustJSON(t.Format(time.RFC3339))
	default: // FormRaw
		return record.MustJSON(raw)
	}
}

// marshalOrNull marshals v, but a nil slice becomes JSON null rather than
// an empty array (the form functions return nil to mean "no value").
func marshalOrNull[T any](v []T) json.RawMessage {
	if v == nil {
		return record.JSONNull
	}
	return record.MustJSON(v)
}
