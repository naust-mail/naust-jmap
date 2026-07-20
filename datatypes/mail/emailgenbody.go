package mail

// The body half of message generation (RFC 8621 section 4.6): validating
// the EmailBodyPart objects of a creation object, and building the
// outbound part tree - either the declared bodyStructure as is, or the
// convenience mapping of textBody/htmlBody/attachments:
//
//	multipart/mixed(
//	    multipart/related(              when cid-referenced parts exist
//	        multipart/alternative(text/plain, text/html),
//	        cid attachments...),
//	    other attachments...)
//
// with each layer collapsing away when it has a single child. Attachments
// the client gave no disposition are generated as "attachment" (an inline
// text or media part would otherwise be classified into the body views on
// read-back, RFC 8621 section 4.1.4's algorithm), except cid-referenced
// parts inside multipart/related, which default to "inline".

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// bodyProps collects the raw body-shaped properties of a creation object.
type bodyProps struct {
	structure, text, html, attachments, values json.RawMessage
}

// bodyValueIn is a client-submitted EmailBodyValue (RFC 8621 section
// 4.1.4). isEncodingProblem and isTruncated MUST be false or omitted on
// creation (section 4.6).
type bodyValueIn struct {
	Value             string `json:"value"`
	IsEncodingProblem bool   `json:"isEncodingProblem"`
	IsTruncated       bool   `json:"isTruncated"`
}

// Bounds on a creation object's part tree. The parser survives absurd
// trees; the generator has no reason to accept them.
const (
	genMaxParts = 100
	genMaxDepth = 10
)

// partIn is one validated EmailBodyPart of a creation object.
type partIn struct {
	path        string // JSON path, for error reporting
	partID      string
	blobID      jmap.Id
	typ         string
	charset     *string
	disposition *string
	cid         *string
	language    []string
	location    *string
	name        *string
	extra       []message.HeaderField
	extraFields []string // lowercase field names of extra
	subParts    []*partIn
}

// bodyPlanner carries the cross-part state of one body plan.
type bodyPlanner struct {
	values  map[string]bodyValueIn
	partIDs map[string]bool
	count   int
	open    blobOpener
	blobIDs map[jmap.Id]bool
}

// planBody validates the body properties and builds the outbound part
// tree. rootExtras is the lowercase field names of any client-declared
// headers on the part that became the root, whose headers share the
// message's top header block (checked against it by the caller).
func planBody(body bodyProps, open blobOpener) (root *message.OutPart, blobIds []jmap.Id, rootExtras []string, serr *jmap.SetError) {
	// 4.6: "If a bodyStructure property is given, there MUST NOT be
	// textBody, htmlBody, or attachments properties."
	if body.structure != nil && (body.text != nil || body.html != nil || body.attachments != nil) {
		props := []string{"bodyStructure"}
		for _, p := range [...]struct {
			name string
			raw  json.RawMessage
		}{{"textBody", body.text}, {"htmlBody", body.html}, {"attachments", body.attachments}} {
			if p.raw != nil {
				props = append(props, p.name)
			}
		}
		return nil, nil, nil, &jmap.SetError{
			Type: jmap.SetErrInvalidProperties, Properties: props,
			Description: "bodyStructure excludes the body-view properties",
		}
	}
	pl := &bodyPlanner{partIDs: map[string]bool{}, open: open, blobIDs: map[jmap.Id]bool{}}
	if serr := pl.parseValues(body.values); serr != nil {
		return nil, nil, nil, serr
	}
	if body.structure != nil {
		p, serr := pl.parsePart(body.structure, "bodyStructure", 0)
		if serr != nil {
			return nil, nil, nil, serr
		}
		return pl.build(p), pl.blobList(), p.extraFields, nil
	}
	root, rootExtras, serr = pl.convenienceRoot(body)
	if serr != nil {
		return nil, nil, nil, serr
	}
	return root, pl.blobList(), rootExtras, nil
}

// parseValues decodes the bodyValues map, strictly: unknown EmailBodyValue
// properties are rejected, and isEncodingProblem/isTruncated MUST be false
// or omitted (section 4.6).
func (pl *bodyPlanner) parseValues(raw json.RawMessage) *jmap.SetError {
	pl.values = map[string]bodyValueIn{}
	if raw == nil || isNullRaw(raw) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pl.values); err != nil {
		return invalidProp("bodyValues", "must be a map of partId to EmailBodyValue")
	}
	for id, v := range pl.values {
		if v.IsEncodingProblem || v.IsTruncated {
			return invalidProp("bodyValues/"+id, "isEncodingProblem and isTruncated must be false or omitted")
		}
	}
	return nil
}

