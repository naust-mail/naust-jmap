package quotas

// Protocol surface: the section 4.1 types-filtering requirements, the
// section 4.4 query language and its mandatory sorts, and the section
// 6 push requirement.

import (
	"context"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// mailQuota is a quota applying to a single type.
func mailQuota(name string, types ...string) Quota {
	return Quota{
		ResourceType: "octets",
		HardLimit:    25000,
		Scope:        "account",
		Name:         name,
		Types:        types,
	}
}

func mustUpsert(t *testing.T, svc *Service, q Quota) jmap.Id {
	t.Helper()
	id, err := svc.Upsert(context.Background(), testAccount, q)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// getIds returns the ids in a Quota/get list response.
func getIds(t *testing.T, args map[string]any) []string {
	t.Helper()
	list, _ := args["list"].([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.(map[string]any)["id"].(string))
	}
	return out
}

// RFC 9425 section 4.1: the server MUST filter out any types for which
// the client did not request the associated capability in "using".
func TestTypesFilteredPerRequest(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("mixed", "Email", "CalendarEvent", "ContactCard"))

	r := callUsing(t, ts, []string{jmap.CoreCapability, CapabilityURI, "urn:ietf:params:jmap:mail"},
		"Quota/get", `{"accountId":"Atest1","ids":null}`)
	list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v, want one record", list)
	}
	types, _ := list[0].(map[string]any)["types"].([]any)
	if len(types) != 1 || types[0] != "Email" {
		t.Errorf("types = %v, want [Email]: the client opted in to mail only", types)
	}

	// Opting in to calendars as well widens the list.
	r = callUsing(t, ts, []string{jmap.CoreCapability, CapabilityURI, "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:calendars"},
		"Quota/get", `{"accountId":"Atest1","ids":null}`)
	list, _ = argsOf(t, r, 0, "Quota/get")["list"].([]any)
	types, _ = list[0].(map[string]any)["types"].([]any)
	if len(types) != 2 {
		t.Errorf("types = %v, want Email and CalendarEvent", types)
	}
}

// RFC 9425 section 4.1: the server MUST NOT return Quota objects for
// which there are no types recognized by the client. A hidden record
// is reported the way any unfetchable id is (RFC 8620 section 5.1).
func TestUnrecognizedQuotaHidden(t *testing.T) {
	ts, svc := newTestServer(t)
	calendar := mustUpsert(t, svc, mailQuota("calendar-only", "CalendarEvent"))
	mail := mustUpsert(t, svc, mailQuota("mail-only", "Email"))

	using := []string{jmap.CoreCapability, CapabilityURI, "urn:ietf:params:jmap:mail"}
	r := callUsing(t, ts, using, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	if ids := getIds(t, argsOf(t, r, 0, "Quota/get")); len(ids) != 1 || ids[0] != string(mail) {
		t.Errorf("ids = %v, want only the mail quota %s", ids, mail)
	}

	// Naming the hidden record explicitly still must not return it.
	r = callUsing(t, ts, using, "Quota/get", `{"accountId":"Atest1","ids":["`+string(calendar)+`"]}`)
	args := argsOf(t, r, 0, "Quota/get")
	if list, _ := args["list"].([]any); len(list) != 0 {
		t.Errorf("list = %v, want empty", list)
	}
	nf, _ := args["notFound"].([]any)
	if len(nf) != 1 || nf[0] != string(calendar) {
		t.Errorf("notFound = %v, want [%s]", nf, calendar)
	}

	// It is absent from queries too.
	r = callUsing(t, ts, using, "Quota/query", `{"accountId":"Atest1"}`)
	qids, _ := argsOf(t, r, 0, "Quota/query")["ids"].([]any)
	if len(qids) != 1 || qids[0] != string(mail) {
		t.Errorf("query ids = %v, want only %s", qids, mail)
	}
}

// A quota with an empty types list can never be recognized, so it is
// invisible to every client.
func TestEmptyTypesAlwaysHidden(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("orphan"))
	r := callUsing(t, ts, append(mailUsing, "urn:ietf:params:jmap:calendars"),
		"Quota/get", `{"accountId":"Atest1","ids":null}`)
	if list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any); len(list) != 0 {
		t.Errorf("list = %v, want empty", list)
	}
}

