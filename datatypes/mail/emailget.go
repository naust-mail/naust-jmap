package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// emailGetArgNames are the extra Email/get arguments (RFC 8621 section
// 4.2) beyond the standard /get set.
var emailGetArgNames = []string{
	"bodyProperties", "fetchTextBodyValues", "fetchHTMLBodyValues",
	"fetchAllBodyValues", "maxBodyValueBytes",
}

// defaultBodyProperties is the EmailBodyPart property set returned when
// bodyProperties is omitted (RFC 8621 section 4.2). Note that headers and
// subParts are not in it: a client wanting the full tree must ask.
var defaultBodyProperties = []string{
	"partId", "blobId", "size", "name", "type", "charset",
	"disposition", "cid", "language", "location",
}

// maxBodyProperties bounds how many EmailBodyPart properties one
// Email/get or Email/parse may request. The set is a small fixed
// vocabulary plus header:{name} forms; each is evaluated for every body
// part of every returned Email, so an unbounded list is a CPU amplifier
// with no legitimate use. The cap is generous enough that real clients
// never meet it.
const maxBodyProperties = 256

// bodyPartProps is the set of valid non-header EmailBodyPart property
// names (section 4.1.4), for validating bodyProperties.
var bodyPartProps = map[string]bool{
	"partId": true, "blobId": true, "size": true, "headers": true,
	"name": true, "type": true, "charset": true, "disposition": true,
	"cid": true, "language": true, "location": true, "subParts": true,
}

// checkEmailGetArgs validates the extra Email/get arguments; any problem
// is invalidArguments (section 3.6.2). Every bodyProperties entry must be
// a known EmailBodyPart property or a valid header:{name} form.
func checkEmailGetArgs(extra map[string]json.RawMessage) error {
	for _, name := range []string{"fetchTextBodyValues", "fetchHTMLBodyValues", "fetchAllBodyValues"} {
		if raw, ok := extra[name]; ok && !isNullRaw(raw) {
			var b bool
			if err := json.Unmarshal(raw, &b); err != nil {
				return fmt.Errorf("%s must be a boolean", name)
			}
		}
	}
	if raw, ok := extra["maxBodyValueBytes"]; ok && !isNullRaw(raw) {
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil || !jmap.ValidUnsignedInt(n) {
			return fmt.Errorf("maxBodyValueBytes must be an UnsignedInt")
		}
	}
	if raw, ok := extra["bodyProperties"]; ok && !isNullRaw(raw) {
		var props []string
		if err := json.Unmarshal(raw, &props); err != nil {
			return fmt.Errorf("bodyProperties must be an array of strings")
		}
		if len(props) > maxBodyProperties {
			return fmt.Errorf("bodyProperties has more than %d entries", maxBodyProperties)
		}
		for _, p := range props {
			if bodyPartProps[p] {
				continue
			}
			if _, ok := parseHeaderProp(p); ok {
				continue
			}
			return fmt.Errorf("bodyProperties includes unknown property %q", p)
		}
	}
	return nil
}

// emailComputed resolves the Email properties that are not stored on the
// record: headers, the body-part views, bodyValues, and the header:{name}
// forms. All are derived by parsing the message blob on demand (decision
// 2A: parse the fast list at delivery, everything else lazily).
type emailComputed struct {
	store blob.Store
}

// computedFixed are the non-stored Email properties with fixed names.
var computedFixed = map[string]bool{
	"headers": true, "bodyStructure": true, "bodyValues": true,
	"textBody": true, "htmlBody": true, "attachments": true,
}

func (c *emailComputed) Accepts(name string) bool {
	if computedFixed[name] {
		return true
	}
	_, ok := parseHeaderProp(name)
	return ok
}

// getArgs is the decoded extra Email/get arguments.
type getArgs struct {
	bodyProperties    []string
	fetchTextBody     bool
	fetchHTMLBody     bool
	fetchAllBody      bool
	maxBodyValueBytes int64
}

func decodeGetArgs(extra map[string]json.RawMessage) getArgs {
	a := getArgs{bodyProperties: defaultBodyProperties}
	if raw, ok := extra["bodyProperties"]; ok && !isNullRaw(raw) {
		var props []string
		if json.Unmarshal(raw, &props) == nil {
			a.bodyProperties = props
		}
	}
	json.Unmarshal(rawOr(extra["fetchTextBodyValues"]), &a.fetchTextBody)
	json.Unmarshal(rawOr(extra["fetchHTMLBodyValues"]), &a.fetchHTMLBody)
	json.Unmarshal(rawOr(extra["fetchAllBodyValues"]), &a.fetchAllBody)
	json.Unmarshal(rawOr(extra["maxBodyValueBytes"]), &a.maxBodyValueBytes)
	return a
}

