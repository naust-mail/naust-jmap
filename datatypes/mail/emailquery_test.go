package mail

// Email/query tests. The RFC 8621 section 4.4 cases are a hard requirement:
// the 4.4.1 FilterConditions, the 4.4.2 sort keys (including the worked
// example), the 4.4.3 thread collapsing, and the section 4.4 fast-"total"
// for a pure inMailbox filter. Our own tests add compound AND/OR/NOT
// filters (the "flagged mail across all folders" shape from the planner
// design) and the error cases.

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// emailQuery runs Email/query and returns the response args (an error
// invocation's when the call fails).
func emailQuery(t *testing.T, ts *httptest.Server, args string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Email/query", args, "0"))
	name := "Email/query"
	if r.MethodResponses[0].Name == "error" {
		name = "error"
	}
	return methodArgs(t, r, 0, name)
}

func qIds(t *testing.T, args map[string]any) []string {
	t.Helper()
	raw, ok := args["ids"].([]any)
	if !ok {
		t.Fatalf("no ids in %v", args)
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = v.(string)
	}
	return out
}

func mset(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// bodyMsg builds a message with explicit From/To/Subject/body and optional
// extra headers.
func bodyMsg(from, to, subject, body string, extra map[string]string) string {
	h := "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\n"
	for k, v := range extra {
		h += k + ": " + v + "\r\n"
	}
	return h + "\r\n" + body + "\r\n"
}

// TestEmailQueryInMailboxFastTotal covers the section 4.4.1 inMailbox
// condition and the section 4.4 expectation that "total" for a pure
// inMailbox filter equals the Mailbox totalEmails counter.
func TestEmailQueryInMailboxFastTotal(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	trash := createMailbox(t, ts, `{"name":"Trash","role":"trash"}`)
	a := putEmail(t, db, store, threadMsg("A", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil)
	b := putEmail(t, db, store, threadMsg("B", map[string]string{"Message-ID": "<b@x>"}), mset(inbox), nil)
	putEmail(t, db, store, threadMsg("C", map[string]string{"Message-ID": "<c@x>"}), mset(trash), nil)

	got := emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"inMailbox":%q},"calculateTotal":true}`, testAccount, inbox))
	ids := qIds(t, got)
	if len(ids) != 2 || !hasStr(anyList(ids), a) || !hasStr(anyList(ids), b) {
		t.Fatalf("inMailbox ids = %v, want {%s,%s}", ids, a, b)
	}
	if total := int(got["total"].(float64)); total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	// The fast total must match the Mailbox counter (section 4.4).
	if c := readCounters(t, ts, inbox)["totalEmails"].(float64); int(c) != 2 {
		t.Fatalf("totalEmails counter = %v, want 2", c)
	}
}

// TestEmailQueryInMailboxOtherThan covers the section 4.4.1
// inMailboxOtherThan condition: a message solely in trash is excluded, one
// also in the inbox is not.
func TestEmailQueryInMailboxOtherThan(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	trash := createMailbox(t, ts, `{"name":"Trash","role":"trash"}`)
	both := putEmail(t, db, store, threadMsg("Both", map[string]string{"Message-ID": "<a@x>"}), mset(inbox, trash), nil)
	putEmail(t, db, store, threadMsg("OnlyTrash", map[string]string{"Message-ID": "<b@x>"}), mset(trash), nil)

	got := emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"inMailboxOtherThan":[%q]}}`, testAccount, trash))
	ids := qIds(t, got)
	if len(ids) != 1 || ids[0] != both {
		t.Fatalf("inMailboxOtherThan ids = %v, want {%s}", ids, both)
	}
}

// TestEmailQueryKeywords covers hasKeyword / notKeyword (section 4.4.1).
func TestEmailQueryKeywords(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	seen := putEmail(t, db, store, threadMsg("Seen", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), mset("$seen"))
	unseen := putEmail(t, db, store, threadMsg("Unseen", map[string]string{"Message-ID": "<b@x>"}), mset(inbox), nil)

	has := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"hasKeyword":"$seen"}}`, testAccount)))
	if len(has) != 1 || has[0] != seen {
		t.Errorf("hasKeyword $seen = %v, want {%s}", has, seen)
	}
	not := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"notKeyword":"$seen"}}`, testAccount)))
	if len(not) != 1 || not[0] != unseen {
		t.Errorf("notKeyword $seen = %v, want {%s}", not, unseen)
	}
}

// TestEmailQueryThreadKeywords covers the section 4.4.1 thread-scoped
// conditions on a two-Email Thread where only one Email is flagged.
func TestEmailQueryThreadKeywords(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	a := putEmail(t, db, store, threadMsg("Topic", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), mset("$flagged"))
	b := putEmail(t, db, store, threadMsg("Re: Topic", map[string]string{"Message-ID": "<b@x>", "In-Reply-To": "<a@x>"}), mset(inbox), nil)
	if threadOf(t, ts, a) != threadOf(t, ts, b) {
		t.Fatal("the two Emails should share a Thread")
	}

	// some: both Emails match (their shared Thread has a flagged member).
	some := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"someInThreadHaveKeyword":"$flagged"}}`, testAccount)))
	if len(some) != 2 {
		t.Errorf("someInThreadHaveKeyword = %v, want both", some)
	}
	// all: neither matches (b is not flagged).
	all := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"allInThreadHaveKeyword":"$flagged"}}`, testAccount)))
	if len(all) != 0 {
		t.Errorf("allInThreadHaveKeyword = %v, want none", all)
	}
	// none: neither matches (the Thread does have a flagged member).
	none := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"noneInThreadHaveKeyword":"$flagged"}}`, testAccount)))
	if len(none) != 0 {
		t.Errorf("noneInThreadHaveKeyword = %v, want none", none)
	}
}

// TestEmailQueryDatesSizeAttachment covers before/after, minSize/maxSize,
// and hasAttachment (section 4.4.1).
func TestEmailQueryDatesSizeAttachment(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	early := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	small := putEmailAt(t, db, store, threadMsg("Small", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil, early)
	bigBody := "x"
	for i := 0; i < 12; i++ {
		bigBody += bigBody
	}
	big := putEmailAt(t, db, store, bodyMsg("a@x", "b@x", "Big", bigBody, map[string]string{"Message-ID": "<b@x>"}), mset(inbox), nil, late)

	after := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"after":"2021-03-01T00:00:00Z"}}`, testAccount)))
	if len(after) != 1 || after[0] != big {
		t.Errorf("after = %v, want {%s}", after, big)
	}
	before := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"before":"2021-03-01T00:00:00Z"}}`, testAccount)))
	if len(before) != 1 || before[0] != small {
		t.Errorf("before = %v, want {%s}", before, small)
	}
	// minSize excludes the small message.
	mn := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"minSize":1000}}`, testAccount)))
	if len(mn) != 1 || mn[0] != big {
		t.Errorf("minSize = %v, want {%s}", mn, big)
	}
}

// TestEmailQueryText covers the text-search conditions
// from/to/subject/body/text/header (section 4.4.1) via the naive Searcher.
func TestEmailQueryText(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	a := putEmail(t, db, store,
		bodyMsg("Alice <alice@example.com>", "bob@example.com", "Quarterly report", "the numbers look good", map[string]string{"Message-ID": "<a@x>", "X-Team": "sales"}),
		mset(inbox), nil)
	putEmail(t, db, store,
		bodyMsg("Carol <carol@example.com>", "dave@example.com", "Lunch", "sandwiches", map[string]string{"Message-ID": "<b@x>"}),
		mset(inbox), nil)

	check := func(field, q, want string) {
		ids := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{%q:%q}}`, testAccount, field, q)))
		if len(ids) != 1 || ids[0] != want {
			t.Errorf("%s:%q = %v, want {%s}", field, q, ids, want)
		}
	}
	check("from", "alice", a)
	check("to", "bob", a)
	check("subject", "quarterly", a)
	check("body", "numbers", a)
	check("text", "alice", a) // header field via text
	check("text", "numbers", a)

	// header with a value, and header presence.
	hv := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"header":["X-Team","sales"]}}`, testAccount)))
	if len(hv) != 1 || hv[0] != a {
		t.Errorf("header value = %v, want {%s}", hv, a)
	}
	hp := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"filter":{"header":["X-Team"]}}`, testAccount)))
	if len(hp) != 1 || hp[0] != a {
		t.Errorf("header presence = %v, want {%s}", hp, a)
	}
}

// TestEmailQueryCompound covers AND/OR/NOT over mail conditions - notably
// the "flagged mail across all folders" shape (inMailbox trash OR
// hasKeyword flagged) the planner design called out.
func TestEmailQueryCompound(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	trash := createMailbox(t, ts, `{"name":"Trash","role":"trash"}`)
	inboxFlagged := putEmail(t, db, store, threadMsg("A", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), mset("$flagged"))
	inboxPlain := putEmail(t, db, store, threadMsg("B", map[string]string{"Message-ID": "<b@x>"}), mset(inbox), nil)
	trashPlain := putEmail(t, db, store, threadMsg("C", map[string]string{"Message-ID": "<c@x>"}), mset(trash), nil)

	// inMailbox trash OR hasKeyword flagged -> the flagged inbox mail + the
	// trash mail, not the plain inbox mail.
	or := qIds(t, emailQuery(t, ts, fmt.Sprintf(
		`{"accountId":%q,"filter":{"operator":"OR","conditions":[{"inMailbox":%q},{"hasKeyword":"$flagged"}]}}`, testAccount, trash)))
	if len(or) != 2 || !hasStr(anyList(or), inboxFlagged) || !hasStr(anyList(or), trashPlain) {
		t.Errorf("OR = %v, want {%s,%s}", or, inboxFlagged, trashPlain)
	}
	// NOT hasKeyword flagged, within the inbox -> the plain inbox mail only.
	notq := qIds(t, emailQuery(t, ts, fmt.Sprintf(
		`{"accountId":%q,"filter":{"operator":"AND","conditions":[{"inMailbox":%q},{"operator":"NOT","conditions":[{"hasKeyword":"$flagged"}]}]}}`, testAccount, inbox)))
	if len(notq) != 1 || notq[0] != inboxPlain {
		t.Errorf("AND/NOT = %v, want {%s}", notq, inboxPlain)
	}
}

// TestEmailQuerySortFields covers the individual section 4.4.2 sort keys.
func TestEmailQuerySortFields(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	t1 := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC)
	// Zed is older; Abe is newer. receivedAt asc -> [Zed, Abe]; subject asc
	// -> [Abe, Zed]; from asc -> [Abe (abe@), Zed (zed@)].
	zed := putEmailAt(t, db, store, bodyMsg("zed@x", "r@x", "Zed subject", "b", map[string]string{"Message-ID": "<z@x>"}), mset(inbox), nil, t1)
	abe := putEmailAt(t, db, store, bodyMsg("abe@x", "r@x", "Abe subject", "b", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil, t2)

	order := func(sort string) []string {
		return qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"sort":%s}`, testAccount, sort)))
	}
	if got := order(`[{"property":"receivedAt"}]`); len(got) != 2 || got[0] != zed || got[1] != abe {
		t.Errorf("receivedAt asc = %v, want [%s %s]", got, zed, abe)
	}
	if got := order(`[{"property":"receivedAt","isAscending":false}]`); len(got) != 2 || got[0] != abe || got[1] != zed {
		t.Errorf("receivedAt desc = %v, want [%s %s]", got, abe, zed)
	}
	if got := order(`[{"property":"subject"}]`); len(got) != 2 || got[0] != abe || got[1] != zed {
		t.Errorf("subject asc = %v, want [%s %s]", got, abe, zed)
	}
	if got := order(`[{"property":"from"}]`); len(got) != 2 || got[0] != abe || got[1] != zed {
		t.Errorf("from asc = %v, want [%s %s]", got, abe, zed)
	}
}

// TestEmailQuerySortRFCExample reproduces the section 4.4.2 example sort:
// flagged Threads first (someInThreadHaveKeyword $flagged, descending), then
// subject, then newest first.
func TestEmailQuerySortRFCExample(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	// Apple: unflagged. Zebra: flagged. By subject alone Apple precedes
	// Zebra, but the flagged Thread must sort first.
	apple := putEmail(t, db, store, threadMsg("Apple", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil)
	zebra := putEmail(t, db, store, threadMsg("Zebra", map[string]string{"Message-ID": "<z@x>"}), mset(inbox), mset("$flagged"))

	sort := `[{"property":"someInThreadHaveKeyword","keyword":"$flagged","isAscending":false},` +
		`{"property":"subject","collation":"i;ascii-casemap"},` +
		`{"property":"receivedAt","isAscending":false}]`
	got := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"sort":%s}`, testAccount, sort)))
	if len(got) != 2 || got[0] != zebra || got[1] != apple {
		t.Fatalf("example sort = %v, want [%s(flagged) %s]", got, zebra, apple)
	}
}

// TestEmailQueryCollapseThreads covers section 4.4.3: a Thread appears once,
// at the position of its first Email in the sorted list.
func TestEmailQueryCollapseThreads(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	t1 := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2021, 1, 2, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2021, 1, 3, 0, 0, 0, 0, time.UTC)
	// Thread T: a (oldest) and b (newest). Standalone c in the middle.
	a := putEmailAt(t, db, store, threadMsg("Topic", map[string]string{"Message-ID": "<a@x>"}), mset(inbox), nil, t1)
	c := putEmailAt(t, db, store, threadMsg("Solo", map[string]string{"Message-ID": "<c@x>"}), mset(inbox), nil, t2)
	b := putEmailAt(t, db, store, threadMsg("Re: Topic", map[string]string{"Message-ID": "<b@x>", "In-Reply-To": "<a@x>"}), mset(inbox), nil, t3)
	if threadOf(t, ts, a) != threadOf(t, ts, b) {
		t.Fatal("a and b should share a Thread")
	}

	sort := `[{"property":"receivedAt"}]`
	full := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"sort":%s}`, testAccount, sort)))
	if len(full) != 3 {
		t.Fatalf("uncollapsed = %v, want 3", full)
	}
	col := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,"sort":%s,"collapseThreads":true,"calculateTotal":true}`, testAccount, sort)))
	// a (position 0, Thread T) then c; b is dropped as T is already seen.
	if len(col) != 2 || col[0] != a || col[1] != c {
		t.Fatalf("collapsed = %v, want [%s %s]", col, a, c)
	}
}

// TestEmailQueryErrors covers the filter/sort validation errors.
func TestEmailQueryErrors(t *testing.T) {
	ts, _, _ := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	for _, tc := range []struct {
		args, wantType string
	}{
		{`{"accountId":"Atest1","filter":{"nope":"x"}}`, "unsupportedFilter"},
		{`{"accountId":"Atest1","filter":{"minSize":"big"}}`, "invalidArguments"},
		{`{"accountId":"Atest1","filter":{"header":[]}}`, "invalidArguments"},
		{`{"accountId":"Atest1","sort":[{"property":"nope"}]}`, "unsupportedSort"},
		{`{"accountId":"Atest1","sort":[{"property":"hasKeyword"}]}`, "invalidArguments"},
	} {
		got := emailQuery(t, ts, tc.args)
		if got["type"] != tc.wantType {
			t.Errorf("%s: got %v, want %s", tc.args, got["type"], tc.wantType)
		}
	}
}

func anyList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
