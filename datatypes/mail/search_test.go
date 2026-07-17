package mail

// SearchSnippet/get tests (RFC 8621 section 5). The section 5.2 example shape
// is a hard requirement: a text match highlights the subject and a body
// preview, a non-matching Email returns null for both, and an unknown id is
// notFound. Our own tests add the section 5 HTML escaping, the subject/body
// term scoping, the 255-octet preview cap, and the unsupportedFilter error.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// snippetGet runs SearchSnippet/get for the given filter and ids, returning
// the response args (or an error invocation's).
func snippetGet(t *testing.T, ts *httptest.Server, filter string, ids ...string) map[string]any {
	t.Helper()
	idsJSON, _ := json.Marshal(ids)
	args := fmt.Sprintf(`{"accountId":%q,"filter":%s,"emailIds":%s}`, testAccount, filter, idsJSON)
	r := callMail(t, ts, inv("SearchSnippet/get", args, "0"))
	name := "SearchSnippet/get"
	if r.MethodResponses[0].Name == "error" {
		name = "error"
	}
	return methodArgs(t, r, 0, name)
}

// snippetByEmail indexes the response list by emailId.
func snippetByEmail(t *testing.T, args map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, ok := args["list"].([]any)
	if !ok {
		t.Fatalf("no list in %v", args)
	}
	for _, v := range list {
		m := v.(map[string]any)
		out[m["emailId"].(string)] = m
	}
	return out
}

// TestSearchSnippetRFCExample reproduces the section 5.2 shape: text:"foo"
// highlights the subject and a body preview for a matching Email, returns null
// for both on a non-matching Email, and reports an unknown id in notFound.
func TestSearchSnippetRFCExample(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	hit := putEmail(t, db, store,
		bodyMsg("a@x", "b@x", "The Foosball competition",
			"This year the foosball competition will be held in the Stadium de France.",
			map[string]string{"Message-ID": "<a@x>"}),
		mset(inbox), nil)
	miss := putEmail(t, db, store,
		bodyMsg("a@x", "b@x", "Lunch", "sandwiches", map[string]string{"Message-ID": "<b@x>"}),
		mset(inbox), nil)

	args := snippetGet(t, ts, `{"text":"foo"}`, hit, miss, "Mbogus")
	snips := snippetByEmail(t, args)

	if got := snips[hit]["subject"]; got != "The <mark>Foo</mark>sball competition" {
		t.Errorf("hit subject = %v, want highlighted", got)
	}
	prev, _ := snips[hit]["preview"].(string)
	if !strings.Contains(prev, "<mark>foo</mark>") {
		t.Errorf("hit preview = %q, want a <mark>foo</mark>", prev)
	}
	if snips[miss]["subject"] != nil || snips[miss]["preview"] != nil {
		t.Errorf("miss should be null/null, got %v", snips[miss])
	}
	nf, _ := args["notFound"].([]any)
	if len(nf) != 1 || nf[0] != "Mbogus" {
		t.Errorf("notFound = %v, want [Mbogus]", args["notFound"])
	}
}

// TestSearchSnippetNotFoundNull covers section 5.1: notFound is null when
// every requested id was found.
func TestSearchSnippetNotFoundNull(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store, bodyMsg("a@x", "b@x", "Hello foo", "body", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil)
	args := snippetGet(t, ts, `{"text":"foo"}`, id)
	if args["notFound"] != nil {
		t.Errorf("notFound = %v, want null", args["notFound"])
	}
}

// TestSearchSnippetEscaping covers the section 5 transformation: &, <, > are
// replaced by HTML entities and the matched term is wrapped in <mark>.
func TestSearchSnippetEscaping(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store,
		bodyMsg("a@x", "b@x", "Q&A <foo> results", "body here", map[string]string{"Message-ID": "<a@x>"}),
		mset(inbox), nil)

	snips := snippetByEmail(t, snippetGet(t, ts, `{"subject":"foo"}`, id))
	want := "Q&amp;A &lt;<mark>foo</mark>&gt; results"
	if got := snips[id]["subject"]; got != want {
		t.Errorf("subject = %v, want %q", got, want)
	}
	// A subject-only term drives no body preview.
	if snips[id]["preview"] != nil {
		t.Errorf("preview = %v, want null", snips[id]["preview"])
	}
}

// TestSearchSnippetScoping covers term scoping: subject drives only the
// subject snippet, body drives only the preview (section 5).
func TestSearchSnippetScoping(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store,
		bodyMsg("a@x", "b@x", "foo subject", "bar body", map[string]string{"Message-ID": "<a@x>"}),
		mset(inbox), nil)

	subj := snippetByEmail(t, snippetGet(t, ts, `{"subject":"foo"}`, id))[id]
	if subj["subject"] == nil || subj["preview"] != nil {
		t.Errorf("subject term: got %v, want subject set / preview null", subj)
	}
	body := snippetByEmail(t, snippetGet(t, ts, `{"body":"bar"}`, id))[id]
	if body["subject"] != nil || body["preview"] == nil {
		t.Errorf("body term: got %v, want subject null / preview set", body)
	}
}

// TestSearchSnippetPreviewCap covers the section 5 rule that a preview is
// never larger than 255 octets, bracketing the match with ellipses.
func TestSearchSnippetPreviewCap(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	body := strings.Repeat("word ", 100) + "needle " + strings.Repeat("word ", 100)
	id := putEmail(t, db, store, bodyMsg("a@x", "b@x", "Long", body, map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil)

	prev, _ := snippetByEmail(t, snippetGet(t, ts, `{"body":"needle"}`, id))[id]["preview"].(string)
	if len(prev) == 0 {
		t.Fatal("no preview for a body match")
	}
	if len(prev) > maxPreviewOctets {
		t.Errorf("preview is %d octets, want <= %d", len(prev), maxPreviewOctets)
	}
	if !strings.Contains(prev, "<mark>needle</mark>") {
		t.Errorf("preview = %q, want <mark>needle</mark>", prev)
	}
	if !strings.HasPrefix(prev, "...") || !strings.HasSuffix(prev, "...") {
		t.Errorf("preview = %q, want ellipsis on both sides", prev)
	}
}

// TestSearchSnippetUnsupportedFilter covers the section 5.1 error for a filter
// the server cannot process.
func TestSearchSnippetUnsupportedFilter(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store, bodyMsg("a@x", "b@x", "Hello", "body", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil)
	if got := snippetGet(t, ts, `{"nope":"x"}`, id); got["type"] != "unsupportedFilter" {
		t.Errorf("type = %v, want unsupportedFilter", got["type"])
	}
}