func rawOr(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

func (c *emailComputed) Resolve(ctx context.Context, acct jmap.Id, stored objectdb.Object, names []string, extra map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	args := decodeGetArgs(extra)
	plan := compileBodyProps(args.bodyProperties)
	msg, err := c.parseBlob(ctx, acct, stored, getCapture(names, plan, args))
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(names))
	for _, name := range names {
		switch name {
		case "headers":
			out[name] = mustJSON(headerList(msg.msg.Headers))
		case "bodyStructure":
			out[name] = mustJSON(bodyPartJSON(msg.msg.Root, plan))
		case "textBody":
			out[name] = mustJSON(bodyPartArray(msg.textBody, plan))
		case "htmlBody":
			out[name] = mustJSON(bodyPartArray(msg.htmlBody, plan))
		case "attachments":
			out[name] = mustJSON(bodyPartArray(msg.attachments, plan))
		case "bodyValues":
			out[name] = mustJSON(bodyValues(msg, args))
		default:
			hp, ok := parseHeaderProp(name)
			if !ok {
				continue
			}
			out[name] = hp.resolve(msg.msg.Headers)
		}
	}
	return out, nil
}

// getCapture is the content this Email/get must collect while parsing: the
// per-part identity behind blobId and size, needed only when a body-part view
// is rendered with one of those properties (section 4.1.4), and the text of the
// text/* parts, needed only when the request asks for bodyValues (section 4.2).
// A request for headers alone therefore decodes no body content at all.
func getCapture(names []string, plan bodyPropPlan, args getArgs) *capture {
	c := newCapture()
	c.maxValueBytes = args.maxBodyValueBytes
	for _, name := range names {
		switch name {
		case "bodyValues":
			// bodyValues holds only the parts the fetch flags select (section 4.2);
			// with none set it is an empty object and no text need be captured.
			c.values = args.fetchTextBody || args.fetchHTMLBody || args.fetchAllBody
		case "bodyStructure", "textBody", "htmlBody", "attachments":
			c.identity = c.identity || wantsIdentity(plan)
		}
	}
	return c
}

// wantsIdentity reports whether a body-part rendering needs each leaf's content
// identity: blobId is its content address and size its decoded octet count
// (section 4.1.4), and both come from hashing and counting the decoded content.
// The other EmailBodyPart properties are pure metadata.
func wantsIdentity(plan bodyPropPlan) bool {
	for _, prop := range plan.standard {
		if prop == "blobId" || prop == "size" {
			return true
		}
	}
	return false
}

// parseBlob opens the message blob and parses it, collecting the content the
// capture declares. A missing blob is a serverFail: a stored Email must always
// have its blob.
func (c *emailComputed) parseBlob(ctx context.Context, acct jmap.Id, stored objectdb.Object, want *capture) (*parsed, error) {
	var blobID jmap.Id
	if err := json.Unmarshal(stored["blobId"], &blobID); err != nil {
		return nil, fmt.Errorf("mail: Email record has no blobId: %w", err)
	}
	rc, _, err := c.store.Open(ctx, acct, blobID)
	if err != nil {
		return nil, fmt.Errorf("mail: opening message blob %s: %w", blobID, err)
	}
	defer rc.Close()
	return parseMessage(rc, want)
}

// bodyPropPlan is the compiled bodyProperties selection. Compiling once
// per Email - rather than re-parsing each header:{name} form for every
// body part - keeps rendering linear in (parts x distinct properties)
// instead of paying a property-name parse per part. Duplicate names
// collapse to one entry.
type bodyPropPlan struct {
	standard    []string             // known EmailBodyPart props, deduped
	headerProps []compiledHeaderProp // header:{name} forms, parsed once
}

// compiledHeaderProp pairs a requested header property name (the JSON key
// to emit) with its already-parsed form.
type compiledHeaderProp struct {
	name string
	hp   headerProp
}

// compileBodyProps parses and de-duplicates a bodyProperties list once.
// Unknown names are dropped here; checkEmailGetArgs has already rejected
// the request if any were present.
func compileBodyProps(props []string) bodyPropPlan {
	var plan bodyPropPlan
	seen := make(map[string]bool, len(props))
	for _, name := range props {
		if seen[name] {
			continue
		}
		seen[name] = true
		if bodyPartProps[name] {
			plan.standard = append(plan.standard, name)
			continue
		}
		if hp, ok := parseHeaderProp(name); ok {
			plan.headerProps = append(plan.headerProps, compiledHeaderProp{name: name, hp: hp})
		}
	}
	return plan
}

// bodyPartArray serializes a flat list of parts (textBody/htmlBody/
// attachments).
func bodyPartArray(parts []*message.Part, plan bodyPropPlan) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		out = append(out, mustJSON(bodyPartJSON(p, plan)))
	}
	return out
}

// bodyPartJSON serializes one EmailBodyPart, including only the requested
// properties (section 4.1.4). subParts, when requested, recurses with the
// same property selection.
func bodyPartJSON(p *message.Part, plan bodyPropPlan) map[string]json.RawMessage {
	if p == nil {
		return nil
	}
	multipart := p.SubParts != nil
	obj := make(map[string]json.RawMessage, len(plan.standard)+len(plan.headerProps))
	for _, name := range plan.standard {
		switch name {
		case "partId":
			obj[name] = strOrNull(nonEmpty(p.PartID))
		case "blobId":
			if multipart {
				obj[name] = jsonNull
			} else {
				// The blobId is the content address of the decoded part (RFC 8620
				// section 6.1); the parse computed its digest as the content
				// streamed past, so nothing is rehashed here.
				obj[name] = mustJSON(blob.IdFromDigest(p.Digest))
			}
		case "size":
			obj[name] = mustJSON(p.Size)
		case "name":
			obj[name] = ptrStrOrNull(p.Name)
		case "type":
			obj[name] = mustJSON(p.Type)
		case "charset":
			obj[name] = ptrStrOrNull(p.Charset)
		case "disposition":
			obj[name] = ptrStrOrNull(p.Disposition)
		case "cid":
			obj[name] = ptrStrOrNull(p.Cid)
		case "language":
			if len(p.Language) == 0 {
				obj[name] = jsonNull
			} else {
				obj[name] = mustJSON(p.Language)
			}
		case "location":
			obj[name] = ptrStrOrNull(p.Location)
		case "headers":
			obj[name] = mustJSON(headerList(p.Headers))
		case "subParts":
			if multipart {
				obj[name] = mustJSON(bodyPartArray(p.SubParts, plan))
			} else {
				obj[name] = jsonNull
			}
		}
	}
	for _, chp := range plan.headerProps {
		obj[chp.name] = chp.hp.resolve(p.Headers)
	}
	return obj
}

// bodyValue is one EmailBodyValue (section 4.1.4).
type bodyValue struct {
	Value             string `json:"value"`
	IsEncodingProblem bool   `json:"isEncodingProblem"`
	IsTruncated       bool   `json:"isTruncated"`
}

// bodyValues builds the bodyValues map for the text/* parts selected by
// the fetch arguments (section 4.2). The values themselves were captured by the
// parse: a value sink charset decoded each text part's content as it streamed
// and truncated it to maxBodyValueBytes, so nothing is decoded again here and no
// whole part is held. isEncodingProblem merges the two stages the flag covers
// (4.1.4): an unknown or broken Content-Transfer-Encoding, found by the parser,
// and a malformed or unknown charset, found by the sink.
func bodyValues(msg *parsed, args getArgs) map[string]bodyValue {
	selected := map[string]*message.Part{}
	add := func(parts []*message.Part) {
		for _, p := range parts {
			if p.PartID != "" && strings.HasPrefix(p.Type, "text/") {
				selected[p.PartID] = p
			}
		}
	}
	if args.fetchTextBody {
		add(msg.textBody)
	}
	if args.fetchHTMLBody {
		add(msg.htmlBody)
	}
	if args.fetchAllBody {
		add(allTextParts(msg.msg.Root))
	}
	out := make(map[string]bodyValue, len(selected))
	for id, p := range selected {
		s, ok := msg.cap.valueSinks[p]
		if !ok {
			continue // no value captured for this part: the capture did not ask
		}
		out[id] = bodyValue{
			Value:             s.value,
			IsEncodingProblem: s.problem || p.EncodingProblem,
			IsTruncated:       s.truncated,
		}
	}
	return out
}

// allTextParts collects every text/* leaf in the tree, depth first.
func allTextParts(root *message.Part) []*message.Part {
	var out []*message.Part
	var walk func(p *message.Part)
	walk = func(p *message.Part) {
		if p == nil {
			return
		}
		if p.SubParts != nil {
			for _, c := range p.SubParts {
				walk(c)
			}
			return
		}
		if strings.HasPrefix(p.Type, "text/") {
			out = append(out, p)
		}
	}
	walk(root)
	return out
}

// headerList serializes header fields as EmailHeader[] (section 4.1.3).
func headerList(headers []message.HeaderField) []message.HeaderField {
	if headers == nil {
		return []message.HeaderField{}
	}
	return headers
}

var jsonNull = json.RawMessage("null")

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		// Values here are plain data structures; a marshal failure is a
		// programming error, not a runtime condition.
		panic(fmt.Sprintf("mail: marshalling computed value: %v", err))
	}
	return raw
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func strOrNull(s *string) json.RawMessage {
	if s == nil {
		return jsonNull
	}
	return mustJSON(*s)
}

func ptrStrOrNull(s *string) json.RawMessage { return strOrNull(s) }
