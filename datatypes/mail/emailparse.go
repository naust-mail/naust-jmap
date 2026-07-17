package mail

// Email/parse (RFC 8621 section 4.9): render already-uploaded blobs as Email
// objects without storing them, so a client can display an attached message
// without importing it as a top-level Email. Parse is a custom method: it
// produces Email representations that have no id and are never persisted.
// The store-only metadata (id, mailboxIds, keywords, receivedAt) is null;
// threadId is null too (the MAY-calculate is not attempted). Everything else
// is built from the parsed message with the same builders Email/get uses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// emailParse handles Email/parse. db holds the upload records that gate
// access to the blobs in store; core bounds the number parsed in one call.
type emailParse struct {
	db    *objectdb.DB
	store blob.Store
	core  jmap.CoreCapabilities
}

// parseArgNames is the full set of accepted Email/parse arguments (section
// 4.9); any other argument is invalidArguments.
var parseArgNames = map[string]bool{
	"accountId": true, "blobIds": true, "properties": true,
	"bodyProperties":      true,
	"fetchTextBodyValues": true, "fetchHTMLBodyValues": true,
	"fetchAllBodyValues": true, "maxBodyValueBytes": true,
}

// maxParseProperties bounds the client-supplied properties list for one
// Email/parse, matching the Foo/get cap: each name is resolved per blob,
// and computed header:{name} forms are open-ended, so an unbounded list is
// a CPU amplifier with no legitimate use.
const maxParseProperties = 512

// emailParseDefaultProperties is the section 4.9 default property list used
// when properties is omitted. Note it is body-oriented and omits the
// metadata that parse always returns null.
var emailParseDefaultProperties = []string{
	"messageId", "inReplyTo", "references", "sender", "from", "to", "cc",
	"bcc", "replyTo", "subject", "sentAt", "hasAttachment", "preview",
	"bodyValues", "textBody", "htmlBody", "attachments",
}

// parseResponse is the Email/parse response (section 4.9). parsed is
// Id[Email]|null; notParsable and notFound are Id[]|null - a nil map or
// slice marshals to null.
type parseResponse struct {
	AccountId   jmap.Id                                `json:"accountId"`
	Parsed      map[jmap.Id]map[string]json.RawMessage `json:"parsed"`
	NotParsable []jmap.Id                              `json:"notParsable"`
	NotFound    []jmap.Id                              `json:"notFound"`
}

func (h emailParse) handle(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(call.Args, &all); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	for name := range all {
		if !parseArgNames[name] {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, fmt.Sprintf("unknown argument %q", name))
		}
	}
	var accountId jmap.Id
	json.Unmarshal(all["accountId"], &accountId)
	if errType, desc := runtime.CheckAccount(call, accountId, false); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	var blobIds []jmap.Id
	if raw, ok := all["blobIds"]; ok && !isNullRaw(raw) {
		if err := json.Unmarshal(raw, &blobIds); err != nil {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "blobIds must be an array of ids")
		}
	}
	if int64(len(blobIds)) > h.core.MaxObjectsInGet {
		return runtime.Fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}

	// The requested property list and the body arguments are validated
	// once: an unknown property or a header form used on an inappropriate
	// field (e.g. header:From:asDate) rejects the whole call (section 4.9).
	props := emailParseDefaultProperties
	if raw, ok := all["properties"]; ok && !isNullRaw(raw) {
		// Decode into a fresh slice: unmarshalling into props would reuse the
		// package-level default's backing array and corrupt it for later calls.
		var p []string
		if err := json.Unmarshal(raw, &p); err != nil {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "properties must be an array of strings")
		}
		if len(p) > maxParseProperties {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, fmt.Sprintf("more than %d properties requested", maxParseProperties))
		}
		props = p
		for _, name := range props {
			if !parseAcceptsProperty(name) {
				return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, fmt.Sprintf("unknown or inappropriate property %q", name))
			}
		}
	}
	if err := checkEmailGetArgs(all); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	args := decodeGetArgs(all)

	resp := parseResponse{AccountId: accountId}
	for _, id := range blobIds {
		// The blobIds are client-supplied, so each open goes through the
		// checked path: an id that is invalid, unknown to the account, or
		// unreferenced and uploaded by someone else (RFC 8620 section 6.1)
		// is notFound, indistinguishable from an absent blob.
		rc, size, err := runtime.OpenBlob(ctx, h.db, h.store, accountId, id, call.Identity)
		if errors.Is(err, blob.ErrNotFound) {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		if err != nil {
			return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		msg, err := parseMessage(rc, parseCapture(props, compileBodyProps(args.bodyProperties), args))
		rc.Close()
		if err != nil {
			return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		if resp.Parsed == nil {
			resp.Parsed = make(map[jmap.Id]map[string]json.RawMessage)
		}
		resp.Parsed[id] = parseEmail(msg, id, uint64(size), props, args)
	}
	return runtime.Reply("Email/parse", call.CallID, resp)
}

// parseAcceptsProperty reports whether name is a valid Email/parse property:
// id, a stored/fast Email property, a computed body view, or an appropriate
// header:{name} form (parseHeaderProp rejects inappropriate forms such as
// header:From:asDate).
func parseAcceptsProperty(name string) bool {
	if name == "id" || computedFixed[name] {
		return true
	}
	if _, declared := EmailType().Properties[name]; declared {
		return true
	}
	_, ok := parseHeaderProp(name)
	return ok
}

// parseCapture is the content Email/parse must collect: what Email/get would
// collect for the same body arguments, plus the preview - Email/parse renders
// the same stored/fast fields buildEmailRecord produces (section 4.9), and the
// preview is one of them (section 4.1.4).
func parseCapture(props []string, plan bodyPropPlan, args getArgs) *capture {
	c := getCapture(props, plan, args)
	c.preview = true
	return c
}

// parseEmail assembles the Email object for one parsed message, limited to
// the requested properties (section 4.9). It reuses buildEmailRecord for the
// stored/fast fields, forces the store-only metadata and threadId to null,
// and computes the body views on demand from the message.
func parseEmail(msg *parsed, blobID jmap.Id, size uint64, props []string, args getArgs) map[string]json.RawMessage {
	rec := buildEmailRecord(msg, emailMeta{BlobID: blobID, Size: size})
	// A parsed Email is not stored: it has no id, mailbox membership,
	// keywords, or received date, and its Thread is not calculated (4.9).
	rec["id"] = jsonNull
	rec["mailboxIds"] = jsonNull
	rec["keywords"] = jsonNull
	rec["receivedAt"] = jsonNull
	rec["threadId"] = jsonNull

	plan := compileBodyProps(args.bodyProperties)
	out := make(map[string]json.RawMessage, len(props))
	for _, name := range props {
		if v, ok := rec[name]; ok {
			out[name] = v
			continue
		}
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
			if hp, ok := parseHeaderProp(name); ok {
				out[name] = hp.resolve(msg.msg.Headers)
			}
		}
	}
	return out
}