// convenienceRoot builds the part tree from textBody/htmlBody/attachments.
func (pl *bodyPlanner) convenienceRoot(body bodyProps) (*message.OutPart, []string, *jmap.SetError) {
	text, serr := pl.parseSingle(body.text, "textBody", "text/plain")
	if serr != nil {
		return nil, nil, serr
	}
	html, serr := pl.parseSingle(body.html, "htmlBody", "text/html")
	if serr != nil {
		return nil, nil, serr
	}
	var atts []*partIn
	if body.attachments != nil && !isNullRaw(body.attachments) {
		var raws []json.RawMessage
		if err := json.Unmarshal(body.attachments, &raws); err != nil {
			return nil, nil, invalidProp("attachments", "must be an EmailBodyPart array")
		}
		for i, raw := range raws {
			p, serr := pl.parsePart(raw, "attachments/"+strconv.Itoa(i), 1)
			if serr != nil {
				return nil, nil, serr
			}
			if p.subParts != nil {
				return nil, nil, invalidProp(p.path, "an attachment cannot be multipart")
			}
			atts = append(atts, p)
		}
	}

	// The body core: the single part, or plain and html as alternatives
	// (plain first - parts are ordered from least to richest, RFC 2046
	// section 5.1.4).
	var core *message.OutPart
	var coreTyp string
	var coreExtras []string
	switch {
	case text != nil && html != nil:
		core = multipartOut("multipart/alternative", nil, []*message.OutPart{pl.build(text), pl.build(html)})
		coreTyp = "multipart/alternative"
	case text != nil:
		core, coreTyp, coreExtras = pl.build(text), text.typ, text.extraFields
	case html != nil:
		core, coreTyp, coreExtras = pl.build(html), html.typ, html.extraFields
	}

	// cid-referenced attachments join the html part in multipart/related
	// (RFC 2387); the rest go in multipart/mixed.
	var related, mixed []*partIn
	for _, a := range atts {
		if html != nil && a.cid != nil {
			if a.disposition == nil {
				a.disposition = strPtr("inline")
			}
			related = append(related, a)
			continue
		}
		if a.disposition == nil {
			a.disposition = strPtr("attachment")
		}
		mixed = append(mixed, a)
	}
	if len(related) > 0 {
		kids := []*message.OutPart{core}
		for _, a := range related {
			kids = append(kids, pl.build(a))
		}
		core = multipartOut("multipart/related", [][2]string{{"type", coreTyp}}, kids)
		coreExtras = nil
	}

	var parts []*message.OutPart
	var partExtras [][]string
	if core != nil {
		parts, partExtras = append(parts, core), append(partExtras, coreExtras)
	}
	for _, a := range mixed {
		parts, partExtras = append(parts, pl.build(a)), append(partExtras, a.extraFields)
	}
	switch len(parts) {
	case 0:
		// No body properties at all: an empty text/plain body.
		empty := &message.OutPart{
			Headers: []message.HeaderField{
				{Name: "Content-Type", Value: "text/plain; charset=utf-8"},
				{Name: "Content-Transfer-Encoding", Value: message.Enc7Bit},
			},
			Encoding: message.Enc7Bit,
			Content: func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("")), nil
			},
		}
		return empty, nil, nil
	case 1:
		return parts[0], partExtras[0], nil
	default:
		return multipartOut("multipart/mixed", nil, parts), nil, nil
	}
}

