package mail

// Message generation for Email/set create (RFC 8621 section 4.6): turn a
// client-submitted Email creation object into an RFC 5322 message,
// streamed - a blobId attachment is never buffered. This file owns the
// property-level work: the section 4.6 ambiguity rules are enforced with
// invalidProperties (STRICT - the spec's "server MAY choose to modify"
// alternative hides client bugs behind undocumented policy), header
// properties are serialized through the write-side form functions, and
// the mandatory Message-ID and Date are synthesized when absent. The
// body-part tree is built in emailgenbody.go; the wire framing lives in
// internal/message (WriteMessage).
//
// Validity beyond the 4.6 list is deliberately NOT enforced: a
// half-finished draft is legal ("the final message generated may be
// invalid per RFC 5322"), and strict conformance is checked at
// submission, not creation.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// genConfig configures message generation.
type genConfig struct {
	// msgIDDomain is the domain synthesized Message-IDs are scoped to
	// (RFC 5322 section 3.6.4). Empty falls back to the domain of the
	// first From address, then "localhost".
	msgIDDomain string
}

// blobOpener opens a referenced blob's content for streaming into the
// message. The generator calls it once per blobId leaf, at write time.
type blobOpener func(ctx context.Context, blobID jmap.Id) (io.ReadCloser, error)

// outMessage is a planned outbound message, ready to stream.
type outMessage struct {
	headers []message.HeaderField
	root    *message.OutPart
	// blobIds lists every blob the body references (deduplicated, sorted),
	// so the caller can verify existence up front and answer blobNotFound
	// with the spec's complete notFound list (section 4.6).
	blobIds []jmap.Id
}

// write streams the message to w.
func (m *outMessage) write(ctx context.Context, w io.Writer) error {
	return message.WriteMessage(ctx, w, m.headers, m.root)
}

// headerSpec is one header-field-bearing property of the creation object.
type headerSpec struct {
	prop  string // the JSON property name, for error reporting
	field string // the header field name as it will be written
	form  headerForm
	all   bool
	raw   json.RawMessage
}

// convenienceHeaders maps the top-level convenience properties (RFC 8621
// section 4.1.2.3-4.1.2.6 usage in section 4.1.1) to their header field
// and parsed form.
var convenienceHeaders = map[string]struct {
	field string
	form  headerForm
}{
	"messageId":  {"Message-ID", formMessageIds},
	"inReplyTo":  {"In-Reply-To", formMessageIds},
	"references": {"References", formMessageIds},
	"sender":     {"Sender", formAddresses},
	"from":       {"From", formAddresses},
	"to":         {"To", formAddresses},
	"cc":         {"Cc", formAddresses},
	"bcc":        {"Bcc", formAddresses},
	"replyTo":    {"Reply-To", formAddresses},
	"subject":    {"Subject", formText},
	"sentAt":     {"Date", formDate},
}

// headerWriteOrder is the canonical order of the well-known fields in a
// generated message. Fields outside it follow, ordered by property name
// (the creation object is a JSON map, so it carries no order of its own).
var headerWriteOrder = map[string]int{
	"date": 0, "from": 1, "sender": 2, "reply-to": 3, "to": 4, "cc": 5,
	"bcc": 6, "subject": 7, "message-id": 8, "in-reply-to": 9,
	"references": 10,
}

