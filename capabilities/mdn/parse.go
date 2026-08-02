package mdn

// MDN/parse (RFC 9007 section 2.2): parse blobs as received MDN
// messages. Each blob is streamed through the report package's strict
// MDN reader; a blob the account cannot reach is notFound, one that is
// not a recognizable MDN is notParsable, and a recognized one is
// rendered as the full MDN object, with forEmailId resolved through
// the submission queue's Message-ID index when the correlation is
// unambiguous.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

type mdnParse struct {
	db    *objectdb.DB
	store blob.Store
	core  jmap.CoreCapabilities
	queue *submit.Queue
}

// parseArgNames is the full set of accepted MDN/parse arguments (section
// 2.2); any other argument is invalidArguments.
var parseArgNames = map[string]bool{"accountId": true, "blobIds": true}

// parseResponse is the MDN/parse response (section 2.2). parsed is
// Id[MDN]|null keyed by blob id; notParsable and notFound are
// Id[]|null. A nil map or slice marshals to null.
type parseResponse struct {
	AccountID   jmap.Id          `json:"accountId"`
	Parsed      map[jmap.Id]*MDN `json:"parsed"`
	NotParsable []jmap.Id        `json:"notParsable"`
	NotFound    []jmap.Id        `json:"notFound"`
}

func (h mdnParse) Handle(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(call.Args, &all); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	for name := range all {
		if !parseArgNames[name] {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, fmt.Sprintf("unknown argument %q", name))
		}
	}
	var accountID jmap.Id
	json.Unmarshal(all["accountId"], &accountID)
	if errType, desc := runtime.CheckAccount(call, accountID, false); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	var blobIds []jmap.Id
	if raw, ok := all["blobIds"]; ok {
		if err := json.Unmarshal(raw, &blobIds); err != nil {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "blobIds must be an array of ids")
		}
	}
	// The same per-call object cap every get-shaped method enforces
	// (RFC 8620 section 3.8; section 2.2 names requestTooLarge for it).
	if int64(len(blobIds)) > h.core.MaxObjectsInGet {
		return runtime.Fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}

	resp := parseResponse{AccountID: accountID}
	seen := make(map[jmap.Id]bool, len(blobIds))
	for _, id := range blobIds {
		// A repeated id would land in the same response slot with the
		// same verdict, so the work for it is done once.
		if seen[id] {
			continue
		}
		seen[id] = true
		// The blobIds are client-supplied, so each open goes through the
		// checked path: an id that is invalid, unknown to the account, or
		// unreferenced and uploaded by someone else (RFC 8620 section
		// 6.1) is notFound, indistinguishable from an absent blob.
		rc, _, err := runtime.OpenBlob(ctx, h.db, h.store, accountID, id, call.Identity)
		if errors.Is(err, blob.ErrNotFound) {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		if err != nil {
			return internalFail(call.CallID, "opening blob", err)
		}
		parsed, err := report.ParseMDN(rc)
		rc.Close()
		if errors.Is(err, report.ErrNotMDN) {
			resp.NotParsable = append(resp.NotParsable, id)
			continue
		}
		if err != nil {
			return internalFail(call.CallID, "reading blob", err)
		}
		if resp.Parsed == nil {
			resp.Parsed = make(map[jmap.Id]*MDN)
		}
		resp.Parsed[id] = h.wireMDN(ctx, accountID, parsed)
	}
	return runtime.Reply("MDN/parse", call.CallID, resp)
}

// wireMDN renders one parsed MDN as the RFC 9007 section 2 object.
// String properties the notification omits are null, not empty strings;
// the sample forms are kept - finalRecipient and originalRecipient with
// their address-type prefixes, originalMessageId as the full Message-ID
// header value with angle brackets.
func (h mdnParse) wireMDN(ctx context.Context, accountID jmap.Id, p *report.ParsedMDN) *MDN {
	m := &MDN{
		Subject:     strPtr(p.Subject),
		TextBody:    strPtr(p.TextBody),
		ReportingUA: strPtr(p.ReportingUA),
		Disposition: &Disposition{
			ActionMode:  p.Disposition.ActionMode,
			SendingMode: p.Disposition.SendingMode,
			Type:        p.Disposition.Type,
		},
		MDNGateway:        strPtr(p.MDNGateway),
		OriginalRecipient: strPtr(p.OriginalRecipient),
		FinalRecipient:    strPtr(p.FinalRecipient),
		Error:             p.Errors,
		// The original message appeared as the third report component,
		// which is what includeOriginalMessage asks for on the send side
		// (section 2).
		IncludeOriginalMessage: p.HasOriginal,
	}
	// The bracketed form is emitted only for an id that can wear it: an
	// extracted id carrying a bracket of its own would produce a
	// malformed Message-ID form, so it is passed through bare.
	if p.OriginalMessageID != "" {
		if strings.ContainsAny(p.OriginalMessageID, "<>") {
			m.OriginalMessageID = strPtr(p.OriginalMessageID)
		} else {
			m.OriginalMessageID = strPtr("<" + p.OriginalMessageID + ">")
		}
		// forEmailId resolves through the submission Message-ID index
		// only (section 2.2): the key is the parsed, bounded
		// Original-Message-ID, never a client-supplied string. A missing
		// or ambiguous match leaves it null, as the section permits -
		// and so does a lookup error: null is the answer the section
		// blesses for "could not efficiently determine", which an index
		// fault is.
		if emailID, ok, err := h.queue.EmailIDForMessageID(ctx, accountID, p.OriginalMessageID); err == nil && ok {
			m.ForEmailID = &emailID
		}
	}
	// Extension field names become JSON object member names, so only
	// names in the RFC 5322 field-name grammar pass (arbitrary bytes
	// could collide or malform the object); the first occurrence of a
	// name wins, matching how every standard field is read.
	for _, f := range p.ExtensionFields {
		if !fieldNameOK(f.Name) {
			continue
		}
		if _, dup := m.ExtensionFields[f.Name]; dup {
			continue
		}
		if m.ExtensionFields == nil {
			m.ExtensionFields = make(map[string]string, len(p.ExtensionFields))
		}
		m.ExtensionFields[f.Name] = f.Value
	}
	return m
}

// fieldNameOK reports whether name is an RFC 5322 section 3.6.8
// field-name: printable US-ASCII with no colon.
func fieldNameOK(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] < 33 || name[i] > 126 || name[i] == ':' {
			return false
		}
	}
	return name != ""
}

// strPtr maps the report vocabulary's "" (field absent) to a JSON null.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
