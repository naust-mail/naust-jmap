package mail

// Text search: the Searcher socket and SearchSnippet/get (RFC 8621 section
// 5). The built-in substring implementation of Searcher lives in the search
// sub-package (datatypes/mail/search): it must not import this package, so
// that a host wanting real relevance can plug an index-backed Searcher
// (RegisterEmail's searcher argument) without the built-in one entering its
// build.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailmethods"
)

// Searcher answers the text-search parts of Email that the structural query
// planner cannot: the RFC 8621 section 4.4.1 text FilterConditions and the
// section 5 search snippets. Both take a loaded Email record and may read its
// message blob.
type Searcher interface {
	// Match reports whether the Email matches one text FilterCondition
	// (RFC 8621 section 4.4.1): field is text, from, to, cc, bcc, subject,
	// body, or header, and value is the condition's raw JSON value. It may
	// read the message blob, so it returns an error for an I/O failure, which
	// the query fails on.
	Match(ctx context.Context, acct jmap.Id, obj objectdb.Object, field string, value json.RawMessage) (bool, error)

	// Snippet produces the highlighted subject and body preview for an Email
	// (RFC 8621 section 5). subjectTerms are the filter's text terms that
	// apply to the subject, bodyTerms those that apply to the body. Each
	// return is the empty string when that part does not match or the searcher
	// cannot produce it - section 5 requires null for a part the server cannot
	// determine, and SearchSnippet/get maps "" to JSON null. Unlike Match it
	// returns no error: section 5 makes snippets best-effort (a later fetch
	// MAY differ), so a read failure yields no snippet rather than failing.
	Snippet(ctx context.Context, acct jmap.Id, obj objectdb.Object, subjectTerms, bodyTerms []string) (subject, preview string)
}

// ---- SearchSnippet/get (RFC 8621 section 5.1) ----

// SearchSnippet is one section 5 snippet: the relevant, highlighted portion of
// an Email that matched a search. Unlike most types it has no id.
type SearchSnippet struct {
	// EmailId is the Email the snippet applies to.
	EmailId jmap.Id `json:"emailId"`
	// Subject is the highlighted subject when the filter text matched it, else
	// null (section 5).
	Subject *string `json:"subject"`
	// Preview is the highlighted body excerpt when the filter text matched the
	// body, else null; never larger than 255 octets (section 5).
	Preview *string `json:"preview"`
}

// searchSnippet handles SearchSnippet/get. It is a custom method (SearchSnippet
// has no id, state, or standard methods, section 5), so it is registered
// directly rather than derived from a descriptor.
type searchSnippet struct {
	db       *objectdb.DB
	searcher Searcher
	core     jmap.CoreCapabilities
}

type snippetGetArgs struct {
	AccountId jmap.Id         `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	EmailIds  []jmap.Id       `json:"emailIds"`
}

type snippetGetResponse struct {
	AccountId jmap.Id         `json:"accountId"`
	List      []SearchSnippet `json:"list"`
	// NotFound is null when every requested id was found (section 5.1).
	NotFound []jmap.Id `json:"notFound"`
}

// get implements SearchSnippet/get (RFC 8621 section 5.1): for each requested
// Email it returns the highlighted subject and body preview against the same
// filter Email/query takes, null for a part that did not match, and notFound
// for ids that do not exist.
func (h searchSnippet) get(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	var a snippetGetArgs
	if err := runtime.DecodeArgs(call.Args, &a); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := runtime.CheckAccount(call, a.AccountId, false); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	// Too many ids for one call is requestTooLarge (section 5.1); the get cap
	// is the analogous per-call object bound.
	if int64(len(a.EmailIds)) > h.core.MaxObjectsInGet {
		return runtime.Fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}
	// The filter is the Email/query filter language; an unprocessable one is
	// unsupportedFilter (section 5.1), validated exactly as Email/query does.
	if errType, desc := runtime.ValidateFilter(emailType(), emailmethods.EmailFilter{}, a.Filter); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	subjectTerms, bodyTerms := snippetTerms(a.Filter)

	resp := snippetGetResponse{AccountId: a.AccountId, List: make([]SearchSnippet, 0, len(a.EmailIds))}
	for _, id := range a.EmailIds {
		obj, err := h.db.Get(ctx, a.AccountId, TypeEmail, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		if err != nil {
			return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		subject, preview := h.searcher.Snippet(ctx, a.AccountId, obj, subjectTerms, bodyTerms)
		snip := SearchSnippet{EmailId: id}
		if subject != "" {
			snip.Subject = &subject
		}
		if preview != "" {
			snip.Preview = &preview
		}
		resp.List = append(resp.List, snip)
	}
	return runtime.Reply("SearchSnippet/get", call.CallID, resp)
}

// snippetTerms collects the filter's text terms that apply to the subject and
// to the body for section 5 highlighting: a "text" condition applies to both,
// "subject" to the subject, "body" to the body. FilterOperator nodes
// (AND/OR/NOT) are traversed; conditions that are not free-text (inMailbox,
// dates, keywords, addresses, header) contribute no highlight terms. A term
// under NOT simply never occurs in a matching Email, so it highlights nothing.
func snippetTerms(filter json.RawMessage) (subjectTerms, bodyTerms []string) {
	if len(filter) == 0 || isNullRaw(filter) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(filter, &m) != nil {
		return nil, nil
	}
	if conds, ok := m["conditions"]; ok {
		var arr []json.RawMessage
		json.Unmarshal(conds, &arr)
		for _, c := range arr {
			st, bt := snippetTerms(c)
			subjectTerms = append(subjectTerms, st...)
			bodyTerms = append(bodyTerms, bt...)
		}
		return subjectTerms, bodyTerms
	}
	for name, raw := range m {
		s, ok := decodeString(raw)
		if !ok {
			continue
		}
		switch name {
		case "text":
			subjectTerms = append(subjectTerms, s)
			bodyTerms = append(bodyTerms, s)
		case "subject":
			subjectTerms = append(subjectTerms, s)
		case "body":
			bodyTerms = append(bodyTerms, s)
		}
	}
	return subjectTerms, bodyTerms
}