// parseSingle parses a textBody/htmlBody property: exactly one part of
// exactly the given type (section 4.6).
func (pl *bodyPlanner) parseSingle(raw json.RawMessage, prop, wantType string) (*partIn, *jmap.SetError) {
	if raw == nil || isNullRaw(raw) {
		return nil, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil || len(raws) != 1 {
		return nil, invalidProp(prop, "must contain exactly one body part of type "+wantType)
	}
	p, serr := pl.parsePart(raws[0], prop+"/0", 1)
	if serr != nil {
		return nil, serr
	}
	if p.typ != wantType {
		return nil, invalidProp(p.path, "must be of type "+wantType)
	}
	return p, nil
}

// parsePart validates one EmailBodyPart of a creation object, recursing
// into subParts.
func (pl *bodyPlanner) parsePart(raw json.RawMessage, path string, depth int) (*partIn, *jmap.SetError) {
	if pl.count++; pl.count > genMaxParts {
		return nil, invalidProp(path, "too many body parts")
	}
	if depth > genMaxDepth {
		return nil, invalidProp(path, "body parts nested too deeply")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, invalidProp(path, "not an EmailBodyPart object")
	}
	p := &partIn{path: path, typ: "text/plain"}
	var subRaw json.RawMessage
	var hasPartID, hasBlobID, hasSize bool
	extraFields := map[string][]string{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := m[key]
		kp := path + "/" + key
		switch key {
		case "partId":
			if s, ok := decodeString(v); ok {
				p.partID, hasPartID = s, true
			} else if !isNullRaw(v) {
				return nil, invalidProp(kp, "must be a string")
			}
		case "blobId":
			if s, ok := decodeString(v); ok {
				p.blobID, hasBlobID = jmap.Id(s), true
			} else if !isNullRaw(v) {
				return nil, invalidProp(kp, "must be an Id")
			}
		case "size":
			hasSize = !isNullRaw(v)
		case "headers":
			return nil, invalidProp(kp, "set each header field as an individual property")
		case "type", "charset", "disposition", "cid", "location", "name":
			s, ok := decodeString(v)
			if !ok {
				if isNullRaw(v) {
					continue
				}
				return nil, invalidProp(kp, "must be a string")
			}
			switch key {
			case "type":
				p.typ = strings.ToLower(s)
			case "charset":
				p.charset = &s
			case "disposition":
				p.disposition = &s
			case "cid":
				p.cid = &s
			case "location":
				p.location = &s
			case "name":
				p.name = &s
			}
		case "language":
			if isNullRaw(v) {
				continue
			}
			if err := json.Unmarshal(v, &p.language); err != nil {
				return nil, invalidProp(kp, "must be a string array")
			}
		case "subParts":
			if !isNullRaw(v) {
				subRaw = v
			}
		default:
			hp, ok := parseHeaderProp(key)
			if !ok {
				return nil, invalidProp(kp, "unknown property, or a form not allowed for this header field")
			}
			lf := strings.ToLower(hp.field)
			if lf == "content-transfer-encoding" {
				return nil, invalidProp(kp, "the server chooses the transfer encoding")
			}
			if strings.HasPrefix(lf, "content-") {
				// The Content-* fields the generator writes are derived from
				// the part's own properties; a raw override would fight the
				// framing (a client-declared boundary, most dangerously).
				return nil, invalidProp(kp, "use the EmailBodyPart properties for Content-* fields")
			}
			hf, serr := serializeHeader(headerSpec{prop: kp, field: hp.field, form: hp.form, all: hp.all, raw: v})
			if serr != nil {
				return nil, serr
			}
			extraFields[lf] = append(extraFields[lf], kp)
			p.extra = append(p.extra, hf...)
		}
	}
	// 4.6: no two properties may represent the same header field within
	// one EmailBodyPart.
	for _, props := range extraFields {
		if len(props) > 1 {
			sort.Strings(props)
			return nil, &jmap.SetError{
				Type: jmap.SetErrInvalidProperties, Properties: props,
				Description: "multiple properties represent the same header field",
			}
		}
	}
	for f := range extraFields {
		p.extraFields = append(p.extraFields, f)
	}
	sort.Strings(p.extraFields)

	if !isMediaType(p.typ) {
		return nil, invalidProp(path+"/type", "not a media type")
	}
	for _, check := range [...]struct {
		val  *string
		prop string
	}{{p.charset, "charset"}, {p.cid, "cid"}, {p.location, "location"}} {
		if check.val != nil && (!isTokenSafe(*check.val) || strings.ContainsAny(*check.val, "<>\"")) {
			return nil, invalidProp(path+"/"+check.prop, "invalid value")
		}
	}
	for _, tag := range p.language {
		if !isTokenSafe(tag) || strings.ContainsAny(tag, ",;\"") {
			return nil, invalidProp(path+"/language", "invalid language tag")
		}
	}
	if p.disposition != nil && !isTokenSafe(*p.disposition) {
		return nil, invalidProp(path+"/disposition", "invalid value")
	}
	if p.name != nil && hasCtl(*p.name) {
		return nil, invalidProp(path+"/name", "invalid value")
	}

	if strings.HasPrefix(p.typ, "multipart/") {
		if hasPartID || hasBlobID || p.charset != nil {
			return nil, invalidProp(path, "a multipart part carries no content of its own")
		}
		if subRaw == nil {
			return nil, invalidProp(path+"/subParts", "a multipart part requires subParts")
		}
		var raws []json.RawMessage
		if err := json.Unmarshal(subRaw, &raws); err != nil || len(raws) == 0 {
			return nil, invalidProp(path+"/subParts", "must be a non-empty EmailBodyPart array")
		}
		p.subParts = make([]*partIn, 0, len(raws))
		for i, sub := range raws {
			sp, serr := pl.parsePart(sub, path+"/subParts/"+strconv.Itoa(i), depth+1)
			if serr != nil {
				return nil, serr
			}
			p.subParts = append(p.subParts, sp)
		}
		return p, nil
	}

	if subRaw != nil {
		return nil, invalidProp(path+"/subParts", "only multipart/* parts have subParts")
	}
	// 4.6: a partId or a blobId, but not both.
	if hasPartID == hasBlobID {
		return nil, invalidProp(path, "a body part takes a partId or a blobId, but not both")
	}
	if hasPartID {
		// 4.6: charset and size MUST be omitted when a partId is given.
		if p.charset != nil {
			return nil, invalidProp(path+"/charset", "the server chooses the encoding for a partId part")
		}
		if hasSize {
			return nil, invalidProp(path+"/size", "size must be omitted when a partId is given")
		}
		if !strings.HasPrefix(p.typ, "text/") {
			return nil, invalidProp(path+"/type", "a partId part must be text/*")
		}
		if _, ok := pl.values[p.partID]; !ok {
			return nil, invalidProp(path+"/partId", "partId not present in bodyValues")
		}
		if pl.partIDs[p.partID] {
			return nil, invalidProp(path+"/partId", "partId used by more than one part")
		}
		pl.partIDs[p.partID] = true
	} else {
		pl.blobIDs[p.blobID] = true
	}
	return p, nil
}

// build turns a validated partIn into its OutPart, assembling the
// Content-* headers its properties imply.
func (pl *bodyPlanner) build(p *partIn) *message.OutPart {
	if p.subParts != nil {
		kids := make([]*message.OutPart, 0, len(p.subParts))
		for _, sp := range p.subParts {
			kids = append(kids, pl.build(sp))
		}
		out := multipartOut(p.typ, nil, kids)
		out.Headers = append(out.Headers, pl.commonHeaders(p)...)
		return out
	}
	var ctParams [][2]string
	out := &message.OutPart{}
	if p.partID != "" {
		value := pl.values[p.partID].Value
		out.Encoding = chooseTextEncoding(value)
		prepped := crlfNormalize(value)
		out.Content = func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(prepped)), nil
		}
		// The server may choose any encoding for a partId part (4.6);
		// bodyValues are Unicode strings, so utf-8 always fits.
		ctParams = append(ctParams, [2]string{"charset", "utf-8"})
	} else {
		blobID := p.blobID
		out.Content = func(ctx context.Context) (io.ReadCloser, error) {
			return pl.open(ctx, blobID)
		}
		// A blob is streamed, never scanned, so its encoding cannot be
		// chosen from content: base64 always fits. The exception is
		// message/*, for which RFC 2046 section 5.2.1 forbids base64 -
		// those stream unencoded as 8bit.
		if strings.HasPrefix(p.typ, "message/") {
			out.Encoding = message.Enc8Bit
		} else {
			out.Encoding = message.EncBase64
		}
		if p.charset != nil {
			ctParams = append(ctParams, [2]string{"charset", *p.charset})
		}
	}
	ct := p.typ
	for _, param := range ctParams {
		ct += "; " + param[0] + "=" + quoteParamValue(param[1])
	}
	if p.name != nil && p.disposition == nil {
		// A name with no disposition maps to the legacy Content-Type name
		// parameter (the read side's fallback, RFC 8621 section 4.1.4).
		ct += "; " + filenameParam("name", *p.name)
	}
	out.Headers = append(out.Headers,
		message.HeaderField{Name: "Content-Type", Value: ct},
		message.HeaderField{Name: "Content-Transfer-Encoding", Value: out.Encoding},
	)
	if p.disposition != nil {
		cd := *p.disposition
		if p.name != nil {
			cd += "; " + filenameParam("filename", *p.name)
		}
		out.Headers = append(out.Headers, message.HeaderField{Name: "Content-Disposition", Value: cd})
	}
	out.Headers = append(out.Headers, pl.commonHeaders(p)...)
	return out
}