// RFC 9425 section 4.4: name contains, scope exact, resourceType
// exact, type membership; all given conditions must match; zero
// conditions match everything.
func TestQueryFilterConditions(t *testing.T) {
	// An administrator's view, so the domain-scoped fixture below is
	// visible and the scope condition has something to match.
	ts, svc := newAdminTestServer(t)
	storage := mustUpsert(t, svc, Quota{
		ResourceType: "octets", HardLimit: 1000, Scope: "account",
		Name: "storage-personal", Types: []string{"Email"},
	})
	messages := mustUpsert(t, svc, Quota{
		ResourceType: "count", HardLimit: 50, Scope: "domain",
		Name: "message-count", Types: []string{"Email", "CalendarEvent"},
	})

	cases := []struct {
		name   string
		filter string
		want   []string
	}{
		{"no conditions matches all", `{}`, []string{string(storage), string(messages)}},
		{"name substring", `{"name":"storage"}`, []string{string(storage)}},
		{"name matches both", `{"name":"e"}`, []string{string(storage), string(messages)}},
		{"name is not exact match", `{"name":"storage-personal-extra"}`, nil},
		{"scope exact", `{"scope":"domain"}`, []string{string(messages)}},
		{"scope no partial match", `{"scope":"acc"}`, nil},
		{"resourceType exact", `{"resourceType":"octets"}`, []string{string(storage)}},
		{"type membership", `{"type":"CalendarEvent"}`, []string{string(messages)}},
		{"type matches both", `{"type":"Email"}`, []string{string(storage), string(messages)}},
		{"conditions combine with AND", `{"scope":"domain","resourceType":"count"}`, []string{string(messages)}},
		{"AND with one false condition", `{"scope":"domain","resourceType":"octets"}`, nil},
	}
	using := append(mailUsing, "urn:ietf:params:jmap:calendars")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := callUsing(t, ts, using, "Quota/query",
				`{"accountId":"Atest1","filter":`+tc.filter+`,"sort":[{"property":"name"}]}`)
			args := argsOf(t, r, 0, "Quota/query")
			got, _ := args["ids"].([]any)
			if len(got) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
			seen := map[string]bool{}
			for _, id := range got {
				seen[id.(string)] = true
			}
			for _, want := range tc.want {
				if !seen[want] {
					t.Errorf("ids = %v, missing %s", got, want)
				}
			}
		})
	}
}

// The type condition matches the client-visible list: a type the
// client did not opt in to is not a member as far as that client is
// concerned (section 4.1 filtering applies to the object the filter
// runs against).
func TestQueryTypeConditionRespectsUsing(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("mixed", "Email", "CalendarEvent"))
	r := callUsing(t, ts, []string{jmap.CoreCapability, CapabilityURI, "urn:ietf:params:jmap:mail"},
		"Quota/query", `{"accountId":"Atest1","filter":{"type":"CalendarEvent"}}`)
	if ids, _ := argsOf(t, r, 0, "Quota/query")["ids"].([]any); len(ids) != 0 {
		t.Errorf("ids = %v, want empty: CalendarEvent is not visible to this client", ids)
	}
}

func TestQueryUnsupportedFilter(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("q", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/query",
		`{"accountId":"Atest1","filter":{"hardLimit":100}}`)
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("got %s, want error", r.MethodResponses[0].Args)
	}
	args := argsOf(t, r, 0, "error")
	if args["type"] != jmap.ErrUnsupportedFilter {
		t.Errorf("error type = %v, want %s", args["type"], jmap.ErrUnsupportedFilter)
	}
}

// RFC 9425 section 4.4: name and used MUST be supported for sorting.
func TestQuerySorts(t *testing.T) {
	ts, svc := newTestServer(t)
	ctx := context.Background()
	big := mustUpsert(t, svc, mailQuota("alpha", "Email"))
	small := mustUpsert(t, svc, mailQuota("zulu", "Email"))
	if err := svc.SetUsed(ctx, testAccount, big, 900); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetUsed(ctx, testAccount, small, 100); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		sort string
		want []string
	}{
		{"name ascending", `[{"property":"name"}]`, []string{string(big), string(small)}},
		{"name descending", `[{"property":"name","isAscending":false}]`, []string{string(small), string(big)}},
		{"used ascending", `[{"property":"used"}]`, []string{string(small), string(big)}},
		{"used descending", `[{"property":"used","isAscending":false}]`, []string{string(big), string(small)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := callUsing(t, ts, mailUsing, "Quota/query",
				`{"accountId":"Atest1","sort":`+tc.sort+`}`)
			ids, _ := argsOf(t, r, 0, "Quota/query")["ids"].([]any)
			if len(ids) != 2 || ids[0] != tc.want[0] || ids[1] != tc.want[1] {
				t.Errorf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// Visibility depends on the caller, so no Quota query is change-
// calculable: /query says so and /queryChanges refuses with the RFC
// 8620 section 5.6 escape hatch.
func TestQueryChangesRefused(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("q", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/query", `{"accountId":"Atest1"}`)
	args := argsOf(t, r, 0, "Quota/query")
	if args["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v, want false", args["canCalculateChanges"])
	}
	state := args["queryState"].(string)

	r = callUsing(t, ts, mailUsing, "Quota/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"`+state+`"}`)
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("got %s, want error", r.MethodResponses[0].Args)
	}
	if got := argsOf(t, r, 0, "error")["type"]; got != jmap.ErrCannotCalculateChanges {
		t.Errorf("error type = %v, want %s", got, jmap.ErrCannotCalculateChanges)
	}
}

// RFC 9425 section 4: the type supports get/changes/query/queryChanges
// and nothing else - there is no Quota/set.
func TestQuotaSetIsUnknownMethod(t *testing.T) {
	ts, _ := newTestServer(t)
	r := callUsing(t, ts, mailUsing, "Quota/set",
		`{"accountId":"Atest1","create":{"c":{"name":"mine","hardLimit":1}}}`)
	if got := argsOf(t, r, 0, "error")["type"]; got != jmap.ErrUnknownMethod {
		t.Errorf("error type = %v, want %s", got, jmap.ErrUnknownMethod)
	}
}

// RFC 9425 section 6: servers MUST support the push mechanisms so
// clients learn of Quota state changes.
func TestPushOnStateChange(t *testing.T) {
	ts, svc := newTestServer(t)
	_ = ts
	n := notify.NewInProcess()
	svc.db.SetNotifier(n)
	sub, err := n.Subscribe(context.Background(), []jmap.Id{testAccount})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	id := mustUpsert(t, svc, mailQuota("q", "Email"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	changes, err := sub.Wait(ctx)
	if err != nil {
		t.Fatalf("no push for the created Quota: %v", err)
	}
	if _, ok := changes[testAccount][TypeName]; !ok {
		t.Errorf("push carried %v, want a %s state", changes[testAccount], TypeName)
	}

	// A usage-only counter bump pushes as well.
	if err := svc.AddUsed(context.Background(), testAccount, id, 40); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	changes, err = sub.Wait(ctx2)
	if err != nil {
		t.Fatalf("no push for the usage change: %v", err)
	}
	if _, ok := changes[testAccount][TypeName]; !ok {
		t.Errorf("push carried %v, want a %s state", changes[testAccount], TypeName)
	}
}

// RFC 9425 section 8: quotas covering a shared resource report other
// people's activity, so domain and global scope are withheld from
// general users by default.
func TestSharedScopesHiddenByDefault(t *testing.T) {
	ts, svc := newTestServer(t)
	personal := mustUpsert(t, svc, mailQuota("personal", "Email"))
	shared := make(map[string]jmap.Id, 2)
	for _, scope := range []string{"domain", "global"} {
		q := mailQuota(scope+"-wide", "Email")
		q.Scope = scope
		shared[scope] = mustUpsert(t, svc, q)
	}

	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	ids := getIds(t, argsOf(t, r, 0, "Quota/get"))
	if len(ids) != 1 || ids[0] != string(personal) {
		t.Errorf("ids = %v, want only the account-scoped quota %s", ids, personal)
	}

	// Naming a shared quota directly does not reveal it either.
	for scope, id := range shared {
		r = callUsing(t, ts, mailUsing, "Quota/get",
			`{"accountId":"Atest1","ids":["`+string(id)+`"]}`)
		args := argsOf(t, r, 0, "Quota/get")
		if list, _ := args["list"].([]any); len(list) != 0 {
			t.Errorf("%s scope: list = %v, want empty", scope, list)
		}
		nf, _ := args["notFound"].([]any)
		if len(nf) != 1 || nf[0] != string(id) {
			t.Errorf("%s scope: notFound = %v, want [%s]", scope, nf, id)
		}
	}

	// Nor does querying for them by scope.
	for _, scope := range []string{"domain", "global"} {
		r = callUsing(t, ts, mailUsing, "Quota/query",
			`{"accountId":"Atest1","filter":{"scope":"`+scope+`"}}`)
		if qids, _ := argsOf(t, r, 0, "Quota/query")["ids"].([]any); len(qids) != 0 {
			t.Errorf("%s scope: query ids = %v, want empty", scope, qids)
		}
	}
}

// An embedder that knows who its administrators are widens the rule
// through Config.ScopeVisible; the decision is per request.
func TestScopeVisibleOverride(t *testing.T) {
	ts, svc := newAdminTestServer(t)
	q := mailQuota("global-wide", "Email")
	q.Scope = "global"
	id := mustUpsert(t, svc, q)
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	ids := getIds(t, argsOf(t, r, 0, "Quota/get"))
	if len(ids) != 1 || ids[0] != string(id) {
		t.Errorf("ids = %v, want the global quota %s visible to an administrator", ids, id)
	}
}

// The predicate sees the request context, so one identity may be shown
// what another is not.
func TestScopeVisiblePerRequest(t *testing.T) {
	type adminKey struct{}
	ts, svc := newTestServerConfig(t, Config{
		TypeCapabilities: rfcTypeCapabilities(),
		ScopeVisible: func(ctx context.Context, _ jmap.Id, scope string) bool {
			return scope == "account" || ctx.Value(adminKey{}) == true
		},
	})
	q := mailQuota("global-wide", "Email")
	q.Scope = "global"
	mustUpsert(t, svc, q)

	// The HTTP path carries no admin marker, so the quota stays hidden.
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	if ids := getIds(t, argsOf(t, r, 0, "Quota/get")); len(ids) != 0 {
		t.Errorf("ids = %v, want empty for a caller with no admin marker", ids)
	}
}
