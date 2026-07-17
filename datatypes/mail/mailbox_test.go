package mail

// Mailbox tests, with fixtures modeled on the RFC 8621 section 2.6
// examples: full /get shape, updatedProperties over the change flow,
// and the section 2 invariants on /set and /query.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

const testAccount = "Atest1"

// mailServer wires a runtime with the Mailbox type registered; the
// returned db is the same instance, for tests that need to stage
// server-side changes (counter updates) the API cannot express yet.
func mailServer(t *testing.T) (*httptest.Server, *objectdb.DB) {
	t.Helper()
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := runtime.NewProcessor()
	if err := RegisterMailbox(p, db, runtime.DefaultCoreCapabilities()); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", runtime.DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(CapabilityURI, struct{}{}, DefaultAccountCapability()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db
}

func inv(name, args, callID string) jmap.Invocation {
	return jmap.Invocation{Name: name, Args: json.RawMessage(args), CallID: callID}
}

func callMail(t *testing.T, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	return callMailAs(t, ts, "john@example.com", calls...)
}

// callMailAs is callMail as an explicit user; the test server registers
// john (the uploader in blob helpers) and jane (a second member of the
// shared account).
func callMailAs(t *testing.T, ts *httptest.Server, user string, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, CapabilityURI},
		"methodCalls": calls,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api", strings.NewReader(string(body)))
	hreq.SetBasicAuth(user, "secret")
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out jmap.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func methodArgs(t *testing.T, r *jmap.Response, i int, wantName string) map[string]any {
	t.Helper()
	if i >= len(r.MethodResponses) {
		t.Fatalf("no method response %d (have %d)", i, len(r.MethodResponses))
	}
	got := r.MethodResponses[i]
	if got.Name != wantName {
		t.Fatalf("response %d is %s (%s), want %s", i, got.Name, got.Args, wantName)
	}
	var m map[string]any
	if err := json.Unmarshal(got.Args, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// createMailbox makes one mailbox from a JSON properties object and
// returns its server id, failing the test if it is rejected.
func createMailbox(t *testing.T, ts *httptest.Server, props string) string {
	t.Helper()
	r := callMail(t, ts, inv("Mailbox/set",
		fmt.Sprintf(`{"accountId":%q,"create":{"c":%s}}`, testAccount, props), "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", args)
	}
	return created["c"].(map[string]any)["id"].(string)
}

// setError runs one /set and returns the SetError object for the given
// creation id / record id from notCreated, notUpdated or notDestroyed.
func setError(t *testing.T, ts *httptest.Server, setArgs, notKey, id string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Mailbox/set", setArgs, "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	not, ok := args[notKey].(map[string]any)
	if !ok {
		t.Fatalf("no %s in %v", notKey, args)
	}
	serr, ok := not[id].(map[string]any)
	if !ok {
		t.Fatalf("%s has no entry for %s: %v", notKey, id, args)
	}
	return serr
}

func wantInvalidProp(t *testing.T, serr map[string]any, prop string) {
	t.Helper()
	if serr["type"] != "invalidProperties" {
		t.Fatalf("type %v, want invalidProperties (%v)", serr["type"], serr)
	}
	props, _ := serr["properties"].([]any)
	if len(props) != 1 || props[0] != prop {
		t.Fatalf("properties %v, want [%s]", serr["properties"], prop)
	}
}

// TestMailboxGetShape checks the full default /get object against the
// section 2.6 example shape: every section 2 property present, nulls
// literal, counters zero, myRights complete.
func TestMailboxGetShape(t *testing.T) {
	ts, _ := mailServer(t)
	id := createMailbox(t, ts, `{"name":"Inbox","role":"inbox","sortOrder":10}`)

	r := callMail(t, ts, inv("Mailbox/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, id), "0"))
	args := methodArgs(t, r, 0, "Mailbox/get")
	list := args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list: %v", args)
	}
	box := list[0].(map[string]any)

	wantKeys := []string{"id", "name", "parentId", "role", "sortOrder",
		"totalEmails", "unreadEmails", "totalThreads", "unreadThreads",
		"myRights", "isSubscribed"}
	if len(box) != len(wantKeys) {
		t.Fatalf("property count %d, want %d: %v", len(box), len(wantKeys), box)
	}
	for _, k := range wantKeys {
		if _, has := box[k]; !has {
			t.Fatalf("missing property %s: %v", k, box)
		}
	}
	if box["name"] != "Inbox" || box["role"] != "inbox" || box["sortOrder"] != float64(10) {
		t.Fatalf("stored values: %v", box)
	}
	if box["parentId"] != nil {
		t.Fatalf("parentId not null: %v", box["parentId"])
	}
	for _, c := range mailboxCounters {
		if box[c] != float64(0) {
			t.Fatalf("%s = %v, want 0", c, box[c])
		}
	}
	if box["isSubscribed"] != true {
		t.Fatalf("isSubscribed default: %v", box["isSubscribed"])
	}
	rights := box["myRights"].(map[string]any)
	wantRights := []string{"mayReadItems", "mayAddItems", "mayRemoveItems",
		"maySetSeen", "maySetKeywords", "mayCreateChild", "mayRename",
		"mayDelete", "maySubmit"}
	if len(rights) != len(wantRights) {
		t.Fatalf("myRights: %v", rights)
	}
	for _, right := range wantRights {
		if rights[right] != true {
			t.Fatalf("myRights.%s = %v", right, rights[right])
		}
	}

	// Explicit properties selection still resolves the computed
	// myRights next to stored properties.
	r = callMail(t, ts, inv("Mailbox/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["name","myRights"]}`, testAccount, id), "0"))
	box = methodArgs(t, r, 0, "Mailbox/get")["list"].([]any)[0].(map[string]any)
	if len(box) != 3 || box["name"] != "Inbox" || box["myRights"] == nil || box["id"] != id {
		t.Fatalf("selected properties: %v", box)
	}
}

func TestMailboxCreateInvariants(t *testing.T) {
	ts, _ := mailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)

	bad := []struct {
		name  string
		props string
		prop  string
	}{
		{"missing name", `{}`, "name"},
		{"empty name", `{"name":""}`, "name"},
		{"control char in name", `{"name":"a\u0007b"}`, "name"},
		{"non-NFC name", `{"name":"Cafe\u0301"}`, "name"},
		{"name too long", fmt.Sprintf(`{"name":%q}`, strings.Repeat("x", maxSizeMailboxName+1)), "name"},
		{"unregistered role", `{"name":"Boss","role":"boss"}`, "role"},
		{"role wrong case", `{"name":"In","role":"Inbox"}`, "role"},
		{"duplicate role", `{"name":"Second","role":"inbox"}`, "role"},
		{"duplicate sibling name", `{"name":"Inbox"}`, "name"},
		{"missing parent", `{"name":"Orphan","parentId":"Znope"}`, "parentId"},
		{"sortOrder over 2^31", `{"name":"Big","sortOrder":2147483648}`, "sortOrder"},
	}
	for _, tc := range bad {
		serr := setError(t, ts,
			fmt.Sprintf(`{"accountId":%q,"create":{"c":%s}}`, testAccount, tc.props),
			"notCreated", "c")
		if serr["type"] != "invalidProperties" {
			t.Fatalf("%s: %v", tc.name, serr)
		}
		if props, _ := serr["properties"].([]any); len(props) != 1 || props[0] != tc.prop {
			t.Fatalf("%s: properties %v, want [%s]", tc.name, serr["properties"], tc.prop)
		}
	}

	// The same name under a different parent is fine.
	createMailbox(t, ts, fmt.Sprintf(`{"name":"Inbox","parentId":%q}`, inbox))

	// Duplicate sibling names within one call: the staged first create
	// must be visible to the second's uniqueness check.
	r := callMail(t, ts, inv("Mailbox/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"a":{"name":"Twin"},"b":{"name":"Twin"}}}`, testAccount), "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	created, _ := args["created"].(map[string]any)
	notCreated, _ := args["notCreated"].(map[string]any)
	if len(created) != 1 || len(notCreated) != 1 {
		t.Fatalf("same-call twins: created %v notCreated %v", created, notCreated)
	}

	// Self-exclusion: updating the inbox without changing name or role
	// must not trip its own uniqueness checks.
	r = callMail(t, ts, inv("Mailbox/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"sortOrder":3}}}`, testAccount, inbox), "0"))
	args = methodArgs(t, r, 0, "Mailbox/set")
	if _, ok := args["updated"].(map[string]any)[inbox]; !ok {
		t.Fatalf("self-exclusion update: %v", args)
	}
}

func TestMailboxLoopAndDepth(t *testing.T) {
	ts, _ := mailServer(t)

	// A chain at exactly maxMailboxDepth is allowed; one more is not.
	chain := make([]string, maxMailboxDepth)
	parent := "null"
	for i := range chain {
		chain[i] = createMailbox(t, ts, fmt.Sprintf(`{"name":"A%d","parentId":%s}`, i+1, parent))
		parent = fmt.Sprintf("%q", chain[i])
	}
	serr := setError(t, ts, fmt.Sprintf(
		`{"accountId":%q,"create":{"c":{"name":"TooDeep","parentId":%q}}}`,
		testAccount, chain[maxMailboxDepth-1]), "notCreated", "c")
	wantInvalidProp(t, serr, "parentId")

	// Self-parent and a two-node loop.
	serr = setError(t, ts, fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"parentId":%q}}}`, testAccount, chain[0], chain[0]),
		"notUpdated", chain[0])
	wantInvalidProp(t, serr, "parentId")
	serr = setError(t, ts, fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"parentId":%q}}}`, testAccount, chain[0], chain[1]),
		"notUpdated", chain[0])
	wantInvalidProp(t, serr, "parentId")

	// Reparenting counts the moved subtree's height: chain[1] carries
	// eight levels below it, so two levels of new ancestors exceed the
	// limit while one level fits.
	b1 := createMailbox(t, ts, `{"name":"B1"}`)
	b2 := createMailbox(t, ts, fmt.Sprintf(`{"name":"B2","parentId":%q}`, b1))
	serr = setError(t, ts, fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"parentId":%q}}}`, testAccount, chain[1], b2),
		"notUpdated", chain[1])
	wantInvalidProp(t, serr, "parentId")
	r := callMail(t, ts, inv("Mailbox/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"parentId":%q}}}`, testAccount, chain[1], b1), "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	if _, ok := args["updated"].(map[string]any)[chain[1]]; !ok {
		t.Fatalf("reparent within depth: %v", args)
	}
}

func TestMailboxDestroy(t *testing.T) {
	ts, db := mailServer(t)
	parent := createMailbox(t, ts, `{"name":"Parent"}`)
	child := createMailbox(t, ts, fmt.Sprintf(`{"name":"Child","parentId":%q}`, parent))

	serr := setError(t, ts, fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, parent), "notDestroyed", parent)
	if serr["type"] != "mailboxHasChild" {
		t.Fatalf("destroy with child: %v", serr)
	}

	// A mailbox with emails needs onDestroyRemoveEmails; stage the
	// counter server-side the way the Email type will.
	full := createMailbox(t, ts, `{"name":"Full"}`)
	bumpCounter(t, db, full, "totalEmails", 2)
	serr = setError(t, ts, fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, full), "notDestroyed", full)
	if serr["type"] != "mailboxHasEmail" {
		t.Fatalf("destroy with emails: %v", serr)
	}

	// Child first, then the parent; the extra argument is accepted on
	// an empty mailbox.
	r := callMail(t, ts,
		inv("Mailbox/set", fmt.Sprintf(`{"accountId":%q,"destroy":[%q]}`, testAccount, child), "0"),
		inv("Mailbox/set", fmt.Sprintf(`{"accountId":%q,"destroy":[%q],"onDestroyRemoveEmails":true}`, testAccount, parent), "1"))
	for i, id := range []string{child, parent} {
		args := methodArgs(t, r, i, "Mailbox/set")
		if destroyed, _ := args["destroyed"].([]any); len(destroyed) != 1 || destroyed[0] != id {
			t.Fatalf("destroy %d: %v", i, args)
		}
	}
}

// bumpCounter stages a server-side counter change on one mailbox, the
// same shape the Email type will produce in-commit.
func bumpCounter(t *testing.T, db *objectdb.DB, id, counter string, n int64) {
	t.Helper()
	ctx := context.Background()
	_, err := db.Update(ctx, testAccount, func(u *objectdb.Update) error {
		obj, err := u.Get(TypeMailbox, jmap.Id(id))
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(n)
		obj[counter] = raw
		return u.Put(TypeMailbox, jmap.Id(id), obj)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMailboxChangesUpdatedProperties follows the section 2.6 flow:
// updatedProperties is null unless the window holds only updates that
// touched nothing but the count properties.
func TestMailboxChangesUpdatedProperties(t *testing.T) {
	ts, db := mailServer(t)

	changes := func(since string) map[string]any {
		t.Helper()
		r := callMail(t, ts, inv("Mailbox/changes",
			fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, since), "0"))
		return methodArgs(t, r, 0, "Mailbox/changes")
	}
	state := func() string {
		t.Helper()
		r := callMail(t, ts, inv("Mailbox/get",
			fmt.Sprintf(`{"accountId":%q,"ids":[]}`, testAccount), "0"))
		return methodArgs(t, r, 0, "Mailbox/get")["state"].(string)
	}

	s0 := state()
	id := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)

	// A window with a create: null.
	args := changes(s0)
	if v, has := args["updatedProperties"]; !has || v != nil {
		t.Fatalf("create window: %v", args)
	}

	// A rename touches a non-count property: null.
	s1 := state()
	r := callMail(t, ts, inv("Mailbox/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"name":"In"}}}`, testAccount, id), "0"))
	methodArgs(t, r, 0, "Mailbox/set")
	args = changes(s1)
	if upd, _ := args["updated"].([]any); len(upd) != 1 {
		t.Fatalf("rename window updated: %v", args)
	}
	if args["updatedProperties"] != nil {
		t.Fatalf("rename window: %v", args["updatedProperties"])
	}

	// A counter-only window names exactly the changed counters.
	s2 := state()
	bumpCounter(t, db, id, "totalEmails", 1)
	bumpCounter(t, db, id, "unreadThreads", 1)
	args = changes(s2)
	props, ok := args["updatedProperties"].([]any)
	if !ok || len(props) != 2 || props[0] != "totalEmails" || props[1] != "unreadThreads" {
		t.Fatalf("counter window: %v", args["updatedProperties"])
	}

	// Counters plus a rename in one window: null again.
	s3 := state()
	bumpCounter(t, db, id, "totalEmails", 2)
	r = callMail(t, ts, inv("Mailbox/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"name":"Inbox"}}}`, testAccount, id), "0"))
	methodArgs(t, r, 0, "Mailbox/set")
	if args = changes(s3); args["updatedProperties"] != nil {
		t.Fatalf("mixed window: %v", args["updatedProperties"])
	}
}

func TestMailboxQuery(t *testing.T) {
	ts, _ := mailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox","sortOrder":1}`)
	sent := createMailbox(t, ts, `{"name":"Sent","role":"sent","sortOrder":2,"isSubscribed":false}`)
	plans := createMailbox(t, ts, `{"name":"Plans","sortOrder":3}`)
	monday := createMailbox(t, ts, fmt.Sprintf(`{"name":"Monday","parentId":%q}`, plans))

	query := func(qargs string) []any {
		t.Helper()
		r := callMail(t, ts, inv("Mailbox/query",
			fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, qargs), "0"))
		return methodArgs(t, r, 0, "Mailbox/query")["ids"].([]any)
	}

	cases := []struct {
		name  string
		qargs string
		want  []string
	}{
		{"parentId null", `"filter":{"parentId":null},"sort":[{"property":"sortOrder"}]`, []string{inbox, sent, plans}},
		{"parentId exact", fmt.Sprintf(`"filter":{"parentId":%q}`, plans), []string{monday}},
		{"name contains", `"filter":{"name":"n"},"sort":[{"property":"name"}]`, []string{inbox, monday, plans, sent}},
		{"name contains narrow", `"filter":{"name":"onda"}`, []string{monday}},
		{"role exact", `"filter":{"role":"sent"}`, []string{sent}},
		{"role null", `"filter":{"role":null},"sort":[{"property":"name"}]`, []string{monday, plans}},
		{"hasAnyRole true", `"filter":{"hasAnyRole":true},"sort":[{"property":"sortOrder"}]`, []string{inbox, sent}},
		{"hasAnyRole false", `"filter":{"hasAnyRole":false},"sort":[{"property":"name"}]`, []string{monday, plans}},
		{"isSubscribed false", `"filter":{"isSubscribed":false}`, []string{sent}},
		{"AND composition", `"filter":{"operator":"AND","conditions":[{"name":"n"},{"hasAnyRole":true}]},"sort":[{"property":"sortOrder"}]`, []string{inbox, sent}},
	}
	for _, tc := range cases {
		got := query(tc.qargs)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	}

	// Unknown condition and wrong-typed values are the standard method
	// errors; extras must be booleans.
	for _, tc := range []struct {
		name    string
		qargs   string
		errType string
	}{
		{"unknown condition", `"filter":{"color":"red"}`, "unsupportedFilter"},
		{"name null", `"filter":{"name":null}`, "invalidArguments"},
		{"hasAnyRole null", `"filter":{"hasAnyRole":null}`, "invalidArguments"},
		{"parentId not an id", `"filter":{"parentId":7}`, "invalidArguments"},
		{"sortAsTree not a boolean", `"sortAsTree":"yes"`, "invalidArguments"},
	} {
		r := callMail(t, ts, inv("Mailbox/query",
			fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, tc.qargs), "0"))
		if methodArgs(t, r, 0, "error")["type"] != tc.errType {
			t.Fatalf("%s: want %s, got %v", tc.name, tc.errType, r.MethodResponses[0].Args)
		}
	}
}

func TestMailboxQueryTrees(t *testing.T) {
	ts, _ := mailServer(t)
	// Two roots with children; names chosen so a flat name sort
	// interleaves the trees while the tree sort may not.
	autumn := createMailbox(t, ts, `{"name":"Autumn"}`)
	zebra := createMailbox(t, ts, fmt.Sprintf(`{"name":"Zebra","parentId":%q}`, autumn))
	spring := createMailbox(t, ts, `{"name":"Spring"}`)
	berry := createMailbox(t, ts, fmt.Sprintf(`{"name":"Berry","parentId":%q}`, spring))

	query := func(qargs string) []any {
		t.Helper()
		r := callMail(t, ts, inv("Mailbox/query",
			fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, qargs), "0"))
		return methodArgs(t, r, 0, "Mailbox/query")["ids"].([]any)
	}
	wantIds := func(name string, got []any, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", name, got, want)
			}
		}
	}

	// Flat name sort interleaves; sortAsTree walks each root's subtree
	// before the next root.
	wantIds("flat", query(`"sort":[{"property":"name"}]`), autumn, berry, spring, zebra)
	wantIds("sortAsTree", query(`"sort":[{"property":"name"}],"sortAsTree":true`), autumn, zebra, spring, berry)

	// filterAsTree drops a matching child whose parent did not match:
	// "r" matches Zebra, Berry and Berry's parent Spring, but not
	// Zebra's parent Autumn.
	wantIds("filter flat", query(`"filter":{"name":"r"},"sort":[{"property":"name"}]`), berry, spring, zebra)
	wantIds("filterAsTree", query(`"filter":{"name":"r"},"sort":[{"property":"name"}],"filterAsTree":true`), berry, spring)

	// Both together: the surviving subtree in tree order.
	wantIds("both", query(`"filter":{"name":"r"},"sort":[{"property":"name"}],"sortAsTree":true,"filterAsTree":true`), spring, berry)

	// sortAsTree places a matched child by the full tree even when its
	// parent is not in the result set.
	wantIds("subset", query(`"filter":{"parentId":null},"sort":[{"property":"name"}],"sortAsTree":true`), autumn, spring)
	wantIds("children only", query(`"filter":{"operator":"NOT","conditions":[{"parentId":null}]},"sort":[{"property":"name"}],"sortAsTree":true`), zebra, berry)
}