// commonHeaders assembles the Content-* headers legal on any part, plus
// the part's client-declared extra headers.
func (pl *bodyPlanner) commonHeaders(p *partIn) []message.HeaderField {
	var hs []message.HeaderField
	if p.cid != nil {
		hs = append(hs, message.HeaderField{Name: "Content-ID", Value: "<" + *p.cid + ">"})
	}
	if len(p.language) > 0 {
		hs = append(hs, message.HeaderField{Name: "Content-Language", Value: strings.Join(p.language, ", ")})
	}
	if p.location != nil {
		hs = append(hs, message.HeaderField{Name: "Content-Location", Value: *p.location})
	}
	return append(hs, p.extra...)
}

// multipartOut builds a multipart node with a fresh boundary.
func multipartOut(typ string, params [][2]string, kids []*message.OutPart) *message.OutPart {
	b := message.NewBoundary()
	ct := typ + `; boundary="` + b + `"`
	for _, param := range params {
		ct += "; " + param[0] + "=" + quoteParamValue(param[1])
	}
	return &message.OutPart{
		Headers:  []message.HeaderField{{Name: "Content-Type", Value: ct}},
		Boundary: b,
		SubParts: kids,
	}
}

// blobList returns the referenced blob ids, deduplicated and sorted.
func (pl *bodyPlanner) blobList() []jmap.Id {
	ids := make([]jmap.Id, 0, len(pl.blobIDs))
	for id := range pl.blobIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// chooseTextEncoding picks the Content-Transfer-Encoding for a bodyValues
// string: 7bit when it already fits (ASCII, short lines), base64 when it
// is mostly non-ASCII, quoted-printable otherwise (RFC 2045 section 6).
func chooseTextEncoding(s string) string {
	nonASCII, col := 0, 0
	longLine := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			col = 0
			continue
		}
		if col++; col > 78 {
			longLine = true
		}
		if c := s[i]; c > 0x7e || (c < 0x20 && c != '\t' && c != '\r') {
			nonASCII++
		}
	}
	switch {
	case nonASCII == 0 && !longLine:
		return message.Enc7Bit
	case nonASCII*3 > len(s):
		return message.EncBase64
	default:
		return message.EncQP
	}
}