// planEmailMessage validates a creation object against the section 4.6
// constraint list and plans the outbound message. The caller handles the
// non-message properties (mailboxIds, keywords, receivedAt) itself; now
// feeds the synthesized Date, and open is bound into the plan's blobId
// leaves for write time.
func planEmailMessage(obj map[string]json.RawMessage, cfg genConfig, now time.Time, open blobOpener) (*outMessage, *jmap.SetError) {
	var specs []headerSpec
	var body bodyProps
	for prop, raw := range obj {
		switch prop {
		case "mailboxIds", "keywords", "receivedAt":
			continue
		case "headers":
			// 4.6: "The headers property MUST NOT be given ... the client
			// must set each header field as an individual property."
			return nil, invalidProp("headers", "set each header field as an individual property")
		case "bodyStructure":
			body.structure = raw
			continue
		case "textBody":
			body.text = raw
			continue
		case "htmlBody":
			body.html = raw
			continue
		case "attachments":
			body.attachments = raw
			continue
		case "bodyValues":
			body.values = raw
			continue
		}
		if conv, ok := convenienceHeaders[prop]; ok {
			specs = append(specs, headerSpec{prop: prop, field: conv.field, form: conv.form, raw: raw})
			continue
		}
		if hp, ok := parseHeaderProp(prop); ok {
			lf := strings.ToLower(hp.field)
			// 4.6: header fields beginning with "Content-" MUST NOT be
			// specified on the Email object, only on EmailBodyPart objects.
			if strings.HasPrefix(lf, "content-") {
				return nil, invalidProp(prop, "Content-* header fields belong on body parts")
			}
			if lf == "mime-version" {
				return nil, invalidProp(prop, "MIME-Version is set by the server")
			}
			specs = append(specs, headerSpec{prop: prop, field: hp.field, form: hp.form, all: hp.all, raw: raw})
			continue
		}
		// parseHeaderProp also rejects a form inappropriate for its field
		// (4.6: "header fields MUST NOT be specified in parsed forms that
		// are forbidden for that particular field").
		return nil, invalidProp(prop, "unknown property, or a form not allowed for this header field")
	}

	// 4.6: "There MUST NOT be two properties that represent the same
	// header field."
	fields := map[string][]string{}
	for _, s := range specs {
		lf := strings.ToLower(s.field)
		fields[lf] = append(fields[lf], s.prop)
	}
	var dup []string
	for _, props := range fields {
		if len(props) > 1 {
			dup = append(dup, props...)
		}
	}
	if dup != nil {
		sort.Strings(dup)
		return nil, &jmap.SetError{
			Type: jmap.SetErrInvalidProperties, Properties: dup,
			Description: "multiple properties represent the same header field",
		}
	}

	sort.SliceStable(specs, func(i, j int) bool {
		ri, iw := headerWriteOrder[strings.ToLower(specs[i].field)]
		rj, jw := headerWriteOrder[strings.ToLower(specs[j].field)]
		if iw != jw {
			return iw
		}
		if iw && ri != rj {
			return ri < rj
		}
		return specs[i].prop < specs[j].prop
	})
	var headers []message.HeaderField
	for _, s := range specs {
		hf, serr := serializeHeader(s)
		if serr != nil {
			return nil, serr
		}
		headers = append(headers, hf...)
	}

	// 4.6: the server MUST generate Message-ID (RFC 5322 3.6.4) and Date
	// (3.6.1) when the client did not include them.
	if _, has := fields["date"]; !has {
		headers = append([]message.HeaderField{{Name: "Date", Value: message.FormatDate(now)}}, headers...)
	}
	if _, has := fields["message-id"]; !has {
		headers = append(headers, message.HeaderField{Name: "Message-ID", Value: "<" + randomToken() + "@" + msgIDDomain(cfg, obj) + ">"})
	}
	headers = append(headers, message.HeaderField{Name: "MIME-Version", Value: "1.0"})

	root, blobIds, rootExtras, serr := planBody(body, open)
	if serr != nil {
		return nil, serr
	}
	// 4.6: the bodyStructure part must not carry a header field already
	// defined on the Email object. The check runs on whichever part became
	// the root, because the root's headers share the message's single
	// header block regardless of which property declared the part.
	for _, f := range rootExtras {
		if props, has := fields[f]; has {
			return nil, &jmap.SetError{
				Type: jmap.SetErrInvalidProperties, Properties: props,
				Description: "header field also present on the message's top part",
			}
		}
	}
	return &outMessage{headers: headers, root: root, blobIds: blobIds}, nil
}

// msgIDDomain resolves the domain a synthesized Message-ID is scoped to:
// the configured domain, else the first From address's domain, else
// "localhost" (any domain the generator can scope uniqueness to is legal
// per RFC 5322 section 3.6.4).
func msgIDDomain(cfg genConfig, obj map[string]json.RawMessage) string {
	if cfg.msgIDDomain != "" {
		return cfg.msgIDDomain
	}
	var from []message.Address
	if raw, ok := obj["from"]; ok && json.Unmarshal(raw, &from) == nil && len(from) > 0 {
		if _, domain, ok := splitAddr(from[0].Email); ok && isTokenSafe(domain) {
			return domain
		}
	}
	return "localhost"
}

// randomToken returns 128 bits of randomness as hex, the unique half of a
// synthesized Message-ID.
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("mail: reading random message-id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// serializeHeader turns one header property into header field instances:
// one for a plain property, one per array element for an :all property. A
// JSON null (or an empty list value) contributes no instance.
func serializeHeader(s headerSpec) ([]message.HeaderField, *jmap.SetError) {
	vals := []json.RawMessage{s.raw}
	if s.all {
		if isNullRaw(s.raw) {
			return nil, nil
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(s.raw, &arr); err != nil {
			return nil, invalidProp(s.prop, "an :all header property takes an array of values")
		}
		vals = arr
	}
	var out []message.HeaderField
	for _, v := range vals {
		if isNullRaw(v) {
			continue
		}
		value, ok := serializeHeaderValue(s.field, s.form, v)
		if !ok {
			return nil, invalidProp(s.prop, "value does not fit the property's form")
		}
		if value == "" && s.form != formRaw && s.form != formText {
			continue // an empty list serializes to no header instance
		}
		out = append(out, message.HeaderField{Name: s.field, Value: value})
	}
	return out, nil
}

// serializeHeaderValue serializes one JSON value in the given parsed form
// into a raw header value (the inverse of headerProp.parseOne). ok is
// false when the value does not fit the form or cannot be represented in
// a message header.
func serializeHeaderValue(field string, form headerForm, raw json.RawMessage) (string, bool) {
	switch form {
	case formText:
		var v string
		if json.Unmarshal(raw, &v) != nil || hasCtl(v) {
			return "", false
		}
		return message.FoldValue(field, message.EncodeText(v)), true
	case formAddresses:
		var list []message.Address
		if json.Unmarshal(raw, &list) != nil || !addressesOK(list) {
			return "", false
		}
		atoms, ok := message.FormatAddresses(list)
		if !ok {
			return "", false
		}
		return message.FoldValue(field, atoms), true
	case formGroupedAddresses:
		var groups []message.AddressGroup
		if json.Unmarshal(raw, &groups) != nil {
			return "", false
		}
		for _, g := range groups {
			if g.Name != nil && hasCtl(*g.Name) || !addressesOK(g.Addresses) {
				return "", false
			}
		}
		atoms, ok := message.FormatGroupedAddresses(groups)
		if !ok {
			return "", false
		}
		return message.FoldValue(field, atoms), true
	case formMessageIds:
		var ids []string
		if json.Unmarshal(raw, &ids) != nil {
			return "", false
		}
		atoms, ok := message.FormatMessageIDs(ids)
		if !ok {
			return "", false
		}
		return message.FoldValue(field, atoms), true
	case formDate:
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return "", false
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return "", false
		}
		return message.FormatDate(t), true
	case formURLs:
		var urls []string
		if json.Unmarshal(raw, &urls) != nil {
			return "", false
		}
		atoms, ok := message.FormatURLs(urls)
		if !ok {
			return "", false
		}
		return message.FoldValue(field, atoms), true
	default: // formRaw
		var v string
		if json.Unmarshal(raw, &v) != nil || !validRawHeaderValue(v) {
			return "", false
		}
		return v, true
	}
}

// addressesOK vets an EmailAddress list for serialization: every element
// carries an email, and no display name smuggles control characters.
func addressesOK(list []message.Address) bool {
	for _, a := range list {
		if a.Email == "" || (a.Name != nil && hasCtl(*a.Name)) {
			return false
		}
	}
	return true
}

// validRawHeaderValue accepts a client-provided Raw form value: CR/LF only
// as a proper fold (CRLF followed by white space, RFC 5322 section 2.2.3 -
// anything else is header injection), and no other control characters
// besides horizontal tab.
func validRawHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '\r':
			if i+2 >= len(v) || v[i+1] != '\n' || (v[i+2] != ' ' && v[i+2] != '\t') {
				return false
			}
			i++
		case c == '\n', c == 0x7f, c < 0x20 && c != '\t':
			return false
		}
	}
	return true
}

// hasCtl reports whether s contains any control character.
func hasCtl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// isTokenSafe reports whether s is printable ASCII with no white space -
// safe inside an angle-bracketed or parameter context.
func isTokenSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return s != ""
}
