package mail

// Email/queryChanges (RFC 8620 section 5.6, RFC 8621 section 4.5)
// against the mail declarations: which queries are calculable, the
// collapsed displaced-representative case that motivates the Thread
// group companion, the receivedAt range producers, and the conformance
// checker run over the shipped declarations.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// spliceIds applies the section 5.6 client algorithm to a fully cached
// id list: splice out removed, splice in added by ascending index.
func spliceIds(cached []string, removed, added []any) []string {
	drop := map[string]bool{}
	for _, r := range removed {
		drop[r.(string)] = true
	}
	out := make([]string, 0, len(cached))
	for _, id := range cached {
		if !drop[id] {
			out = append(out, id)
		}
	}
	for _, a := range added {
		item := a.(map[string]any)
		idx := int(item["index"].(float64))
		if idx > len(out) {
			idx = len(out)
		}
		out = append(out[:idx], append([]string{item["id"].(string)}, out[idx:]...)...)
	}
	return out
}

// TestEmailQueryChangesDeclarations is the canary pinning WHICH names
// are declared: the three *InThreadHaveKeyword conditions and the two
// thread-keyword sorts read sibling records and must stay undeclared -
// a well-meaning future declaration of any of them would let
// queryChanges corrupt client caches.
func TestEmailQueryChangesDeclarations(t *testing.T) {
	hooks := emailQueryHooks(nil, nil)
	for _, name := range []string{"allInThreadHaveKeyword", "someInThreadHaveKeyword", "noneInThreadHaveKeyword"} {
		if _, declared := hooks.LocalConditions[name]; declared {
			t.Errorf("thread condition %q declared record-local; it reads sibling records", name)
		}
	}
	for _, name := range []string{"allInThreadHaveKeyword", "someInThreadHaveKeyword"} {
		if _, declared := hooks.LocalSorts[name]; declared {
			t.Errorf("thread sort %q declared record-local; it reads sibling records", name)
		}
	}
	if hooks.GroupCompanion != TypeThread {
		t.Errorf("GroupCompanion = %q, want Thread", hooks.GroupCompanion)
	}

	// The wire-level verdicts (RFC 8620 section 5.5 per-query truth).
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	putEmail(t, db, store, bodyMsg("a@remote.example", "john@example.com", "Hi", "hello", nil),
		map[string]bool{inbox: true}, nil)
	verdict := func(args string) bool {
		t.Helper()
		q := emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args))
		v, ok := q["canCalculateChanges"].(bool)
		if !ok {
			t.Fatalf("no verdict in %v", q)
		}
		return v
	}
	if !verdict(fmt.Sprintf(`"filter":{"inMailbox":%q},"sort":[{"property":"receivedAt","isAscending":false}],"collapseThreads":true`, inbox)) {
		t.Error("the mailbox-list query is not calculable")
	}
	if verdict(`"filter":{"allInThreadHaveKeyword":"$seen"}`) {
		t.Error("thread-keyword filter reported calculable")
	}
	if verdict(`"sort":[{"property":"someInThreadHaveKeyword","keyword":"$seen"}]`) {
		t.Error("thread-keyword sort reported calculable")
	}
	r := callMail(t, ts, inv("Email/queryChanges", fmt.Sprintf(
		`{"accountId":%q,"filter":{"allInThreadHaveKeyword":"$seen"},"sinceQueryState":"1"}`, testAccount), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "cannotCalculateChanges" {
		t.Error("thread-keyword queryChanges did not refuse")
	}
}

// TestEmailQueryChangesDisplacedRepresentative is the case the Thread
// companion exists for: destroying a collapsed thread's representative
// changes an UNTOUCHED sibling's standing (it becomes the new
// representative). The sibling's own record never changed, so only the
// Thread-record update in the same commit makes the diff sound.
func TestEmailQueryChangesDisplacedRepresentative(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	base := time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)
	older := putEmailAt(t, db, store,
		bodyMsg("a@remote.example", "john@example.com", "Plans", "first", map[string]string{
			"Message-ID": "<plans-1@remote.example>"}),
		map[string]bool{inbox: true}, nil, base)
	newer := putEmailAt(t, db, store,
		bodyMsg("b@remote.example", "john@example.com", "Re: Plans", "second", map[string]string{
			"Message-ID": "<plans-2@remote.example>", "In-Reply-To": "<plans-1@remote.example>"}),
		map[string]bool{inbox: true}, nil, base.Add(time.Hour))

	args := fmt.Sprintf(`"filter":{"inMailbox":%q},"sort":[{"property":"receivedAt","isAscending":false}],"collapseThreads":true`, inbox)
	query := func() map[string]any {
		t.Helper()
		return emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args))
	}
	q0 := query()
	cached := qIds(t, q0)
	if len(cached) != 1 || cached[0] != newer {
		t.Fatalf("collapsed baseline %v, want the newer message %s as representative", cached, newer)
	}
	state0 := q0["queryState"].(string)

	r := callMail(t, ts, inv("Email/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, newer), "0"))
	if destroyed, _ := methodArgs(t, r, 0, "Email/set")["destroyed"].([]any); len(destroyed) != 1 {
		t.Fatalf("destroy failed: %v", r)
	}

	r = callMail(t, ts, inv("Email/queryChanges", fmt.Sprintf(
		`{"accountId":%q,%s,"sinceQueryState":%q}`, testAccount, args, state0), "0"))
	c := methodArgs(t, r, 0, "Email/queryChanges")
	removed, _ := c["removed"].([]any)
	added, _ := c["added"].([]any)
	sawOlder := false
	for _, a := range added {
		if a.(map[string]any)["id"] == older {
			sawOlder = true
		}
	}
	if !sawOlder {
		t.Fatalf("the displaced representative %s was not re-added: removed %v added %v", older, removed, added)
	}
	next := spliceIds(cached, removed, added)
	if fresh := qIds(t, query()); strings.Join(next, ",") != strings.Join(fresh, ",") {
		t.Fatalf("splice %v vs refetch %v", next, fresh)
	}
}

// TestEmailQueryBeforeAfterWindow drives the receivedAt range producers
// (an AND of after and before) and checks the window against records on
// either side.
func TestEmailQueryBeforeAfterWindow(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	base := time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)
	mk := func(n int, at time.Time) string {
		return putEmailAt(t, db, store,
			bodyMsg("a@remote.example", "john@example.com", fmt.Sprintf("w%d", n), "b", map[string]string{
				"Message-ID": fmt.Sprintf("<w%d@remote.example>", n)}),
			map[string]bool{inbox: true}, nil, at)
	}
	mk(1, base)
	e2 := mk(2, base.Add(2*time.Hour))
	mk(3, base.Add(4*time.Hour))

	q := emailQuery(t, ts, fmt.Sprintf(
		`{"accountId":%q,"filter":{"operator":"AND","conditions":[{"after":%q},{"before":%q}]}}`,
		testAccount,
		base.Add(time.Hour).Format(time.RFC3339),
		base.Add(3*time.Hour).Format(time.RFC3339)))
	if ids := qIds(t, q); len(ids) != 1 || ids[0] != e2 {
		t.Fatalf("window = %v, want [%s]", ids, e2)
	}
}

// ---- the shipped declarations pass the conformance checker ----

func TestEmailDeclarationsConformance(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	base := time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)
	var probes []jmap.Id
	for i := 0; i < 3; i++ {
		id := putEmailAt(t, db, store,
			bodyMsg("a@remote.example", "john@example.com", fmt.Sprintf("probe %d", i), "hello world", map[string]string{
				"Message-ID": fmt.Sprintf("<probe-%d@remote.example>", i)}),
			map[string]bool{inbox: true}, map[string]bool{"$seen": i == 0}, base.Add(time.Duration(i)*time.Hour))
		probes = append(probes, jmap.Id(id))
	}
	hooks := emailQueryHooks(db, naiveSearcher{store: store})

	// One or more sample values per declared condition and sort - the
	// checker refuses unexercised declarations.
	conditions := map[string][]json.RawMessage{
		"inMailbox":          {json.RawMessage(fmt.Sprintf("%q", inbox))},
		"inMailboxOtherThan": {json.RawMessage(fmt.Sprintf("[%q]", inbox))},
		"before":             {json.RawMessage(`"2021-03-04T13:30:00Z"`)},
		"after":              {json.RawMessage(`"2021-03-04T13:30:00Z"`)},
		"minSize":            {json.RawMessage(`10`)},
		"maxSize":            {json.RawMessage(`100000`)},
		"hasKeyword":         {json.RawMessage(`"$seen"`)},
		"notKeyword":         {json.RawMessage(`"$seen"`)},
		"hasAttachment":      {json.RawMessage(`true`), json.RawMessage(`false`)},
		"text":               {json.RawMessage(`"hello"`)},
		"from":               {json.RawMessage(`"remote.example"`)},
		"to":                 {json.RawMessage(`"john"`)},
		"cc":                 {json.RawMessage(`"nobody"`)},
		"bcc":                {json.RawMessage(`"nobody"`)},
		"subject":            {json.RawMessage(`"probe"`)},
		"body":               {json.RawMessage(`"world"`)},
		"header":             {json.RawMessage(`["Subject"]`), json.RawMessage(`["Subject","probe 1"]`)},
	}
	sorts := map[string][]json.RawMessage{
		"receivedAt": {json.RawMessage(`{"property":"receivedAt"}`)},
		"sentAt":     {json.RawMessage(`{"property":"sentAt","isAscending":false}`)},
		"size":       {json.RawMessage(`{"property":"size"}`)},
		"from":       {json.RawMessage(`{"property":"from"}`)},
		"to":         {json.RawMessage(`{"property":"to"}`)},
		"subject":    {json.RawMessage(`{"property":"subject"}`)},
		"hasKeyword": {json.RawMessage(`{"property":"hasKeyword","keyword":"$seen"}`)},
	}
	err := runtime.CheckRecordLocal(t, context.Background(), db, hooks, testAccount, TypeEmail, probes,
		conditions, sorts, func() error {
			// Churn everything around the probes: a same-thread reply to
			// probe 0 (updates its Thread, not the probe), an unrelated
			// delivery, a keyword flip and a destroy on non-probes, and a
			// mailbox create.
			reply := putEmail(t, db, store,
				bodyMsg("c@remote.example", "john@example.com", "Re: probe 0", "sibling", map[string]string{
					"Message-ID": "<sibling@remote.example>", "In-Reply-To": "<probe-0@remote.example>"}),
				map[string]bool{inbox: true}, nil)
			other := putEmail(t, db, store,
				bodyMsg("d@remote.example", "john@example.com", "noise", "n", map[string]string{
					"Message-ID": "<noise@remote.example>"}),
				map[string]bool{inbox: true}, nil)
			callMail(t, ts, inv("Email/set", fmt.Sprintf(
				`{"accountId":%q,"update":{%q:{"keywords/$flagged":true}}}`, testAccount, reply), "0"))
			callMail(t, ts, inv("Email/set", fmt.Sprintf(
				`{"accountId":%q,"destroy":[%q]}`, testAccount, other), "0"))
			createMailbox(t, ts, `{"name":"Churn"}`)
			return nil
		})
	if err != nil {
		t.Fatalf("Email declarations failed conformance: %v", err)
	}
}

func TestMailboxDeclarationsConformance(t *testing.T) {
	ts, db, _ := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	work := createMailbox(t, ts, `{"name":"Work"}`)
	probes := []jmap.Id{jmap.Id(inbox), jmap.Id(work)}
	hooks := mailboxQueryHooks(db)

	conditions := map[string][]json.RawMessage{
		"parentId":     {json.RawMessage(`null`), json.RawMessage(fmt.Sprintf("%q", inbox))},
		"name":         {json.RawMessage(`"In"`), json.RawMessage(`"Work"`)},
		"role":         {json.RawMessage(`"inbox"`)},
		"hasAnyRole":   {json.RawMessage(`true`), json.RawMessage(`false`)},
		"isSubscribed": {json.RawMessage(`true`)},
	}
	err := runtime.CheckRecordLocal(t, context.Background(), db, hooks, testAccount, TypeMailbox, probes,
		conditions, nil, func() error {
			extra := createMailbox(t, ts, `{"name":"Extra"}`)
			createMailbox(t, ts, fmt.Sprintf(`{"name":"Child","parentId":%q}`, extra))
			callMail(t, ts, inv("Mailbox/set", fmt.Sprintf(
				`{"accountId":%q,"update":{%q:{"name":"Renamed"}}}`, testAccount, extra), "0"))
			return nil
		})
	if err != nil {
		t.Fatalf("Mailbox declarations failed conformance: %v", err)
	}
}

// TestThreadCommitsAlwaysTouchEmail pins the invariant that makes the
// Thread group companion free for uncollapsed queries: every commit
// that touches a Thread record also touches an Email record. queryState
// is the max of the Email and Thread states, so while this holds, a
// Thread-side change never advances an uncollapsed query's state on its
// own - the caught-up shortcut keeps answering in O(1). A Thread write
// with no Email write in its commit would silently defeat that shortcut
// (every uncollapsed Email/queryChanges behind such a commit walks the
// log to compute a provably empty diff); if a new write path needs to
// do it, the shortcut's state comparison must learn to skip the
// companion when collapseThreads is false. The check runs after each
// operation, so it pins the mutation surface at operation granularity:
// the Thread type state may never be ahead of the Email type state.
func TestThreadCommitsAlwaysTouchEmail(t *testing.T) {
	ts, db, store := emailServer(t)
	ctx := context.Background()
	stateNum := func(typeName string) int64 {
		t.Helper()
		s, err := db.TypeState(ctx, testAccount, typeName)
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("state %q is not a number: %v", s, err)
		}
		return n
	}
	check := func(op string) {
		t.Helper()
		if th, em := stateNum(TypeThread), stateNum(TypeEmail); th > em {
			t.Fatalf("after %s: Thread state %d ahead of Email state %d - a Thread-only commit exists", op, th, em)
		}
	}

	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	archive := createMailbox(t, ts, `{"name":"Archive"}`)
	check("mailbox creation")

	base := time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)
	first := putEmailAt(t, db, store,
		bodyMsg("a@remote.example", "john@example.com", "Plans", "first", map[string]string{
			"Message-ID": "<canary-1@remote.example>"}),
		map[string]bool{inbox: true}, nil, base)
	check("new-thread delivery")
	second := putEmailAt(t, db, store,
		bodyMsg("b@remote.example", "john@example.com", "Re: Plans", "second", map[string]string{
			"Message-ID": "<canary-2@remote.example>", "In-Reply-To": "<canary-1@remote.example>"}),
		map[string]bool{inbox: true}, nil, base.Add(time.Hour))
	check("join-existing-thread delivery")

	set := func(args, op string) {
		t.Helper()
		callMail(t, ts, inv("Email/set", fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args), "0"))
		check(op)
	}
	set(fmt.Sprintf(`"update":{%q:{"keywords":{"$seen":true}}}`, first), "keyword flip")
	set(fmt.Sprintf(`"update":{%q:{"mailboxIds":{%q:true}}}`, first, archive), "mailbox move")
	set(fmt.Sprintf(`"destroy":[%q]`, first), "member departure")
	set(fmt.Sprintf(`"destroy":[%q]`, second), "last-member destroy")
}