// crlfNormalize converts a bodyValues string (LF line endings, RFC 8621
// section 4.1.4) to the CRLF lines of a message body.
func crlfNormalize(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// isMediaType reports whether s looks like a type/subtype pair safe to
// write into a Content-Type header.
func isMediaType(s string) bool {
	slash := strings.IndexByte(s, '/')
	if slash <= 0 || slash == len(s)-1 || strings.Count(s, "/") != 1 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c > 0x7e || strings.IndexByte("()<>@,;:\\\"[]?=", c) >= 0 {
			return false
		}
	}
	return true
}

// tokenChars reports whether s is a non-empty RFC 2045 token, usable as a
// bare parameter value.
func tokenChars(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c > 0x7e || strings.IndexByte("()<>@,;:\\\"/[]?=", c) >= 0 {
			return false
		}
	}
	return true
}

// quoteParamValue writes a Content-Type/Content-Disposition parameter
// value: a token bare, anything else as a quoted-string.
func quoteParamValue(v string) string {
	if tokenChars(v) {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		if v[i] == '"' || v[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('"')
	return b.String()
}

// filenameParam serializes a filename-carrying parameter: plain ASCII as
// a quoted-string under the given name, anything else in the RFC 2231
// extended form (name*=utf-8”...), which the read side decodes.
func filenameParam(name, value string) string {
	plain := true
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			plain = false
			break
		}
	}
	if plain {
		return name + "=" + quoteParamValue(value)
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteString("*=utf-8''")
	const attrChars = "!#$&+-.^_`|~"
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(attrChars, c) >= 0:
			b.WriteByte(c)
		default:
			const hexDigits = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		}
	}
	return b.String()
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }
